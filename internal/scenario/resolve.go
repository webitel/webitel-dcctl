package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// Resolve resolves a conflict, the valid records of the highest valid generation with
// different active datacenters, in favor of the datacenter the operator chose: the
// record of the conflict that names it is the base, and the new record names it active
// one generation above every generation a voter holds, with the isolation of the other
// side confirmed. The other datacenters of the conflict are written in need of
// recovery, as each ran as active on its side, and so is every datacenter a side of
// the conflict holds in need of recovery. Their apps stop by fencing once the new
// record is decided, and their old primaries are demoted by the operator.
//
// It changes neither apps nor Patroni. A conflict is possible in warm_standby only, with
// a voter per datacenter.
type Resolve struct {
	core
}

// NewResolve returns the resolve of a conflict acting through d.
func NewResolve(d Deps) (*Resolve, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &Resolve{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, whatever its verdict, so
// that a refusal can show the plan too. It fails with [ErrCannotPlan] when the winner is
// not configured or no record of a conflict names it.
func (r *Resolve) Plan(req *dcctlv1.ResolveConflictRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (operation.Plan, error) {
	view := pre.Status.View
	winner := datacenterID(req.GetWinner())

	if err := r.d.known(winner); err != nil {
		return operation.Plan{}, fmt.Errorf("%w: %w", ErrCannotPlan, err)
	}

	side := conflictSide(view, winner)
	if side == nil {
		return operation.Plan{}, fmt.Errorf("%w: no record of a conflict names %s active", ErrCannotPlan, winner)
	}

	to := resolveRecord(view, side.State, coord.TopGeneration(view)+1, "")
	mode := dcctlv1.Operation_MODE_UNSPECIFIED

	switch m, err := coord.ResolveMode(view, side.State, to); {
	case err != nil:
	case m == coord.WriteForced:
		mode = dcctlv1.Operation_FORCED
	default:
		mode = dcctlv1.Operation_QUORUM
	}

	c := req.GetConfirmation()

	return operation.Plan{
		Type:      dcctlv1.Operation_RESOLVE_CONFLICT,
		Target:    winner,
		Request:   req,
		RequestID: req.GetRequestId(),
		Initiator: initiator,
		Preflight: preflight.Request{
			Action: preflight.Resolve, Params: preflight.Params{Datacenter: winner},
			Confirmations: check.Confirmations{Isolation: c.GetIsolation()},
		},
		Mode:               mode,
		PreviousGeneration: side.State.Generation,
		NewGeneration:      to.Generation,
		Steps:              r.steps(winner),
	}, nil
}

// Resume rebuilds the steps of the interrupted resolve op, see [operation.Resumer].
func (r *Resolve) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	winner := datacenterID(op.GetTarget())

	switch {
	case op.GetType() != dcctlv1.Operation_RESOLVE_CONFLICT:
		return nil, fmt.Errorf("resolve %s: operation of type %s is not of type %s", op.GetName(), op.GetType(),
			dcctlv1.Operation_RESOLVE_CONFLICT)
	case op.GetPreviousGeneration() <= 0 || op.GetNewGeneration() <= op.GetPreviousGeneration():
		return nil, fmt.Errorf("resolve %s: generation %d does not resolve a conflict of generation %d", op.GetName(),
			op.GetNewGeneration(), op.GetPreviousGeneration())
	}

	if err := r.d.known(winner); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", op.GetName(), err)
	}

	return r.steps(winner), nil
}

func (r *Resolve) steps(winner string) []operation.Step {
	return []operation.Step{{Type: dcctlv1.Operation_Step_GENERATION, Datacenter: winner, Run: r.write(winner)}}
}

// conflictSide returns the record of the conflict of view that names dc active, nil when
// there is none.
func conflictSide(view coord.View, dc string) *coord.Record {
	i := slices.IndexFunc(view.Conflict, func(r coord.Record) bool { return r.State.ActiveDatacenter == dc })
	if i < 0 {
		return nil
	}

	return &view.Conflict[i]
}

// resolveRecord returns the record of generation that resolves the conflict of view in
// favor of winner, written by the operation opID: the timeline of winner, and in need of
// recovery every datacenter some side of the conflict holds so, and the active
// datacenters of the other sides, all but winner. A datacenter one side marked is not
// dropped by the choice of the other.
func resolveRecord(view coord.View, winner kv.State, generation int64, opID string) kv.State {
	var recovery []string

	add := func(dcs ...string) {
		for _, dc := range dcs {
			if dc != winner.ActiveDatacenter && !slices.Contains(recovery, dc) {
				recovery = append(recovery, dc)
			}
		}
	}

	add(winner.NeedsRecovery...)

	for _, r := range view.Conflict {
		add(r.State.NeedsRecovery...)
		add(r.State.ActiveDatacenter)
	}

	return kv.State{
		Generation: generation, ActiveDatacenter: winner.ActiveDatacenter, OpID: opID,
		PatroniTimeline: winner.PatroniTimeline, NeedsRecovery: recovery,
	}
}

// write writes the record that resolves the conflict in favor of winner, see
// [coord.Coordinator.Resolve], and takes the lease of winner. The record is built from
// what the voters hold now: the conflict the operation was planned on, of its
// generation, or the record an earlier run wrote, which is finished as it is. The mode
// is the one the voters allow, and the operation records it. A refusal after an earlier
// run may have written some voters interrupts the operation.
func (r *Resolve) write(winner string) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		op := x.Operation()
		next := coord.RecordKey{Generation: op.GetNewGeneration(), ActiveDatacenter: winner}
		opID := operationID(op.GetName())

		view, err := r.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		forced := false

		if eff := view.Effective; eff != nil && coord.KeyOf(eff.State) == next && eff.State.OpID == opID {
			forced = eff.State.Forced
		} else if forced, err = r.writeRecord(ctx, x, view, next, opID); err != nil {
			return err
		}

		mode := dcctlv1.Operation_QUORUM
		if forced {
			mode = dcctlv1.Operation_FORCED
		}

		if err := x.SetMode(ctx, mode); err != nil {
			x.Warn(fmt.Sprintf("the generation is written %s, the operation does not record it yet: %v", mode, err))
		}

		err = x.AcquireLease(ctx, coord.LeaseHolder{Datacenter: winner, Generation: next.Generation})

		switch {
		case err == nil:
			return nil
		case stopping(ctx, err):
			return interrupt(err)
		}

		x.Warn(fmt.Sprintf("lease of %s not taken, its renewal takes it: %v", winner, err))

		return nil
	}
}

