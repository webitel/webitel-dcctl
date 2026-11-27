package scenario

import (
	"context"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// Failover is the failover of warm_standby from the active datacenter A, lost or
// isolated, to the standby B: the new generation names B active and A in need of
// recovery, the apps of A are stopped and its cluster demoted when they answer, B is
// promoted, its apps start, the other standby clusters follow B unless they diverged
// from it, and the result is verified.
//
// Nothing rolls back once the generation is written: a step after it that cannot go on
// interrupts the operation. The promotion of B is the point of no return.
type Failover struct {
	core
}

// NewFailover returns the failover of warm_standby acting through d.
func NewFailover(d Deps) (*Failover, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &Failover{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict,
// so that a refusal can show the plan too. The failover starts from the record
// [coord.Base] gives a failover, the highest undetermined one included, and writes the
// generation in the mode the voters of that state allow. It fails with
// [ErrCannotPlan] when that state gives no record to start from or no mode to write
// in, when the target is the datacenter it starts from or is not configured, for a
// profile other than warm_standby, and for a request with max_discard_bytes that the
// verdict of pre does not ask for: it bounds only the WAL the demotion of an old active
// datacenter read as primary discards.
func (f *Failover) Plan(req *dcctlv1.FailoverDatacenterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	st := pre.Status

	if st.Profile != config.ProfileWarmStandby {
		return operation.Plan{}, fmt.Errorf("%w: failover of %s is not a failover of %s", ErrCannotPlan, st.Profile, config.ProfileWarmStandby)
	}

	c := req.GetConfirmation()
	if c.GetMaxDiscardBytes() > 0 && !discardAsked(pre.Verdict) {
		return operation.Plan{}, fmt.Errorf("%w: max_discard_bytes confirms only the WAL the demotion of the old primary discards, "+
			"and the preflight does not read it as primary", ErrCannotPlan)
	}

	base := coord.Base(st.View, true)
	if base == nil {
		return operation.Plan{}, fmt.Errorf("%w: the datacenter to fail over from is not known", ErrCannotPlan)
	}

	r, err := f.planRoute(st, base.State.ActiveDatacenter, datacenterID(req.GetName()))
	if err != nil {
		return operation.Plan{}, err
	}

	r.failover = true

	next := kv.State{Generation: base.State.Generation + 1, ActiveDatacenter: r.b}

	wm, err := coord.AdvanceMode(st.View, base.State, next)
	if err != nil {
		return operation.Plan{}, fmt.Errorf("%w: generation %d cannot be written: %w", ErrCannotPlan, next.Generation, err)
	}

	mode := dcctlv1.Operation_QUORUM
	if wm == coord.WriteForced {
		mode = dcctlv1.Operation_FORCED
	}

	return operation.Plan{
		Type:      dcctlv1.Operation_FAILOVER,
		Target:    r.b,
		Request:   req,
		RequestID: req.GetRequestId(),
		Initiator: initiator,
		Preflight: preflight.Request{
			Action: preflight.Failover,
			Params: preflight.Params{Datacenter: r.b},
			Confirmations: check.Confirmations{
				Isolation: c.GetIsolation(), NotMaxLSN: c.GetNotMaxLsn(), MaxDiscardBytes: c.GetMaxDiscardBytes(),
			},
		},
		Mode:               mode,
		PreviousGeneration: base.State.Generation,
		NewGeneration:      next.Generation,
		PreviousActive:     r.a,
		Steps:              f.steps(r),
	}, nil
}

// discardAsked reports whether v asks for max_discard_bytes: the old active datacenter
// is read as primary and its demotion discards a known amount.
func discardAsked(v check.Verdict) bool {
	return slices.ContainsFunc(v.Checks, func(c check.Check) bool {
		return c.ID == check.PatroniSourceDiscard && c.Result == check.Fail && c.DiscardBytes != nil
	})
}

// Resume rebuilds the steps of the interrupted failover op from its recorded steps, see
// [operation.Resumer].
func (f *Failover) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	r, err := routeOf(op, dcctlv1.Operation_FAILOVER)
	if err != nil {
		return nil, fmt.Errorf("failover %s: %w", op.GetName(), err)
	}

	if err := f.d.known(slices.Concat([]string{r.a, r.b}, r.c)...); err != nil {
		return nil, fmt.Errorf("failover %s: %w", op.GetName(), err)
	}

	return f.steps(r), nil
}

// steps returns the steps of r. The last stop step stops every stage, a job whose stage
// the plan did not see too. The steps between the generation and the promotion of b
// interrupt on any error: the generation names b, so a must not start again.
func (f *Failover) steps(r route) []operation.Step {
	steps := make([]operation.Step, 0, len(r.stop)+len(r.start)+len(r.c)+4)

	steps = append(steps, operation.Step{
		Type: dcctlv1.Operation_Step_GENERATION, Datacenter: r.b,
		Run: f.generation(r), Rollback: f.keepGeneration(r),
	})

	for i, stage := range r.stop {
		from := stage
		if i == len(r.stop)-1 {
			from = 0
		}

		steps = append(steps, operation.Step{
			Type: dcctlv1.Operation_Step_APPS_STOP, Stage: int32(stage), Datacenter: r.a, // #nosec G115 -- stages come from stagesOf or a recorded int32
			Run: onward(f.stopOld(r, from)),
		})
	}

	steps = append(steps,
		operation.Step{Type: dcctlv1.Operation_Step_PATRONI_DEMOTE, Datacenter: r.a, Run: onward(f.demoteOld(r))},
		operation.Step{
			Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, Datacenter: r.b, PointOfNoReturn: true,
			Run: f.promoteOver(r),
		},
	)

	steps = append(steps, f.startSteps(r)...)

	for _, c := range r.c {
		steps = append(steps, operation.Step{
			Type: dcctlv1.Operation_Step_PATRONI_REPOINT, Datacenter: c, Run: f.repointUnlessDiverged(r, c),
		})
	}

	return append(steps, operation.Step{Type: dcctlv1.Operation_Step_VERIFY, Run: f.verifyFailover(r)})
}
