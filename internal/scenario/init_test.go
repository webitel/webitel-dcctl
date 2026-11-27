package scenario_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// newBlankWorld is newWorld before the installation is initialized: no voter holds
// dcctl/state.
func newBlankWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	w.clearState()

	return w
}

func (w *world) clearState() {
	for _, v := range w.voters {
		v.set(func(v *fakeVoter) { delete(v.entries, coord.KeyState) })
	}
}

func initRequest(dc string) *dcctlv1.InitializeDatacentersRequest {
	return &dcctlv1.InitializeDatacentersRequest{ActiveDatacenter: "datacenters/" + dc, RequestId: "7c2b9e14-58a3-4d6f-b1e0-3f9a2c4d6e85"}
}

func (w *world) initPlan(dc string) operation.Plan {
	w.t.Helper()

	p, err := w.init.Plan(initRequest(dc), initiator, preflight.Result{Status: w.status()})
	require.NoError(w.t, err)

	return p
}

// stored returns what voter v holds, nil when it holds no dcctl/state.
func (v *fakeVoter) stored(t *testing.T) *kv.State {
	t.Helper()

	v.mu.Lock()
	_, ok := v.entries[coord.KeyState]
	v.mu.Unlock()

	if !ok {
		return nil
	}

	s := withoutTime(v.state(t))

	return &s
}

func TestInitialize_createsState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newBlankWorld(t)

		op, err := w.startPlan(w.initPlan(dcA))
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_INITIALIZE, op.GetType())
		assert.Equal(t, int64(1), op.GetNewGeneration())
		assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode())
		assert.Equal(t, []string{"no_primary dc_b,dc_c"}, w.infra.allCalls(), "no other cluster is primary")

		want := kv.State{Generation: 1, ActiveDatacenter: dcA, OpID: operationID(op), PatroniTimeline: 5}

		for _, v := range w.voters[:2] {
			assert.Equal(t, &want, v.stored(t), v.name)
		}

		assert.Nil(t, w.voters[2].stored(t), "the arbiter gets the state only from its own initialization")

		for _, v := range w.voters[:2] {
			l := v.lease(t)
			require.NotNil(t, l, v.name)
			assert.Equal(t, dcA, l.Holder)
		}

		assert.Nil(t, w.voters[2].lease(t))
		assert.True(t, w.lockFree())
	})
}

// The step reads where the primary runs again: it is not created while the cluster of
// another datacenter is primary or not read, nor when the active one is not primary.
func TestInitialize_refusesPrimaryElsewhere(t *testing.T) {
	tests := []struct {
		name  string
		setup func(dcs map[string]*dcState)
	}{
		{name: "another primary", setup: func(dcs map[string]*dcState) { dcs[dcC].role = patroni.RolePrimary }},
		{name: "cluster not read", setup: func(dcs map[string]*dcState) { dcs[dcB].unread = true }},
		{name: "active not primary", setup: func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RoleStandbyLeader }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newBlankWorld(t)

				// The preflight passed; the infrastructure changes right before the step reads it.
				w.infra.hook("no_primary dc_b,dc_c", func(context.Context) error {
					w.infra.do(tt.setup)

					return nil
				})

				op, err := w.startPlan(w.initPlan(dcA))
				require.NoError(t, err)

				require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
				assert.Contains(t, op.GetError().GetDetail(), "dc_a is not recorded active")

				for _, v := range w.voters {
					assert.Nil(t, v.stored(t), "%s: nothing is created", v.name)
				}
			})
		})
	}
}

// R15: the state is created only where no voter has it and every KV answers, and only
// with the primary in the datacenter made active.
func TestInitialize_preconditions(t *testing.T) {
	tests := []struct {
		name  string
		dc    string
		setup func(w *world)
	}{
		{name: "state present", dc: dcA, setup: func(w *world) {
			w.voters[1].setState(w.t, kv.State{Generation: 3, ActiveDatacenter: dcA, OpID: "x"})
		}},
		{name: "kv does not answer", dc: dcA, setup: func(w *world) { w.voters[1].set(func(v *fakeVoter) { v.getErr = unreachable() }) }},
		{name: "primary elsewhere", dc: dcB},
		{name: "cluster not read", dc: dcA, setup: func(w *world) {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].unread = true })
		}},
		{name: "arbiter does not answer", dc: dcA, setup: func(w *world) {
			w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })
		}},
		{name: "arbiter holds a stale state", dc: dcA, setup: func(w *world) {
			w.voters[2].setState(w.t, kv.State{Generation: 7, ActiveDatacenter: dcB, OpID: "old", Forced: true})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newBlankWorld(t)
				if tt.setup != nil {
					tt.setup(w)
				}

				_, err := w.startPlan(w.initPlan(tt.dc))
				require.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "%v", err)
				assert.Empty(t, w.infra.allCalls())
			})
		})
	}
}

