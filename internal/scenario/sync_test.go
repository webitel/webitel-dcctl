package scenario_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// syncCalls are the calls of apps sync with dc_a active: dc_b and dc_c stop, each on
// its own, then dc_a starts.
var syncCalls = []string{
	"stop dc_b from 3", "stop dc_b from 2", "stop dc_b from 0",
	"stop dc_c from 3", "stop dc_c from 2", "stop dc_c from 0",
	"start dc_a to 1", "start dc_a to 2", "start dc_a to all",
}

// newDriftedWorld is newWorld after a hand install: stage 1 runs in dc_b, and stage 2 of
// dc_a is down.
func newDriftedWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	w.infra.do(func(dcs map[string]*dcState) {
		dcs[dcB].running[1] = true
		dcs[dcA].running[2] = false
	})

	return w
}

func syncRequest() *dcctlv1.SyncAppsRequest {
	return &dcctlv1.SyncAppsRequest{RequestId: "0b8d4e2a-3c71-4f5e-9d26-7a1b5c8e4f03"}
}

func (w *world) syncPlan() operation.Plan {
	w.t.Helper()

	p, err := w.sync.Plan(syncRequest(), initiator, preflight.Result{Status: w.status()})
	require.NoError(w.t, err)

	return p
}

func TestSyncApps_succeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDriftedWorld(t)
		before := w.voterStates()

		op, err := w.startPlan(w.syncPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_SYNC_APPS, op.GetType())
		assert.Equal(t, "datacenters/"+dcA, op.GetTarget())
		assert.Equal(t, int64(5), op.GetPreviousGeneration())
		assert.Zero(t, op.GetNewGeneration())
		assert.Equal(t, syncCalls, w.infra.allCalls())

		assert.True(t, allRunning(w.infra.dc(dcA)))
		assert.True(t, noneRunning(w.infra.dc(dcB)))
		assert.True(t, noneRunning(w.infra.dc(dcC)))

		assert.Equal(t, before, w.voterStates(), "apps sync writes no state")
		assert.True(t, w.lockFree())
	})
}

// A stage that does not stop in time is a warning, and the lower stages of that
// datacenter still stop while its Nomad answers.
func TestSyncApps_keepsStoppingWhileNomadAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDriftedWorld(t)
		w.infra.fail("stop dc_b from 3", fmt.Errorf("stage 3: %w", apps.ErrTimeout))

		op, err := w.startPlan(w.syncPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, syncCalls, w.infra.allCalls(), "every stage of dc_b is still stopped")
		assert.Contains(t, op.GetSteps()[0].GetWarnings(), "the stop of the apps of the datacenter is not confirmed")
		assert.True(t, noneRunning(w.infra.dc(dcB)))
	})
}

// A Nomad that does not list the jobs is a warning, and the rest of that datacenter is
// skipped; the other datacenters go on.
func TestSyncApps_skipsSilentNomad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDriftedWorld(t)
		w.infra.fail("stop dc_b from 3", fmt.Errorf("%w: connection refused", apps.ErrNotListed))

		op, err := w.startPlan(w.syncPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, syncCalls[3:], w.infra.allCalls()[1:])
		assert.Contains(t, op.GetSteps()[0].GetWarnings(), "the Nomad of the datacenter does not answer, its apps are not stopped")

		for _, i := range []int{1, 2} {
			assert.Equal(t, dcctlv1.Operation_Step_SKIPPED, op.GetSteps()[i].GetState())
		}
	})
}

// A stage of the active datacenter that does not start, as with a job of an invalid
// stage, ends the operation FAILED and rolls nothing back.
func TestSyncApps_startFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDriftedWorld(t)
		w.infra.fail("start dc_a to 2", fmt.Errorf("job webitel-x: %w", nomad.ErrInvalidStage))

		op, err := w.startPlan(w.syncPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "job webitel-x")
		assert.Equal(t, syncCalls[:8], w.infra.allCalls(), "nothing is rolled back")
		assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_APPS_START, 2).GetState())
	})
}

