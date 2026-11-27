package scenario_test

import (
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

// newConflictWorld is newWorld after two forced failovers of generation 6, one on each
// side of a partition: the KV of dc_a names dc_a, that of dc_b names dc_b, and the
// arbiter is left at generation 5.
func newConflictWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	w.voters[0].setState(t, kv.State{Generation: 6, ActiveDatacenter: dcA, OpID: "fa", Forced: true, PatroniTimeline: 6})
	w.voters[1].setState(t, kv.State{Generation: 6, ActiveDatacenter: dcB, OpID: "fb", Forced: true, PatroniTimeline: 7, NeedsRecovery: []string{dcC}})

	return w
}

func resolveRequest(winner string, isolation bool) *dcctlv1.ResolveConflictRequest {
	return &dcctlv1.ResolveConflictRequest{
		Winner: "datacenters/" + winner, Confirmation: &dcctlv1.Confirmation{Isolation: isolation},
		RequestId: "5d3a8f61-2b4c-4e97-a0d8-9c1e7b3f2a46",
	}
}

func (w *world) resolvePlan(winner string, isolation bool) operation.Plan {
	w.t.Helper()

	p, err := w.resolve.Plan(resolveRequest(winner, isolation), initiator, preflight.Result{Status: w.status()})
	require.NoError(w.t, err)

	return p
}

func TestResolve_succeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newConflictWorld(t)
		p := w.resolvePlan(dcB, true)

		assert.Equal(t, int64(6), p.PreviousGeneration)
		assert.Equal(t, int64(7), p.NewGeneration)
		assert.Equal(t, dcctlv1.Operation_QUORUM, p.Mode)

		op, err := w.startPlan(p)
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_RESOLVE_CONFLICT, op.GetType())
		assert.True(t, op.GetConfirmation().GetIsolation())
		assert.Empty(t, w.infra.allCalls(), "resolve changes neither apps nor Patroni")

		want := kv.State{Generation: 7, ActiveDatacenter: dcB, OpID: operationID(op), PatroniTimeline: 7, NeedsRecovery: []string{dcA, dcC}}
		for _, s := range w.voterStates() {
			assert.Equal(t, want, withoutTime(s), "the winner's payload, the other side in need of recovery")
		}

		view, err := w.coord.Read(t.Context())
		require.NoError(t, err)
		assert.Empty(t, view.Conflict)
		assert.True(t, coord.ActiveKnown(view))
		assert.Equal(t, dcB, w.voters[0].lease(t).Holder)
	})
}

// D4: resolve always needs the isolation of the other side; it must name a side of the
// conflict, and there must be one.
func TestResolve_preconditions(t *testing.T) {
	tests := []struct {
		name      string
		world     func(t *testing.T) *world
		winner    string
		isolation bool
		reason    apierr.Reason
	}{
		{name: "isolation not confirmed", world: newConflictWorld, winner: dcB, reason: apierr.ReasonConfirmationRequired},
		{name: "winner not a side", world: newConflictWorld, winner: dcC, isolation: true, reason: apierr.ReasonPreflightFailed},
		{name: "no conflict", world: newWorld, winner: dcA, isolation: true, reason: apierr.ReasonPreflightFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := tt.world(t)
				before := w.voterStates()

				// The plan is built on a conflict; the preflight of the start decides.
				p := newConflictWorld(t).resolvePlan(dcB, tt.isolation)
				p.Target = tt.winner
				p.Preflight.Params.Datacenter = tt.winner

				_, err := w.startPlan(p)
				require.Equal(t, tt.reason, apierr.ReasonOf(err), "%v", err)
				assert.Equal(t, before, w.voterStates())
			})
		})
	}
}

// A conflict of another generation than the one planned on is not resolved: the
// engine refuses it under the lock.
func TestResolve_refusedOnChangedConflict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newConflictWorld(t)
		p := w.resolvePlan(dcB, true)

		w.voters[0].setState(t, kv.State{Generation: 7, ActiveDatacenter: dcA, OpID: "fa2", Forced: true})
		w.voters[1].setState(t, kv.State{Generation: 7, ActiveDatacenter: dcB, OpID: "fb2", Forced: true})

		_, err := w.startPlan(p)
		require.ErrorIs(t, err, operation.ErrStateChanged)
	})
}

// interruptResolve starts the resolve p on e with the writes of the voters unknown
// lost, and returns its ID once it is interrupted.
func (w *world) interruptResolve(e *operation.Engine, p operation.Plan, unknown ...int) string {
	w.t.Helper()

	for _, i := range unknown {
		w.voters[i].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })
	}

	first, err := e.Start(w.t.Context(), p)
	require.NoError(w.t, err)

	synctest.Wait()
	require.Equal(w.t, dcctlv1.Operation_INTERRUPTED, w.op().GetState(), w.op().GetError().GetDetail())

	for _, v := range w.voters {
		v.set(func(v *fakeVoter) { v.casErr = nil })
	}

	return operationID(first)
}

// A resolve whose write did not reach a majority is interrupted, and the resume
// finishes the record the first run wrote on the other side.
func TestResolve_resumeFinishesRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newConflictWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		id := w.interruptResolve(e, w.resolvePlan(dcB, true), 1, 2)
		require.Equal(t, int64(7), w.voters[0].state(t).Generation, "the other side is written first")

		op := w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true}})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		w.assertActive(7, dcB)
	})
}

