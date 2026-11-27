package scenario_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
)

// markRecoveredChecks decides mark-recovered of dc as its set does: a majority of voters
// answers, the record is not split, dc needs recovery, and every member of dc follows
// the active datacenter.
func (f fakePreflight) markRecoveredChecks(ctx context.Context, dc string) ([]check.Check, error) {
	view, err := f.view(ctx)
	if err != nil {
		return nil, err
	}

	block := func(id check.ID, pass bool) check.Check {
		c := check.Check{ID: id, Result: check.Pass, Effect: check.Block}
		if !pass {
			c.Result = check.Fail
		}

		return c
	}

	base := coord.Base(view, false)
	needs := base != nil && slices.Contains(base.State.NeedsRecovery, dc)
	recovered := base != nil && fakePatroni{f.w}.follows(dc, base.State.ActiveDatacenter) == ""

	return []check.Check{
		block(check.CoordQuorum, view.Quorum), payloadSettled(view),
		block(check.CoordDCNeedsRecovery, needs), block(check.PatroniDCRecovered, recovered),
	}, nil
}

// payloadSettled is coord.payload_settled on view.
func payloadSettled(view coord.View) check.Check {
	c := check.Check{ID: check.CoordPayloadSettled, Result: check.Pass, Effect: check.Block}

	if base := coord.Base(view, false); base == nil || !base.Settled {
		c.Result = check.Fail
	}

	return c
}

// needingRecovery is generation 5 of dc_a with dc_c in needs_recovery.
var needingRecovery = kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 5, NeedsRecovery: []string{dcC}}

// recoveredState is needingRecovery once mark-recovered wrote it at revision.
func recoveredState(revision int64) kv.State {
	return kv.State{Generation: 5, Revision: revision, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 5}
}

func markRecoveredOf(dc string) *dcctlv1.MarkDatacenterRecoveredRequest {
	return &dcctlv1.MarkDatacenterRecoveredRequest{Name: "datacenters/" + dc, RequestId: "1f2e3d4c-5b6a-4978-8a9b-0c1d2e3f4a5b"}
}

// newRecoveringWorld is newWorld with dc_c in needs_recovery, following dc_a already.
func newRecoveringWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	w.setVoters(needingRecovery)

	return w
}

func (w *world) markPlan() operation.Plan {
	w.t.Helper()

	v, err := w.preflight().Run(w.t.Context(), preflight.Request{Action: preflight.MarkRecovered, Params: preflight.Params{Datacenter: dcC}})
	require.NoError(w.t, err)

	p, err := w.mark.Plan(markRecoveredOf(dcC), initiator, preflight.Result{Verdict: v, Status: w.status()})
	require.NoError(w.t, err)

	return p
}

func TestMarkRecovered_succeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)

		op, err := w.startPlan(w.markPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_MARK_RECOVERED, op.GetType())
		assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode())
		assert.Zero(t, op.GetNewGeneration())

		for _, st := range op.GetSteps() {
			assert.Equal(t, succeeded, st.GetState(), "%s: %s", st.GetType(), st.GetDetail())
		}

		assert.Equal(t, []string{"recovered dc_c", "recovered dc_c"}, w.infra.allCalls(), "read before the write too")

		for _, s := range w.voterStates() {
			assert.Equal(t, recoveredState(1), withoutTime(s))
		}

		assert.True(t, w.lockFree())
	})
}

// While a member of dc_c does not stream, mark-recovered is refused: by the preflight,
// and by the step when it stops streaming after the start. Nothing is written.
func TestMarkRecovered_refusedWhileNotStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].lagging = true })

		_, err := w.startPlan(w.markPlan())
		require.Error(t, err)
		assert.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err))

		for _, s := range w.voterStates() {
			assert.Equal(t, needingRecovery, withoutTime(s))
		}
	})

	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)
		p := w.markPlan()

		w.infra.hook("recovered dc_c", func(context.Context) error {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].timeline = 4 })

			return nil
		})

		op, err := w.startPlan(p)
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		follow := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FOLLOW, 0)
		assert.Equal(t, failed, follow.GetState())
		assert.Contains(t, follow.GetDetail(), "does not follow dc_a in full")
		assert.Equal(t, pending, stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0).GetState())

		for _, s := range w.voterStates() {
			assert.Equal(t, needingRecovery, withoutTime(s))
		}
	})
}

// split leaves generation 5 of dc_a at revision 1 with dc_c in needs_recovery on the KV
// of dc_a and without it on the arbiter, while the KV of dc_b does not answer.
func (w *world) split() {
	x, y := needingRecovery, recoveredState(1)
	x.Revision = 1

	w.voters[0].setState(w.t, x)
	w.voters[2].setState(w.t, y)
	w.voters[1].set(func(v *fakeVoter) { v.getErr = unreachable() })
}

