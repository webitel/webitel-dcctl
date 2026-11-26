package scenario_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// interruptAtGeneration runs the switch on e until its generation write is left
// undetermined: dc_a takes it, dc_b leaves the outcome unknown and, with lost, the
// arbiter does not answer. It returns the ID of the operation.
func (w *world) interruptAtGeneration(e *operation.Engine, lost bool) string {
	w.t.Helper()

	w.infra.hook(noPrimary, func(context.Context) error {
		w.voters[1].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })

		if lost {
			w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })
		}

		return nil
	})

	synctest.Wait()

	first, err := e.Start(w.t.Context(), w.plan())
	require.NoError(w.t, err)

	synctest.Wait()

	op := w.op()
	require.Equal(w.t, dcctlv1.Operation_INTERRUPTED, op.GetState(), op.GetError().GetDetail())
	require.Equal(w.t, failed, stepOf(w.t, op, dcctlv1.Operation_Step_GENERATION, 0).GetState())
	require.Equal(w.t, dcB, w.voters[0].state(w.t).ActiveDatacenter)

	return operationID(first)
}

func (w *world) resume(e *operation.Engine, id string, req operation.ResumeRequest) *dcctlv1.Operation {
	w.t.Helper()

	req.Operation, req.Initiator = id, initiator

	_, err := e.Resume(w.t.Context(), req)
	require.NoError(w.t, err)

	synctest.Wait()

	return w.op()
}

// The resume runs while the arbiter is still lost: the KVs of dc_a and dc_b are a
// majority, the base record is undetermined on dc_b, and the record dc_a holds is
// finished from it.
func TestSwitch_resumeUndeterminedWithVoterStillLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtGeneration(e, true)

		w.voters[1].set(func(v *fakeVoter) { v.casErr = nil })

		op := w.resume(e, id, operation.ResumeRequest{})

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, forward, w.infra.allCalls())

		for _, v := range w.voters[:2] {
			s := v.state(t)
			assert.Equal(t, int64(6), s.Generation, v.name)
			assert.Equal(t, dcB, s.ActiveDatacenter, v.name)
		}
	})
}

// The first resume finds the record of the switch on dc_a only and cannot write it on a
// majority: it is interrupted, never rolled back, so the second one, with every voter
// back, finishes the switch.
func TestSwitch_resumeUndeterminedTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtGeneration(e, true)

		op := w.resume(e, id, operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0).GetState())

		for _, st := range op.GetSteps() {
			assert.False(t, kv.StepRolledBack(st), "%s stage %d: %s", st.GetType(), st.GetStage(), st.GetDetail())
		}

		assert.Equal(t, upTo(t, noPrimary), w.infra.allCalls())

		w.voters[1].set(func(v *fakeVoter) { v.casErr = nil })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = nil })

		op = w.resume(e, id, operation.ResumeRequest{})

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		w.assertActive(6, dcB)
	})
}

// newTwoVoterWorld is newWorld with the KVs of dc_a and dc_b as the only voters: two
// datacenters without an arbiter.
func newTwoVoterWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	base := kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 5}
	w.voters = []*fakeVoter{newVoter(t, dcA, coord.KindKV, base), newVoter(t, dcB, coord.KindKV, base)}
	w.useVoters(w.voters[0], w.voters[1])

	return w
}

// Two datacenters without an arbiter: the record of the switch reached dc_a only and the
// KV of dc_b is lost. The resume with the isolation confirmed writes it forced.
func TestSwitch_resumeToForcedWithHeldRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newTwoVoterWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtGeneration(e, false)

		w.voters[1].set(func(v *fakeVoter) { v.casErr, v.getErr = nil, unreachable() })

		op := w.resume(e, id, operation.ResumeRequest{ToForced: true, Confirmations: check.Confirmations{Isolation: true}})

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_FORCED, op.GetMode())

		s := w.voters[0].state(t)
		assert.Equal(t, int64(6), s.Generation)
		assert.Equal(t, dcB, s.ActiveDatacenter)
		assert.True(t, s.Forced)
	})
}

