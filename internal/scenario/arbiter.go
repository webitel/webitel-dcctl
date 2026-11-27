package scenario

import (
	"context"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// InitializeArbiter gives a new or replaced arbiter, one with an empty store, the
// dcctl/state every KV holds and the current lease, see
// [coord.Coordinator.InitializeArbiter]. An arbiter that holds another state is never
// written over: it is cleared by hand first. It is an action of warm_standby, the only
// profile with an arbiter.
type InitializeArbiter struct {
	core
}

// NewInitializeArbiter returns the arbiter initialization acting through d.
func NewInitializeArbiter(d Deps) (*InitializeArbiter, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &InitializeArbiter{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict, so
// that a refusal can show the plan too. It fails with [ErrCannotPlan] in stretch or
// without an arbiter. The record the KVs agree on, when the state tells the active
// datacenter, is the one the operation starts from.
func (a *InitializeArbiter) Plan(req *dcctlv1.InitializeArbiterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (
	operation.Plan, error,
) {
	st := pre.Status

	switch {
	case st.Profile != config.ProfileWarmStandby:
		return operation.Plan{}, fmt.Errorf("%w: %s has no arbiter", ErrCannotPlan, st.Profile)
	case !slices.ContainsFunc(st.View.Voters, func(vs coord.VoterState) bool { return vs.Kind == coord.KindArbiter }):
		return operation.Plan{}, fmt.Errorf("%w: no arbiter is configured", ErrCannotPlan)
	}

	p := operation.Plan{
		Type:      dcctlv1.Operation_INITIALIZE_ARBITER,
		Request:   req,
		RequestID: req.GetRequestId(),
		Initiator: initiator,
		Preflight: preflight.Request{Action: preflight.ArbiterInit},
		Steps:     a.steps(),
	}

	if base := coord.Base(st.View, false); base != nil {
		p.PreviousGeneration, p.PreviousActive = base.State.Generation, base.State.ActiveDatacenter
	}

	return p, nil
}

// Resume rebuilds the steps of the interrupted arbiter initialization op, see
// [operation.Resumer].
func (a *InitializeArbiter) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	if op.GetType() != dcctlv1.Operation_INITIALIZE_ARBITER {
		return nil, fmt.Errorf("initialize_arbiter %s: operation of type %s is not of type %s", op.GetName(), op.GetType(),
			dcctlv1.Operation_INITIALIZE_ARBITER)
	}

	return a.steps(), nil
}

func (a *InitializeArbiter) steps() []operation.Step {
	return []operation.Step{{Type: dcctlv1.Operation_Step_GENERATION, Run: a.initialize()}}
}

// initialize writes the state and then the lease to the arbiter while the lock is held.
// An arbiter that holds both already, as after an earlier run, needs nothing.
func (a *InitializeArbiter) initialize() operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		if err := x.Guard(ctx); err != nil {
			return err
		}

		res, err := a.d.Coordinator.InitializeArbiter(ctx)
		if err != nil {
			return fmt.Errorf("arbiter is not initialized: %w", err)
		}

		s, l := res.State, res.Lease
		detail := fmt.Sprintf("arbiter has generation %d of %s at revision %d and a lease of %s at generation %d, counter %d",
			s.Generation, s.ActiveDatacenter, s.Revision, l.Holder, l.Generation, l.Counter)

		if !res.StateCreated && !res.LeaseCreated && !x.Resumed() {
			x.Skip("the arbiter was initialized already: " + detail)

			return nil
		}

		x.Progress(detail)

		return nil
	}
}
