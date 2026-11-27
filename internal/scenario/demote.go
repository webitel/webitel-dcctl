package scenario

import (
	"context"
	"fmt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// Demote is the demotion of warm_standby of an old primary: the Patroni cluster of a
// datacenter that is not active but runs a primary becomes a standby of the active
// datacenter, which Patroni rewinds it to, and every member of it follows.
//
// The WAL the old primary holds past the history of the active datacenter is discarded:
// it is decided before the demotion and measured again by it, which goes on only when
// max_discard_bytes covers it. The demotion is the point of no return.
type Demote struct {
	core
}

// NewDemote returns the demotion of warm_standby acting through d.
func NewDemote(d Deps) (*Demote, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &Demote{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict,
// so that a refusal can show the plan too. It fails with [ErrCannotPlan] when that
// state does not tell the active datacenter, when the datacenter of req is it or is not
// configured, or for a profile other than warm_standby.
func (d *Demote) Plan(req *dcctlv1.DemoteDatacenterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	return d.planStandby(dcctlv1.Operation_DEMOTE, preflight.Demote, dcctlv1.Operation_MODE_UNSPECIFIED, req, initiator, pre, d.steps)
}

// Resume rebuilds the steps of the interrupted demotion op, see [operation.Resumer].
func (d *Demote) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	return d.resumeStandby(op, dcctlv1.Operation_DEMOTE, d.steps)
}

func (d *Demote) steps(dc string, generation int64) []operation.Step {
	return []operation.Step{
		{
			Type: dcctlv1.Operation_Step_PATRONI_DEMOTE, Datacenter: dc, PointOfNoReturn: true,
			Recheck: d.discardBeforeDemote(dc, generation), Run: d.demoteDiverged(dc, generation),
		},
		{Type: dcctlv1.Operation_Step_PATRONI_FOLLOW, Datacenter: dc, Run: d.follow(dc, generation)},
	}
}

// discardBeforeDemote is patroni.divergence of dc, read before its demotion runs; a
// cluster that is a standby of the active datacenter already discards nothing more.
func (d *Demote) discardBeforeDemote(dc string, generation int64) func(context.Context) ([]check.Check, error) {
	return func(ctx context.Context) ([]check.Check, error) {
		active, err := d.activeAt(ctx, generation, dc)
		if err != nil {
			return nil, err
		}

		if demoted, err := d.d.Patroni.Demoted(ctx, d.d.cluster(dc), d.d.cluster(active)); err == nil && demoted {
			return nil, nil
		}

		div, err := d.d.Patroni.DivergenceOf(ctx, d.d.cluster(dc), d.d.cluster(active))

		return []check.Check{discardCheck(dc, active, div, err)}, nil
	}
}

// demoteDiverged makes the cluster of dc a standby of the active datacenter. The WAL
// that discards is measured again, against max_discard_bytes the run of the step
// confirmed, unless dc is a standby of the active datacenter already, as after a
// demotion an interrupted run made. The active datacenter must be read as primary.
func (d *Demote) demoteDiverged(dc string, generation int64) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		active, err := d.activeAt(ctx, generation, dc)
		if err != nil {
			return err
		}

		demoted, err := d.d.Patroni.Demoted(ctx, d.d.cluster(dc), d.d.cluster(active))

		switch {
		case err != nil && ctx.Err() != nil:
			return err
		case demoted:
			x.Progress(dc + " is a standby of " + active + " already")
		default:
			div, err := d.d.Patroni.DivergenceOf(ctx, d.d.cluster(dc), d.d.cluster(active))
			if err := confirmDiscard(ctx, x, dc, active, div, err); err != nil {
				return err
			}
		}

		_, err = d.d.Patroni.Demote(ctx, d.d.cluster(dc), d.d.cluster(active), patroniOptions(x, d.guardOn(x, active)))
		if err == nil || stopping(ctx, err) {
			return err
		}

		return interrupt(fmt.Errorf("%s is not demoted, the operator decides: %w", dc, err))
	}
}

// follow waits for every member of dc to follow the active datacenter; what does not is
// a warning, left to a rebuild by hand.
func (d *Demote) follow(dc string, generation int64) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		active, err := d.activeAt(ctx, generation, dc)
		if err != nil {
			return err
		}

		_, err = d.d.Patroni.Recover(ctx, d.d.cluster(dc), d.d.cluster(active), patroniOptions(x, nil))
		if err == nil || stopping(ctx, err) {
			return err
		}

		x.Warn(fmt.Sprintf("%s does not follow %s in full: %v; a cluster that is not rewound or a member that does not "+
			"catch up needs a rebuild by hand", dc, active, err))

		return nil
	}
}
