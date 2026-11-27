package scenario

import (
	"context"
	"math"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// StretchSwitch is the planned switch of stretch, where one Patroni cluster spans the
// datacenters: the apps of the active datacenter A stop, the new generation names the
// target B, Patroni switches the leadership over to a member of B unless it leads there
// already, the apps of B start, the members of A rejoin as replicas and the result is
// verified.
//
// The steps before the generation roll back: the apps of A start again. The switchover
// is the point of no return; once the generation is written a step that cannot go on
// interrupts the operation.
type StretchSwitch struct {
	core
}

// NewStretchSwitch returns the switch of stretch acting through d.
func NewStretchSwitch(d Deps) (*StretchSwitch, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &StretchSwitch{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict,
// so that a refusal can show the plan too. It fails with [ErrCannotPlan] when that
// state does not tell the active datacenter, when the target is it or is not
// configured, or for a profile other than stretch.
func (s *StretchSwitch) Plan(req *dcctlv1.SwitchDatacenterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	return s.planSwitch(req, initiator, pre, config.ProfileStretch, s.steps)
}

// Resume rebuilds the steps of the interrupted switch op from its recorded steps, see
// [operation.Resumer].
func (s *StretchSwitch) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	return s.resume(op, dcctlv1.Operation_SWITCH, true, s.steps)
}

// steps returns the steps of r. The last stop step stops every stage and the first one
// starts every stage when rolled back, as the last start step starts every stage: a job
// whose stage the plan did not see is handled too.
func (s *StretchSwitch) steps(r route) []operation.Step {
	steps := make([]operation.Step, 0, len(r.stop)+len(r.start)+4)

	for i, stage := range r.stop {
		from, to := stage, stage
		if i == len(r.stop)-1 {
			from = 0
		}

		if i == 0 {
			to = math.MaxInt
		}

		steps = append(steps, operation.Step{
			Type: dcctlv1.Operation_Step_APPS_STOP, Stage: int32(stage), Datacenter: r.a, // #nosec G115 -- stages come from stagesOf or a recorded int32
			Run:      s.stopApps(r, from),
			Rollback: s.startApps(r, r.a, to, beforeGeneration),
		})
	}

	steps = append(steps,
		operation.Step{
			Type: dcctlv1.Operation_Step_GENERATION, Datacenter: r.b,
			Run: s.generation(r), Rollback: s.keepGeneration(r),
		},
		operation.Step{
			Type: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, Datacenter: r.b, PointOfNoReturn: true,
			Run: s.handOver(r),
		},
	)

	steps = append(steps, s.startSteps(r)...)

	return append(steps,
		operation.Step{Type: dcctlv1.Operation_Step_PATRONI_FOLLOW, Datacenter: r.a, Run: s.rejoin(r)},
		operation.Step{Type: dcctlv1.Operation_Step_VERIFY, Run: s.verify()},
	)
}

// StretchFailover is the failover of stretch from the active datacenter A, lost or
// isolated, to B: the new generation names B active, the apps of A are stopped when
// its Nomad answers, a member of B is promoted by a manual failover when the cluster
// has no leader, the apps of B start and the result is verified. When Patroni elected a
// leader in B itself the failover of the cluster is skipped.
//
// Nothing rolls back once the generation is written: a step after it that cannot go on
// interrupts the operation. The failover of the cluster is the point of no return.
type StretchFailover struct {
	core
}

// NewStretchFailover returns the failover of stretch acting through d.
func NewStretchFailover(d Deps) (*StretchFailover, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &StretchFailover{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict,
// so that a refusal can show the plan too, as [Failover.Plan] does for stretch;
// max_discard_bytes bounds the WAL the candidate of a failover without a leader lacks.
func (f *StretchFailover) Plan(req *dcctlv1.FailoverDatacenterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	return f.planFailover(req, initiator, pre, config.ProfileStretch, f.steps)
}

// Resume rebuilds the steps of the interrupted failover op from its recorded steps, see
// [operation.Resumer].
func (f *StretchFailover) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	return f.resume(op, dcctlv1.Operation_FAILOVER, true, f.steps)
}

// steps returns the steps of r. The last stop step stops every stage, a job whose stage
// the plan did not see too.
func (f *StretchFailover) steps(r route) []operation.Step {
	steps := make([]operation.Step, 0, len(r.stop)+len(r.start)+3)

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

	steps = append(steps, operation.Step{
		Type: dcctlv1.Operation_Step_PATRONI_FAILOVER, Datacenter: r.b, PointOfNoReturn: true,
		Run: f.failOver(r),
	})

	steps = append(steps, f.startSteps(r)...)

	return append(steps, operation.Step{Type: dcctlv1.Operation_Step_VERIFY, Run: f.verifyFailover(r)})
}