// A resolve resumed with the isolation does not take the forced mode on resume: its step
// records the mode it writes in.
func TestResolve_resumeKeepsMode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newConflictWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		id := w.interruptResolve(e, w.resolvePlan(dcB, true), 1, 2)

		first, err := e.Resume(t.Context(), operation.ResumeRequest{
			Operation: id, Confirmations: check.Confirmations{Isolation: true}, ToForced: true, Initiator: initiator,
		})
		require.NoError(t, err)
		assert.Equal(t, dcctlv1.Operation_QUORUM, first.GetMode())

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op().GetState(), w.op().GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_QUORUM, w.op().GetMode())
	})
}

// An abandon never reverts the record of a resolve: one no voter holds is abandoned and
// the conflict stays; one some voter holds refuses the abandon, and the resume finishes
// it, also above a record of a higher generation than the conflict.
func TestResolve_abandon(t *testing.T) {
	t.Run("not written", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newConflictWorld(t)
			before := w.voterStates()
			e := w.engine()
			stop := start(t, e)

			defer stop()

			synctest.Wait()

			id := w.interruptResolve(e, w.resolvePlan(dcB, true), 0, 2)

			op, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: id, Initiator: initiator})
			require.NoError(t, err)
			assert.Equal(t, dcctlv1.Operation_ABANDONED, op.GetState())
			assert.Equal(t, before, w.voterStates(), "nothing is written back")
		})
	})

	// The arbiter took the record with the answer lost and stopped answering: it may
	// bring the record back, so the abandon waits for it, and the resume finishes.
	t.Run("holder does not answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newConflictWorld(t)
			e := w.engine()
			stop := start(t, e)

			defer stop()

			synctest.Wait()

			id := w.interruptResolve(e, w.resolvePlan(dcB, true), 0, 2)

			s := w.voters[1].state(t)
			s.Generation, s.OpID, s.NeedsRecovery = 7, id, []string{dcA, dcC}
			w.voters[2].setState(t, s)
			w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })

			_, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: id, Initiator: initiator})
			require.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "%v", err)
			assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

			view, err := w.coord.Read(t.Context())
			require.NoError(t, err)
			assert.Contains(t, preflight.GenerationDetermined(w.op(), view).Detail, "abandon once every voter answers")

			w.voters[2].set(func(v *fakeVoter) { v.getErr = nil })

			op := w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true}})
			require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

			view, err = w.coord.Read(t.Context())
			require.NoError(t, err)
			require.NotNil(t, view.Effective)
			assert.Equal(t, coord.RecordKey{Generation: 7, ActiveDatacenter: dcB}, coord.KeyOf(view.Effective.State),
				"the forced record the arbiter brought back is the resolve's own")
		})
	})

	tests := []struct {
		name  string
		setup func(w *world)
		gen   int64
	}{
		{name: "written in part", gen: 7},
		{name: "above an uncommitted record", gen: 8, setup: func(w *world) {
			w.voters[2].setState(w.t, kv.State{Generation: 7, ActiveDatacenter: dcC, OpID: "stray", PatroniTimeline: 5})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newConflictWorld(t)
				if tt.setup != nil {
					tt.setup(w)
				}

				e := w.engine()
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				p := w.resolvePlan(dcB, true)
				require.Equal(t, tt.gen, p.NewGeneration)

				id := w.interruptResolve(e, p, 1, 2)
				held := w.voterStates()

				_, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: id, Initiator: initiator})
				require.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "%v", err)

				view, err := w.coord.Read(t.Context())
				require.NoError(t, err)
				assert.Contains(t, preflight.GenerationDetermined(w.op(), view).Detail, "resume the operation")
				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())
				assert.Equal(t, held, w.voterStates(), "nothing is reverted")

				op := w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true}})
				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
				w.assertActive(tt.gen, dcB)
			})
		})
	}
}

// A resolve crashed after its record was committed only takes the lease on resume.
func TestResolve_crashResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newConflictWorld(t)

		var saved map[string][]byte

		w.voters[2].set(func(v *fakeVoter) {
			v.beforeCAS = func(*fakeVoter, []byte) {
				if saved == nil {
					saved = w.rawOps()
				}
			}
		})

		op, err := w.startPlan(w.resolvePlan(dcB, true))
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		w.setRawOps(saved)

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		before := w.voterStates()
		op = w.resume(e, operationID(op), operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true}})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, before, w.voterStates(), "written once")
	})
}

// A datacenter in need of recovery only on the side that loses stays so, and the winner
// never is, also when the other side marked it.
func TestResolve_keepsRecoveryOfEverySide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newConflictWorld(t)
		w.voters[0].setState(t, kv.State{
			Generation: 6, ActiveDatacenter: dcA, OpID: "fa", Forced: true, PatroniTimeline: 6,
			NeedsRecovery: []string{dcB, dcC},
		})
		w.voters[1].setState(t, kv.State{Generation: 6, ActiveDatacenter: dcB, OpID: "fb", Forced: true, PatroniTimeline: 7})

		op, err := w.startPlan(w.resolvePlan(dcB, true))
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, s := range w.voterStates() {
			assert.Equal(t, []string{dcA, dcC}, s.NeedsRecovery)
		}
	})
}

func TestResolvePlan_cannotPlan(t *testing.T) {
	w := newConflictWorld(t)

	_, err := w.resolve.Plan(resolveRequest(dcC, true), initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)

	_, err = w.resolve.Plan(resolveRequest("dc_x", true), initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)
}
