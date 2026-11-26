package scenario

import (
	"fmt"
	"math"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// Switch is the planned switch of warm_standby: the apps of the active datacenter A stop,
// its Patroni cluster becomes a standby of the target B, B catches up, the new
// generation names B, B is promoted, its apps start, the other standby clusters follow
// B and the result is verified.
//
// The steps before the promotion of B roll back: the cluster of A is promoted again and
// its apps start. The promotion of B is the point of no return.
type Switch struct {
	d Deps
}

// NewSwitch returns the switch of warm_standby acting through d.
func NewSwitch(d Deps) (*Switch, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &Switch{d: d}, nil
}

// route is a switch from a to b. c are the other datacenters, stop the Nomad stages of
// a from the highest down and start those of b from the lowest up.
type route struct {
	a, b  string
	c     []string
	stop  []int
	start []int
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict,
// so that a refusal can show the plan too. It fails with [ErrCannotPlan] when that
// state does not tell the active datacenter, when the target is it or is not
// configured, or for a profile other than warm_standby.
func (s *Switch) Plan(req *dcctlv1.SwitchDatacenterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	st := pre.Status

	if st.Profile != config.ProfileWarmStandby {
		return operation.Plan{}, fmt.Errorf("%w: switch of %s is not a switch of %s", ErrCannotPlan, st.Profile, config.ProfileWarmStandby)
	}

	base := coord.Base(st.View, false)
	if base == nil {
		return operation.Plan{}, fmt.Errorf("%w: the active datacenter is not known", ErrCannotPlan)
	}

	r := route{a: base.State.ActiveDatacenter, b: datacenterID(req.GetName())}

	switch {
	case r.b == r.a:
		return operation.Plan{}, fmt.Errorf("%w: %s is active already", ErrCannotPlan, r.b)
	case !slices.ContainsFunc(st.Datacenters, func(d status.Datacenter) bool { return d.Name == r.b }):
		return operation.Plan{}, fmt.Errorf("%w: datacenter %q is not configured", ErrCannotPlan, r.b)
	}

	for _, d := range st.Datacenters {
		switch d.Name {
		case r.a:
			r.stop = stagesOf(d.Nomad, true)
		case r.b:
			r.start = stagesOf(d.Nomad, false)
		default:
			r.c = append(r.c, d.Name)
		}
	}

	if r.stop == nil {
		return operation.Plan{}, fmt.Errorf("%w: active datacenter %q is not configured", ErrCannotPlan, r.a)
	}

	if err := s.d.known(slices.Concat([]string{r.a, r.b}, r.c)...); err != nil {
		return operation.Plan{}, fmt.Errorf("%w: %w", ErrCannotPlan, err)
	}

	c := req.GetConfirmation()

	return operation.Plan{
		Type:      dcctlv1.Operation_SWITCH,
		Target:    r.b,
		Request:   req,
		RequestID: req.GetRequestId(),
		Initiator: initiator,
		Preflight: preflight.Request{
			Action: preflight.Switch,
			Params: preflight.Params{Datacenter: r.b},
			Confirmations: check.Confirmations{
				Isolation: c.GetIsolation(), NotMaxLSN: c.GetNotMaxLsn(), MaxDiscardBytes: c.GetMaxDiscardBytes(),
			},
		},
		Mode:               dcctlv1.Operation_QUORUM,
		PreviousGeneration: base.State.Generation,
		NewGeneration:      base.State.Generation + 1,
		PreviousActive:     r.a,
		Steps:              s.steps(r),
	}, nil
}

// stagesOf returns the valid stages of the managed jobs of n, the highest first when
// down; one stage when there is none, whose step still handles every job found then.
func stagesOf(n status.Nomad, down bool) []int {
	out := make([]int, 0, len(n.Stages))

	for _, st := range n.Stages {
		if st.Stage > 0 {
			out = append(out, st.Stage)
		}
	}

	slices.Sort(out)
	out = slices.Compact(out)

	if len(out) == 0 {
		return []int{1}
	}

	if down {
		slices.Reverse(out)
	}

	return out
}

// steps returns the steps of r. The last stop step stops every stage and the first one
// starts every stage when rolled back, as the last start step starts every stage: a job
// whose stage the plan did not see is handled too.
func (s *Switch) steps(r route) []operation.Step {
	steps := make([]operation.Step, 0, len(r.stop)+len(r.start)+len(r.c)+7)

	for i, stage := range r.stop {
		from, to := stage, stage
		if i == len(r.stop)-1 {
			from = 0
		}

		if i == 0 {
			to = math.MaxInt
		}

		steps = append(steps, operation.Step{
			Type: dcctlv1.Operation_Step_APPS_STOP, Stage: int32(stage), Datacenter: r.a, //nolint:gosec // a Nomad stage is small
			Run:      s.stopApps(r, from),
			Rollback: s.startApps(r, r.a, to, beforeGeneration),
		})
	}

	steps = append(steps,
		operation.Step{
			Type: dcctlv1.Operation_Step_PATRONI_DEMOTE, Datacenter: r.a,
			Run: s.demote(r), Rollback: s.promoteBack(r),
		},
		operation.Step{Type: dcctlv1.Operation_Step_PATRONI_CATCHUP, Datacenter: r.b, Run: s.catchup(r)},
		operation.Step{Type: dcctlv1.Operation_Step_PATRONI_NO_PRIMARY, Run: s.noPrimary(r)},
		operation.Step{
			Type: dcctlv1.Operation_Step_GENERATION, Datacenter: r.b,
			Run: s.generation(r), Rollback: s.keepGeneration(r),
		},
		operation.Step{
			Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, Datacenter: r.b, PointOfNoReturn: true,
			Run: s.promote(r),
		},
	)

	for i, stage := range r.start {
		to := stage
		if i == len(r.start)-1 {
			to = math.MaxInt
		}

		steps = append(steps, operation.Step{
			Type: dcctlv1.Operation_Step_APPS_START, Stage: int32(stage), Datacenter: r.b, //nolint:gosec // a Nomad stage is small
			Run: s.startApps(r, r.b, to, afterGeneration),
		})
	}

	for _, c := range r.c {
		steps = append(steps, operation.Step{Type: dcctlv1.Operation_Step_PATRONI_REPOINT, Datacenter: c, Run: s.repoint(r, c)})
	}

	return append(steps,
		operation.Step{Type: dcctlv1.Operation_Step_PATRONI_FOLLOW, Datacenter: r.a, Run: s.follow(r)},
		operation.Step{Type: dcctlv1.Operation_Step_VERIFY, Run: s.verify()},
	)
}
