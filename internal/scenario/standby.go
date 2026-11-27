package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// Repoint points the Patroni cluster of a standby datacenter of warm_standby at the
// active datacenter. The WAL a diverged standby holds past the history of the active
// datacenter is discarded: it is decided before the repoint and measured again by it,
// which goes on only when max_discard_bytes covers it. A repoint that does not complete
// in time is a warning.
type Repoint struct {
	core
}

// NewRepoint returns the repoint of warm_standby acting through d.
func NewRepoint(d Deps) (*Repoint, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &Repoint{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, as [Demote.Plan] does.
func (r *Repoint) Plan(req *dcctlv1.RepointDatacenterRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	return r.planStandby(dcctlv1.Operation_REPOINT, preflight.Repoint, dcctlv1.Operation_MODE_UNSPECIFIED, req, initiator, pre, r.steps)
}

// Resume rebuilds the steps of the interrupted repoint op, see [operation.Resumer].
func (r *Repoint) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	return r.resumeStandby(op, dcctlv1.Operation_REPOINT, r.steps)
}

func (r *Repoint) steps(dc string, generation int64) []operation.Step {
	return []operation.Step{{
		Type: dcctlv1.Operation_Step_PATRONI_REPOINT, Datacenter: dc, PointOfNoReturn: true,
		Recheck: r.discardBeforeRepoint(dc, generation), Run: r.repointDiverged(dc, generation),
	}}
}

// discardBeforeRepoint is patroni.divergence of dc, read before its repoint runs.
func (r *Repoint) discardBeforeRepoint(dc string, generation int64) func(context.Context) ([]check.Check, error) {
	return func(ctx context.Context) ([]check.Check, error) {
		active, err := r.activeAt(ctx, generation, dc)
		if err != nil {
			return nil, err
		}

		div, err := r.d.Patroni.Divergence(ctx, r.d.cluster(dc), r.d.cluster(active))

		return []check.Check{discardCheck(dc, active, div, err)}, nil
	}
}

// repointDiverged points the standby dc at the active datacenter once the WAL that
// discards, measured again, is covered by max_discard_bytes the run of the step
// confirmed. dc read as primary, or a cluster the repoint may not change, interrupts the
// operation; a repoint that does not complete in time is a warning.
func (r *Repoint) repointDiverged(dc string, generation int64) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		active, err := r.activeAt(ctx, generation, dc)
		if err != nil {
			return err
		}

		div, err := r.d.Patroni.Divergence(ctx, r.d.cluster(dc), r.d.cluster(active))

		var pe *pgctl.PrimaryError
		if errors.As(err, &pe) {
			return interrupt(fmt.Errorf("%s is primary, it is demoted, not repointed: %w", dc, err))
		}

		if err := confirmDiscard(ctx, x, dc, active, div, err); err != nil {
			return err
		}

		_, err = r.d.Patroni.Repoint(ctx, r.d.cluster(dc), r.d.cluster(active), patroniOptions(x, r.guardOn(x, active)))

		switch {
		case err == nil, stopping(ctx, err):
			return err
		case errors.Is(err, pgctl.ErrTimeout):
			x.Warn(fmt.Sprintf("%s is not repointed to %s: %v", dc, active, err))

			return nil
		}

		return interrupt(fmt.Errorf("%s is not repointed to %s: %w", dc, active, err))
	}
}

// discardCheck is patroni.divergence of dc from active as a step reads it, div with err:
// a divergence that is not known has no amount, which no bound covers.
func discardCheck(dc, active string, div patroni.Divergence, err error) check.Check {
	c := check.Check{
		ID: check.PatroniDivergence, Result: check.Pass, Effect: check.Confirm, Confirmation: check.ConfirmMaxDiscardBytes,
		Datacenter: dc,
	}

	switch {
	case err != nil:
		c.Result, c.Detail = check.Fail, fmt.Sprintf("the WAL %s discards to follow %s is not known: %v", dc, active, err)
	case div.Diverged:
		c.Result, c.Detail, c.DiscardBytes = check.Fail, fmt.Sprintf("%s diverged from %s, following it discards %d bytes", dc, active,
			div.Bytes), &div.Bytes
	default:
		c.Detail = dc + " did not diverge from " + active
	}

	return c
}

// confirmDiscard checks div, the divergence of dc from active read now with err, against
// max_discard_bytes the run of the step confirmed. A divergence that is not known, or
// one larger than the bound, interrupts the operation, telling the bound that covers it;
// one within the bound is a warning, as that WAL is lost.
func confirmDiscard(ctx context.Context, x *operation.Exec, dc, active string, div patroni.Divergence, err error) error {
	confirmed := x.Confirmed().MaxDiscardBytes

	switch {
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil:
		return interrupt(fmt.Errorf("the WAL %s discards to follow %s is not known: %w", dc, active, err))
	case !div.Diverged:
		return nil
	case div.Bytes > confirmed:
		return interrupt(fmt.Errorf("%s diverged from %s at %d by %d bytes, %d are confirmed: max_discard_bytes of %d discards them",
			dc, active, div.LSN, div.Bytes, confirmed, div.Bytes))
	}

	x.Warn(fmt.Sprintf("%s diverged from %s at %d, following it discards %d bytes of WAL", dc, active, div.LSN, div.Bytes))

	return nil
}