// A payload split is refused by name, by the preflight and by the write step, and
// nothing is written.
func TestMarkRecovered_refusesPayloadSplit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)
		w.split()

		_, err := w.startPlan(w.markPlan())
		require.Error(t, err)
		assert.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err))
		assert.Contains(t, err.Error(), string(check.CoordPayloadSettled))
	})

	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)
		p := w.markPlan()

		w.infra.hook("recovered dc_c", func(context.Context) error {
			w.split()

			return nil
		})

		op, err := w.startPlan(p)
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		write := stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0)
		assert.Equal(t, failed, write.GetState())
		assert.Contains(t, write.GetDetail(), "payload split")

		assert.Contains(t, w.voters[0].state(t).NeedsRecovery, dcC, "nothing is written")
		assert.Empty(t, w.voters[2].state(t).NeedsRecovery)
	})
}

// A forced record counts on any voter, but mark-recovered needs a majority: a write that
// reaches fewer interrupts the operation, and the resume finishes it.
func TestMarkRecovered_needsMajorityOnForcedRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)

		forced := needingRecovery
		forced.Forced = true

		w.voters[0].set(func(v *fakeVoter) { v.getErr = unreachable() })
		w.voters[1].setState(t, forced)
		w.voters[2].setState(t, kv.State{Generation: 4, ActiveDatacenter: dcB, OpID: "older", PatroniTimeline: 4})

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		w.voters[2].set(func(v *fakeVoter) { v.casErr = coord.ErrVersionMismatch })

		first, err := e.Start(t.Context(), w.markPlan())
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		write := stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0)
		assert.Equal(t, failed, write.GetState())
		assert.Contains(t, write.GetDetail(), "a majority of 2 is needed")

		w.voters[2].set(func(v *fakeVoter) { v.casErr = nil })

		op = w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, v := range w.voters[1:] {
			s := withoutTime(v.state(t))
			assert.Empty(t, s.NeedsRecovery, v.name)
			assert.Equal(t, int64(5), s.Generation, v.name)
			assert.True(t, s.Forced, v.name)
		}
	})
}

// A write with an unknown outcome interrupts the operation; the resume finishes it.
func TestMarkRecovered_resumeFinishesUndeterminedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		for _, v := range w.voters[1:] {
			v.set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })
		}

		first, err := e.Start(t.Context(), w.markPlan())
		require.NoError(t, err)

		synctest.Wait()

		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		for _, v := range w.voters[1:] {
			v.set(func(v *fakeVoter) { v.casErr = nil })
		}

		op := w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, s := range w.voterStates() {
			assert.Empty(t, s.NeedsRecovery)
			assert.Equal(t, int64(5), s.Generation)
		}
	})
}

// A mark-recovered crashed at each step resumes. Crashed after its write reached the
// voters but before the step was recorded, the resume finds the write and writes
// nothing more.
func TestMarkRecovered_crashResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)

		e, stop, id := w.crashStandby("recovered dc_c", w.markPlan())

		defer stop()

		op := w.resume(e, id, operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, s := range w.voterStates() {
			assert.Equal(t, recoveredState(1), withoutTime(s))
		}
	})

	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)

		var saved map[string][]byte

		w.voters[2].set(func(v *fakeVoter) {
			v.beforeCAS = func(*fakeVoter, []byte) {
				if saved == nil {
					saved = w.rawOps()
				}
			}
		})

		op, err := w.startPlan(w.markPlan())
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		require.NotNil(t, saved)

		w.setRawOps(saved)

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		op = w.resume(e, operationID(op), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_Step_SKIPPED, stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0).GetState())

		for _, s := range w.voterStates() {
			assert.Equal(t, recoveredState(1), withoutTime(s), "written once")
		}
	})
}

// A mark-recovered interrupted at its write is resumed while dc_c no longer follows dc_a:
// the write reads dc_c again and refuses, needs_recovery stays.
func TestMarkRecovered_resumeReadsAgainBeforeTheWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newRecoveringWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		for _, v := range w.voters {
			v.set(func(v *fakeVoter) { v.casErr = errors.New("cas refused") })
		}

		first, err := e.Start(t.Context(), w.markPlan())
		require.NoError(t, err)

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		for _, v := range w.voters {
			v.set(func(v *fakeVoter) { v.casErr = nil })
		}

		w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].lagging = true })

		op := w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0).GetDetail(), "does not follow dc_a in full")

		for _, s := range w.voterStates() {
			assert.Contains(t, s.NeedsRecovery, dcC, "dc_c does not follow, needs_recovery stays")
		}
	})
}

func TestMarkRecoveredPlan_cannotPlan(t *testing.T) {
	w := newRecoveringWorld(t)

	_, err := w.mark.Plan(markRecoveredOf(dcA), initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)

	w.stretch = true

	_, err = w.mark.Plan(markRecoveredOf(dcC), initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)
}