// An arbiter that stops answering between the preflight and the write may hold a state
// from before: nothing is created.
func TestInitialize_refusesArbiterGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newBlankWorld(t)
		w.infra.hook("no_primary dc_b,dc_c", func(context.Context) error {
			w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })

			return nil
		})

		op, err := w.startPlan(w.initPlan(dcA))
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "clear its data_dir")

		for _, v := range w.voters[:2] {
			assert.Nil(t, v.stored(t), "%s: nothing is created", v.name)
		}
	})
}

// An initialization whose write did not reach a majority is interrupted; the resume
// finishes the record the first run wrote.
func TestInitialize_resumeFinishesRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newBlankWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		w.voters[1].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })

		first, err := e.Start(t.Context(), w.initPlan(dcA))
		require.NoError(t, err)

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState(), w.op().GetError().GetDetail())

		w.voters[1].set(func(v *fakeVoter) { v.casErr = nil })
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].timeline = 9 })

		op := w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		want := kv.State{Generation: 1, ActiveDatacenter: dcA, OpID: operationID(first), PatroniTimeline: 5}

		for _, v := range w.voters[:2] {
			assert.Equal(t, &want, v.stored(t), "%s: the record of the first run, one payload", v.name)
		}
	})
}

// An abandon never reverts the record of an initialization, as there is none to go
// back to: one no voter holds is abandoned; one held without a majority refuses the
// abandon, telling the resume or the delete by hand, with the index of every key.
func TestInitialize_abandon(t *testing.T) {
	tests := []struct {
		name    string
		unknown []int
		refused bool
	}{
		{name: "not written", unknown: []int{0, 1}},
		{name: "written in part", unknown: []int{1}, refused: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newBlankWorld(t)
				e := w.engine()
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				for _, i := range tt.unknown {
					w.voters[i].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })
				}

				first, err := e.Start(t.Context(), w.initPlan(dcA))
				require.NoError(t, err)

				synctest.Wait()
				require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState(), w.op().GetError().GetDetail())

				for _, v := range w.voters {
					v.set(func(v *fakeVoter) { v.casErr = nil })
				}

				held := w.voters[0].stored(t)

				op, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: operationID(first), Initiator: initiator})
				if !tt.refused {
					require.NoError(t, err)
					assert.Equal(t, dcctlv1.Operation_ABANDONED, op.GetState())

					return
				}

				require.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "%v", err)
				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())
				assert.Equal(t, held, w.voters[0].stored(t), "nothing is deleted")

				view, err := w.coord.Read(t.Context())
				require.NoError(t, err)

				c := preflight.GenerationDetermined(w.op(), view)
				assert.Contains(t, c.Detail, "consul kv delete -cas")
				assert.Contains(t, c.Detail, "resume the operation")
				assert.Contains(t, c.Facts["modify_index"], dcA)
			})
		})
	}
}

// An initialization crashed during its write resumes; one crashed after the record was
// committed only takes the lease.
func TestInitialize_crashResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newBlankWorld(t)

		e, stop, id := w.crashStandby("no_primary dc_b,dc_c", w.initPlan(dcA))

		defer stop()

		op := w.resume(e, id, operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, v := range w.voters[:2] {
			require.NotNil(t, v.stored(t))
			assert.Equal(t, int64(1), v.stored(t).Generation)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		w := newBlankWorld(t)

		var saved map[string][]byte

		w.voters[1].set(func(v *fakeVoter) {
			v.beforeCAS = func(*fakeVoter, []byte) {
				if saved == nil {
					saved = w.rawOps()
				}
			}
		})

		op, err := w.startPlan(w.initPlan(dcA))
		require.NoError(t, err)
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		w.setRawOps(saved)

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		before := w.voters[0].stored(t)
		op = w.resume(e, operationID(op), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, before, w.voters[0].stored(t), "written once")
	})
}

func TestInitialize_stretch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.clearState()

		op, err := w.startPlan(w.initPlan(dcA))
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		s := w.voters[0].stored(t)
		require.NotNil(t, s)
		assert.Equal(t, dcA, s.ActiveDatacenter)
		assert.Positive(t, s.PatroniTimeline)
	})

	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.clearState()

		_, err := w.startPlan(w.initPlan(dcB))
		require.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "the leader runs in dc_a")
		assert.Nil(t, w.voters[0].stored(t))
	})
}

func TestInitializePlan(t *testing.T) {
	w := newBlankWorld(t)

	p := w.initPlan(dcB)
	assert.Equal(t, dcB, p.Target)
	assert.Zero(t, p.PreviousGeneration)
	assert.Empty(t, p.PreviousActive)

	_, err := w.init.Plan(initRequest("dc_x"), initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)

	w.stretch = true

	_, err = w.init.Plan(initRequest(dcA), initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)

	_, err = scenario.NewInitialize(scenario.Deps{}, config.ProfileWarmStandby)
	require.Error(t, err)
}
