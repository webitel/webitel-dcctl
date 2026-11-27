package scenario_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
)

// newReplacedArbiterWorld is newWorld with an arbiter whose store is empty, as after it
// was replaced; the KVs hold leases of dc_a.
func newReplacedArbiterWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	w.voters[2].set(func(v *fakeVoter) { clear(v.entries) })

	for i, v := range w.voters[:2] {
		b, err := json.Marshal(kv.Lease{Holder: dcA, Generation: 5, Counter: int64(20 + i)})
		require.NoError(t, err)

		v.set(func(v *fakeVoter) { v.store(coord.KeyLease, b) })
	}

	return w
}

func (w *world) arbiterPlan() operation.Plan {
	w.t.Helper()

	p, err := w.arbiter.Plan(&dcctlv1.InitializeArbiterRequest{RequestId: "3e9c1a7b-6d24-4f8e-b5a0-2c7d9e1f4b68"}, initiator,
		preflight.Result{Status: w.status()})
	require.NoError(w.t, err)

	return p
}

func TestInitializeArbiter_succeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newReplacedArbiterWorld(t)

		op, err := w.startPlan(w.arbiterPlan())
		require.NoError(t, err)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_INITIALIZE_ARBITER, op.GetType())
		assert.Equal(t, int64(5), op.GetPreviousGeneration())
		assert.Contains(t, op.GetSteps()[0].GetDetail(), "generation 5 of dc_a")

		assert.Equal(t, w.voters[0].state(t), w.voters[2].state(t), "the arbiter holds the record of the KVs")
		assert.Equal(t, &kv.Lease{Holder: dcA, Generation: 5, Counter: 21}, w.voters[2].lease(t))

		view, err := w.coord.Read(t.Context())
		require.NoError(t, err)
		assert.Len(t, view.Effective.Voters, 3, "the arbiter votes")
	})
}

// The arbiter is filled once: an initialized one, or KVs that disagree, refuse it.
func TestInitializeArbiter_preconditions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
	}{
		{name: "arbiter initialized", setup: func(w *world) {
			w.voters[2].setState(w.t, kv.State{Generation: 3, ActiveDatacenter: dcB, OpID: "old"})
		}},
		{name: "payload split", setup: func(w *world) {
			w.voters[0].setState(w.t, kv.State{Generation: 5, Revision: 1, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 6})
			w.voters[1].setState(w.t, kv.State{Generation: 5, Revision: 1, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 7})
		}},
		{name: "kv does not answer", setup: func(w *world) { w.voters[1].set(func(v *fakeVoter) { v.getErr = unreachable() }) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newReplacedArbiterWorld(t)
				p := w.arbiterPlan()
				tt.setup(w)

				_, err := w.startPlan(p)
				require.Error(t, err)
				assert.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "%v", err)
				assert.Nil(t, w.voters[2].lease(t))
			})
		})
	}
}

// An initialization whose state write had an unknown outcome is interrupted; the resume
// reads the arbiter and writes only what it lacks, the lease.
func TestInitializeArbiter_resumeAfterUnknownOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newReplacedArbiterWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		w.voters[2].set(func(v *fakeVoter) { v.createErr = fmt.Errorf("%w: connection reset", coord.ErrOutcomeUnknown) })

		first, err := e.Start(t.Context(), w.arbiterPlan())
		require.NoError(t, err)

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState(), w.op().GetError().GetDetail())

		// The state had landed; the answer was lost.
		state := w.voters[0].state(t)
		w.voters[2].setState(t, state)
		w.voters[2].set(func(v *fakeVoter) { v.createErr = nil })

		op := w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, state, w.voters[2].state(t))
		require.NotNil(t, w.voters[2].lease(t))
	})
}

func TestInitializeArbiterPlan_cannotPlan(t *testing.T) {
	w := newStretchWorld(t)

	_, err := w.arbiter.Plan(&dcctlv1.InitializeArbiterRequest{}, initiator, preflight.Result{Status: w.status()})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)

	w = newWorld(t)
	st := w.status()
	st.View.Voters = st.View.Voters[:2]

	_, err = w.arbiter.Plan(&dcctlv1.InitializeArbiterRequest{}, initiator, preflight.Result{Status: st})
	require.ErrorIs(t, err, scenario.ErrCannotPlan)
}