// The instance stops after dc_b was promoted and its timeline written, while the
// promotion step still ran. The resume finds dc_b primary and the timeline written.
func TestSwitch_resumePromoteBAlreadyPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		var saved map[string][]byte

		w.infra.hook("promote dc_b", func(ctx context.Context) error {
			w.infra.do(func(dcs map[string]*dcState) {
				dcs[dcB].role, dcs[dcB].timeline, dcs[dcB].upstream = patroni.RolePrimary, 6, ""
			})

			view, err := w.coord.Read(ctx)
			require.NoError(t, err)

			_, err = w.coord.SetTimeline(ctx, coord.KeyOf(view.Effective.State), 6)
			require.NoError(t, err)

			saved = w.rawOps()

			<-ctx.Done()

			return context.Cause(ctx)
		})

		e := w.engine()
		stop := start(t, e)

		synctest.Wait()

		first, err := e.Start(t.Context(), w.plan())
		require.NoError(t, err)

		synctest.Wait()
		stop()

		w.setRawOps(saved)

		e = w.engine()
		stop = start(t, e)

		defer stop()

		synctest.Wait()

		op := w.resume(e, operationID(first), operation.ResumeRequest{})

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, int64(6), w.infra.dc(dcB).timeline, "dc_b is not promoted twice")

		id := operationID(op)
		for _, s := range w.voterStates() {
			assert.Equal(t, kv.State{Generation: 6, Revision: 1, ActiveDatacenter: dcB, OpID: id, PatroniTimeline: 6},
				withoutTime(s), "the timeline is written once")
		}
	})
}

// While the switch is interrupted at the generation, the operator promotes dc_a by hand.
// The resume writes the generation, but does not promote dc_b next to a primary.
func TestSwitch_resumeRefusesPromoteNextToPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtGeneration(e, true)

		w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RolePrimary })
		w.voters[1].set(func(v *fakeVoter) { v.casErr = nil })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = nil })

		op := w.resume(e, id, operation.ResumeRequest{})

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "leader dc_a1 of dc_a is primary")
		assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetState())
		assert.Equal(t, upTo(t, "no_primary dc_a"), w.infra.allCalls())
		assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcB).role)
	})
}

// interruptAtPromote runs the switch on e until the check before the promotion, where
// dc_a stops being read, after the generation is written. It returns the ID of the
// operation.
func (w *world) interruptAtPromote(e *operation.Engine) string {
	w.t.Helper()

	w.infra.hook("no_primary dc_a", func(context.Context) error {
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].unread = true })

		return nil
	})

	synctest.Wait()

	first, err := e.Start(w.t.Context(), w.plan())
	require.NoError(w.t, err)

	synctest.Wait()

	op := w.op()
	require.Equal(w.t, dcctlv1.Operation_INTERRUPTED, op.GetState(), op.GetError().GetDetail())
	require.Equal(w.t, failed, stepOf(w.t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetState())
	w.assertActive(6, dcB)

	return operationID(first)
}

// isolated is a resume with the isolation of the old active datacenter confirmed, as
// the API asks it.
var isolated = operation.ResumeRequest{ToForced: true, Confirmations: check.Confirmations{Isolation: true}}

// dc_a is lost after the generation: without the isolation confirmed every resume is
// interrupted before the promotion.
func TestSwitch_resumePromoteSourceNotRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtPromote(e)

		op := w.resume(e, id, operation.ResumeRequest{})

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "dc_a not read")
		assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcB).role, "dc_b is not promoted")
		assert.NotContains(t, w.infra.allCalls(), "promote dc_b")
	})
}

// With the isolation of dc_a confirmed the resume promotes dc_b, with a warning.
func TestSwitch_resumePromoteSourceNotReadIsolated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtPromote(e)

		op := w.resume(e, id, isolated)

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, []string{"dc_a is not read, it is not known whether it is primary"},
			stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings())
		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)

		id2 := operationID(op)
		for _, s := range w.voterStates() {
			assert.Equal(t, kv.State{Generation: 6, Revision: 1, ActiveDatacenter: dcB, OpID: id2, PatroniTimeline: 6},
				withoutTime(s), "the generation written before the resume stays as it is")
		}
	})
}

// The isolation confirmed does not cover a dc_a read as primary.
func TestSwitch_resumePromoteIsolatedSourcePrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		id := w.interruptAtPromote(e)

		w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].unread, dcs[dcA].role = false, patroni.RolePrimary })

		op := w.resume(e, id, isolated)

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "leader dc_a1 of dc_a is primary")
		assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcB).role, "dc_b is not promoted")
	})
}