// The apps start only while the voters hold the decided record that names dc_a: one
// written meanwhile interrupts the operation before dc_a starts anything.
func TestSyncApps_neverStartsWhereNotActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDriftedWorld(t)
		w.infra.hook("stop dc_c from 0", func(context.Context) error {
			w.setVoters(kv.State{Generation: 6, ActiveDatacenter: dcB, OpID: "other"})

			return nil
		})

		op, err := w.startPlan(w.syncPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Equal(t, syncCalls[:6], w.infra.allCalls())
		assert.Contains(t, op.GetError().GetDetail(), "generation 6 of dc_b")
	})
}

// The record the sync was planned on must still be there under the lock.
func TestSyncApps_refusedOnChangedState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newDriftedWorld(t)
		p := w.syncPlan()
		w.setVoters(kv.State{Generation: 6, ActiveDatacenter: dcA, OpID: "other"})

		_, err := w.startPlan(p)
		require.ErrorIs(t, err, operation.ErrStateChanged)
		assert.Empty(t, w.infra.allCalls())
	})
}

// Preconditions refuse the sync before anything runs.
func TestSyncApps_preconditions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
	}{
		{name: "primary not in the active datacenter", setup: func(w *world) {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RoleStandbyLeader })
		}},
		{name: "cluster not read", setup: func(w *world) {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].unread = true })
		}},
		{name: "uncommitted record above", setup: func(w *world) {
			w.voters[2].setState(w.t, kv.State{Generation: 6, ActiveDatacenter: dcB, OpID: "x"})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newDriftedWorld(t)
				p := w.syncPlan()
				tt.setup(w)

				_, err := w.startPlan(p)
				require.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "%v", err)
				assert.Empty(t, w.infra.allCalls())
			})
		})
	}
}

// Apps sync crashed at each phase resumes from the step it was in and finishes.
func TestSyncApps_crashResumes(t *testing.T) {
	for _, key := range []string{"stop dc_b from 2", "stop dc_c from 0", "start dc_a to 1", "start dc_a to all"} {
		t.Run(key, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newDriftedWorld(t)

				e, stop, id := w.crashStandby(key, w.syncPlan())

				defer stop()

				op := w.resume(e, id, operation.ResumeRequest{})

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
				assert.True(t, allRunning(w.infra.dc(dcA)))
				assert.True(t, noneRunning(w.infra.dc(dcB)))
				assert.True(t, noneRunning(w.infra.dc(dcC)))
			})
		})
	}
}

func TestSyncApps_stretch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcB].running[3] = true })

		op, err := w.startPlan(w.syncPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, syncCalls, w.infra.allCalls())
		assert.True(t, noneRunning(w.infra.dc(dcB)))
	})
}

func TestSyncAppsPlan_cannotPlan(t *testing.T) {
	w := newWorld(t)
	st := w.status()
	st.View = coord.View{Majority: 2}

	_, err := w.sync.Plan(syncRequest(), initiator, preflight.Result{Status: st})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)
}

func TestSyncAppsResume_refusesOtherSteps(t *testing.T) {
	w := newWorld(t)
	p := w.syncPlan()

	op := &dcctlv1.Operation{
		Name: "operations/x", Type: dcctlv1.Operation_SYNC_APPS, Target: "datacenters/" + dcA, PreviousGeneration: 5,
		Steps: p.Pending(),
	}

	steps, err := w.sync.Resume(t.Context(), op)
	require.NoError(t, err)
	require.Len(t, steps, len(p.Steps))

	op.Steps[0].Datacenter = "datacenters/" + dcA

	_, err = w.sync.Resume(t.Context(), op)
	require.Error(t, err, "a stop of the active datacenter is no step of apps sync")

	op.Steps[0].Datacenter, op.Type = "datacenters/"+dcB, dcctlv1.Operation_SWITCH

	_, err = w.sync.Resume(t.Context(), op)
	require.Error(t, err)
}