// standbyRequest is a request of an action on one datacenter of warm_standby.
type standbyRequest interface {
	proto.Message
	GetName() string
	GetConfirmation() *dcctlv1.Confirmation
	GetRequestId() string
}

// planStandby returns the plan of typ, an action on the datacenter req names that
// writes the generation in mode, on the state pre was decided on; steps returns its
// steps. The datacenter must be configured and not the active one of the record the
// action starts from, which the operation starts from only while the voters hold it:
// the steps act under it.
func (s *core) planStandby(typ dcctlv1.Operation_Type, action preflight.Action, mode dcctlv1.Operation_Mode, req standbyRequest,
	initiator *dcctlv1.Initiator, pre preflight.Result, steps func(dc string, generation int64) []operation.Step,
) (operation.Plan, error) {
	st := pre.Status
	name := strings.ToLower(typ.String())

	if st.Profile != config.ProfileWarmStandby {
		return operation.Plan{}, fmt.Errorf("%w: %s applies to %s only, not to %s", ErrCannotPlan, name, config.ProfileWarmStandby, st.Profile)
	}

	base := coord.Base(st.View, false)
	if base == nil {
		return operation.Plan{}, fmt.Errorf("%w: the active datacenter is not known", ErrCannotPlan)
	}

	dc, active := datacenterID(req.GetName()), base.State.ActiveDatacenter

	switch {
	case dc == active:
		return operation.Plan{}, fmt.Errorf("%w: %s is the active datacenter", ErrCannotPlan, dc)
	case !slices.ContainsFunc(st.Datacenters, func(d status.Datacenter) bool { return d.Name == dc }):
		return operation.Plan{}, fmt.Errorf("%w: datacenter %q is not configured", ErrCannotPlan, dc)
	}

	if err := s.d.known(dc, active); err != nil {
		return operation.Plan{}, fmt.Errorf("%w: %w", ErrCannotPlan, err)
	}

	c := req.GetConfirmation()

	return operation.Plan{
		Type:      typ,
		Target:    dc,
		Request:   req,
		RequestID: req.GetRequestId(),
		Initiator: initiator,
		Preflight: preflight.Request{
			Action: action,
			Params: preflight.Params{Datacenter: dc},
			Confirmations: check.Confirmations{
				Isolation: c.GetIsolation(), NotMaxLSN: c.GetNotMaxLsn(), MaxDiscardBytes: c.GetMaxDiscardBytes(),
			},
		},
		Mode:               mode,
		PreviousGeneration: base.State.Generation,
		PreviousActive:     active,
		Steps:              steps(dc, base.State.Generation),
	}, nil
}

// resumeStandby rebuilds by steps the steps of op, an operation of type typ on one
// datacenter of warm_standby.
func (s *core) resumeStandby(op *dcctlv1.Operation, typ dcctlv1.Operation_Type, steps func(dc string, generation int64) []operation.Step) (
	[]operation.Step, error,
) {
	name := strings.ToLower(typ.String())
	dc := datacenterID(op.GetTarget())

	switch {
	case op.GetType() != typ:
		return nil, fmt.Errorf("%s %s: operation of type %s is not of type %s", name, op.GetName(), op.GetType(), typ)
	case dc == "" || op.GetPreviousGeneration() <= 0:
		return nil, fmt.Errorf("%s %s: no datacenter or generation to act under", name, op.GetName())
	}

	if err := s.d.known(dc); err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, op.GetName(), err)
	}

	return steps(dc, op.GetPreviousGeneration()), nil
}

// activeAt returns the active datacenter of the record of generation, the one the
// operation started from, as the voters hold it now. Another generation, or a record
// that does not tell the active datacenter, fails, and so does dc being active.
func (s *core) activeAt(ctx context.Context, generation int64, dc string) (string, error) {
	view, err := s.d.Coordinator.Read(ctx)
	if err != nil {
		return "", err
	}

	return s.startedFrom(view, generation, dc)
}

// startedFrom is [core.activeAt] on view.
func (s *core) startedFrom(view coord.View, generation int64, dc string) (string, error) {
	base := coord.Base(view, false)

	switch {
	case base == nil:
		return "", fmt.Errorf("the voters do not tell the active datacenter of generation %d", generation)
	case base.State.Generation != generation:
		return "", fmt.Errorf("the voters hold generation %d of %s, the operation started from generation %d",
			base.State.Generation, base.State.ActiveDatacenter, generation)
	case base.State.ActiveDatacenter == dc:
		return "", fmt.Errorf("%s is the active datacenter", dc)
	}

	if err := s.d.known(base.State.ActiveDatacenter); err != nil {
		return "", err
	}

	return base.State.ActiveDatacenter, nil
}

// guardOn is the guard of a step of x that acts on a standby of active: the lock is
// held and the voters hold the record the operation started from, whose active
// datacenter is active.
func (s *core) guardOn(x *operation.Exec, active string) func(context.Context) error {
	return s.guard(x, route{a: active}, beforeGeneration)
}