// writeRecord writes next, the record of the operation opID, over the conflict of view
// and tells whether it is forced.
func (r *Resolve) writeRecord(ctx context.Context, x *operation.Exec, view coord.View, next coord.RecordKey, opID string) (bool, error) {
	generation := x.Operation().GetPreviousGeneration()
	to, held := heldRecord(view, next, opID)

	var from kv.State

	switch side := conflictSide(view, next.ActiveDatacenter); {
	case side != nil && side.State.Generation == generation:
		from = side.State
	case held:
		from = kv.State{Generation: generation, ActiveDatacenter: next.ActiveDatacenter}
	case side != nil:
		return false, fmt.Errorf("the conflict is of generation %d, the resolve was planned on generation %d", side.State.Generation, generation)
	default:
		return false, fmt.Errorf("no record of a conflict of generation %d names %s active", generation, next.ActiveDatacenter)
	}

	switch {
	case !held:
		to = resolveRecord(view, from, next.Generation, opID)
	case to.Revision != 0:
		return false, interrupt(fmt.Errorf("generation %d of %s is held at revision %d without being committed",
			next.Generation, next.ActiveDatacenter, to.Revision))
	}

	if !x.Confirmed().Isolation {
		return false, fmt.Errorf("resolving the conflict in favor of %s needs the isolation of the other side confirmed: %w",
			next.ActiveDatacenter, coord.ErrConfirmationRequired)
	}

	mode, err := coord.ResolveMode(view, from, to)
	if err == nil {
		to.Forced = mode == coord.WriteForced

		x.Progress(fmt.Sprintf("writing generation %d with %s active over the conflict of generation %d, %s", next.Generation,
			next.ActiveDatacenter, generation, mode))

		_, err = x.Write(ctx, "resolve conflict", func(ctx context.Context) (coord.WriteResult, error) {
			return r.d.Coordinator.Resolve(ctx, from, to, coord.AdvanceOptions{Mode: mode, IsolationConfirmed: true})
		})
	}

	switch {
	case err == nil:
		return to.Forced, nil
	case held || x.Resumed(), errors.Is(err, coord.ErrSuperseded):
		return false, interrupt(err)
	}

	return false, err
}
