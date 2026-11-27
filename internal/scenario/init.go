package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Initialize creates the first dcctl/state of an installation, generation 1 with the
// datacenter of the request active, on every KV, and takes the lease of that datacenter.
// It is the only action that creates the state where no voter holds one; the arbiter
// gets it from its own initialization.
//
// Right before the write the step reads again where the primary runs: in warm_standby
// the cluster of the active datacenter must lead as primary and no other cluster may be,
// every one of them read; in stretch the leader must run in the active datacenter and
// nothing else may report primary. The record carries the timeline of that primary.
type Initialize struct {
	core

	profile config.Profile
}

// NewInitialize returns the initialization of profile acting through d.
func NewInitialize(d Deps, profile config.Profile) (*Initialize, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	switch profile {
	case config.ProfileWarmStandby, config.ProfileStretch:
	default:
		return nil, fmt.Errorf("unknown profile %q", profile)
	}

	return &Initialize{core: core{d: d}, profile: profile}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict, so
// that a refusal can show the plan too. It fails with [ErrCannotPlan] when the
// datacenter of req is not configured or pre is of another profile.
func (n *Initialize) Plan(req *dcctlv1.InitializeDatacentersRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (
	operation.Plan, error,
) {
	if p := pre.Status.Profile; p != n.profile {
		return operation.Plan{}, fmt.Errorf("%w: initialization of %s is not one of %s", ErrCannotPlan, p, n.profile)
	}

	dc := datacenterID(req.GetActiveDatacenter())
	if err := n.d.known(dc); err != nil {
		return operation.Plan{}, fmt.Errorf("%w: %w", ErrCannotPlan, err)
	}

	return operation.Plan{
		Type:          dcctlv1.Operation_INITIALIZE,
		Target:        dc,
		Request:       req,
		RequestID:     req.GetRequestId(),
		Initiator:     initiator,
		Preflight:     preflight.Request{Action: preflight.Init, Params: preflight.Params{Datacenter: dc}},
		Mode:          dcctlv1.Operation_QUORUM,
		NewGeneration: 1,
		Steps:         n.steps(dc),
	}, nil
}

// Resume rebuilds the steps of the interrupted initialization op, see [operation.Resumer].
func (n *Initialize) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	dc := datacenterID(op.GetTarget())

	switch {
	case op.GetType() != dcctlv1.Operation_INITIALIZE:
		return nil, fmt.Errorf("initialize %s: operation of type %s is not of type %s", op.GetName(), op.GetType(), dcctlv1.Operation_INITIALIZE)
	case op.GetNewGeneration() != 1:
		return nil, fmt.Errorf("initialize %s: generation %d is not the first", op.GetName(), op.GetNewGeneration())
	}

	if err := n.d.known(dc); err != nil {
		return nil, fmt.Errorf("initialize %s: %w", op.GetName(), err)
	}

	return n.steps(dc), nil
}

func (n *Initialize) steps(dc string) []operation.Step {
	return []operation.Step{{Type: dcctlv1.Operation_Step_GENERATION, Datacenter: dc, Run: n.create(dc)}}
}

// create writes the first record with dc active on every KV, see
// [coord.Coordinator.Initialize], and takes the lease of dc. A record an earlier run
// wrote is finished as it is; one the voters hold committed already only needs the
// lease.
func (n *Initialize) create(dc string) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		opID := operationID(x.Operation().GetName())
		rec := coord.RecordKey{Generation: 1, ActiveDatacenter: dc}

		view, err := n.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		if eff := view.Effective; eff == nil || coord.KeyOf(eff.State) != rec || eff.State.OpID != opID {
			if err := n.write(ctx, x, view, rec, opID); err != nil {
				return err
			}
		}

		err = x.AcquireLease(ctx, coord.LeaseHolder{Datacenter: dc, Generation: 1})

		switch {
		case err == nil:
			return nil
		case stopping(ctx, err):
			return interrupt(err)
		}

		x.Warn(fmt.Sprintf("lease of %s not taken, its renewal takes it: %v", dc, err))

		return nil
	}
}

// write writes the record rec of the operation opID once the primary is read where rec
// makes it active. A refusal after an earlier run may have written some KVs
// interrupts the operation.
func (n *Initialize) write(ctx context.Context, x *operation.Exec, view coord.View, rec coord.RecordKey, opID string) error {
	timeline, err := n.primaryIn(ctx, x, rec.ActiveDatacenter)
	if err != nil {
		return fmt.Errorf("%s is not recorded active: %w", rec.ActiveDatacenter, err)
	}

	s, held := heldRecord(view, rec, opID)
	if !held {
		s = kv.State{Generation: 1, ActiveDatacenter: rec.ActiveDatacenter, OpID: opID, PatroniTimeline: timeline}
	}

	x.Progress(fmt.Sprintf("creating generation 1 with %s active on timeline %d", rec.ActiveDatacenter, s.PatroniTimeline))

	_, err = x.Write(ctx, "initialize state", func(ctx context.Context) (coord.WriteResult, error) {
		return n.d.Coordinator.Initialize(ctx, s)
	})

	if err != nil && (held || x.Resumed()) {
		return interrupt(err)
	}

	return err
}

// primaryIn returns the timeline of the primary of dc, which must be the only primary of
// the installation, read now.
func (n *Initialize) primaryIn(ctx context.Context, x *operation.Exec, dc string) (int64, error) {
	if n.profile == config.ProfileStretch {
		return n.leaderIn(ctx, dc)
	}

	others := slices.DeleteFunc(n.d.names(), func(name string) bool { return name == dc })

	if _, err := n.d.Patroni.NoPrimary(ctx, n.d.clusters(others...), nil, patroniOptions(x, nil)); err != nil {
		return 0, fmt.Errorf("no other cluster may be primary: %w", err)
	}

	l, err := n.d.Patroni.Leader(ctx, n.d.cluster(dc))
	if err != nil {
		return 0, err
	}

	if l.Probe == nil || l.Probe.Role != patroni.RolePrimary || l.Probe.Timeline == nil {
		return 0, fmt.Errorf("the leader %s of %s is not read as a primary on a known timeline", l.Name, dc)
	}

	return *l.Probe.Timeline, nil
}

// leaderIn returns the timeline of the leader of the cluster of stretch, which must run
// in dc with nothing else reporting primary.
func (n *Initialize) leaderIn(ctx context.Context, dc string) (int64, error) {
	p, err := n.d.Patroni.Place(ctx, n.d.cluster(dc), n.d.names())
	if err != nil {
		return 0, err
	}

	l := p.Leader

	switch {
	case l == nil:
		return 0, errors.New("the cluster has no leader")
	case l.Datacenter != dc:
		return 0, fmt.Errorf("the leader %s runs in %s", l.Name, where(l.Datacenter))
	case l.Probe == nil || l.Probe.Role != patroni.RolePrimary || l.Probe.Timeline == nil:
		return 0, fmt.Errorf("the leader %s of %s is not read as a primary on a known timeline", l.Name, dc)
	}

	if stray := strayPrimaries(p, l); len(stray) > 0 {
		return 0, fmt.Errorf("%s leads in %s and %s is primary too", l.Name, dc, strings.Join(stray, ", "))
	}

	return *l.Probe.Timeline, nil
}
