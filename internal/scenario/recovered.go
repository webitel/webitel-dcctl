package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// MarkRecovered clears needs_recovery of a datacenter of warm_standby once every member of
// it follows the active datacenter: the step reads that anew and refuses otherwise, and
// the record the operation started from is written without the datacenter on a majority
// of voters, also when the record is forced.
//
// A payload split of that record, different payloads of its revision none of which is
// on a majority, is refused: one payload must be chosen explicitly first.
type MarkRecovered struct {
	core
}

// NewMarkRecovered returns the mark-recovered of warm_standby acting through d.
func NewMarkRecovered(d Deps) (*MarkRecovered, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	return &MarkRecovered{core{d: d}}, nil
}

// Plan returns the plan of req on the state pre was decided on, as [Demote.Plan] does.
func (m *MarkRecovered) Plan(req *dcctlv1.MarkDatacenterRecoveredRequest, initiator *dcctlv1.Initiator, pre preflight.Result) (
	operation.Plan, error,
) {
	return m.planStandby(dcctlv1.Operation_MARK_RECOVERED, preflight.MarkRecovered, dcctlv1.Operation_QUORUM, req, initiator, pre, m.steps)
}

// Resume rebuilds the steps of the interrupted mark-recovered op, see [operation.Resumer].
func (m *MarkRecovered) Resume(_ context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	return m.resumeStandby(op, dcctlv1.Operation_MARK_RECOVERED, m.steps)
}

func (m *MarkRecovered) steps(dc string, generation int64) []operation.Step {
	return []operation.Step{
		{Type: dcctlv1.Operation_Step_PATRONI_FOLLOW, Datacenter: dc, Run: m.recovered(dc, generation)},
		{Type: dcctlv1.Operation_Step_GENERATION, Datacenter: dc, Run: m.clear(dc, generation)},
	}
}

// recovered refuses unless every member of dc streams from the active datacenter on its
// timeline, as read now.
func (m *MarkRecovered) recovered(dc string, generation int64) operation.StepFunc {
	return func(ctx context.Context, _ *operation.Exec) error {
		active, err := m.activeAt(ctx, generation, dc)
		if err != nil {
			return err
		}

		return m.follows(ctx, dc, active)
	}
}

// follows refuses unless every member of dc streams from active on its timeline.
func (m *MarkRecovered) follows(ctx context.Context, dc, active string) error {
	if err := m.d.Patroni.Recovered(ctx, m.d.cluster(dc), m.d.cluster(active)); err != nil {
		return fmt.Errorf("%s is not marked recovered, it does not follow %s in full: %w", dc, active, err)
	}

	return nil
}

// clear writes the record the operation started from without dc in needs_recovery, once
// every member of dc is read following the active datacenter again: a resumed step
// writes on what dc shows then. A record that a majority of voters holds without dc
// already needs nothing; this is how a resumed step finds the write of an earlier run.
// A payload split is refused, and so is a record the voters no longer hold. A write that
// does not reach a majority of voters interrupts the operation, to be finished by a
// resume.
func (m *MarkRecovered) clear(dc string, generation int64) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		view, err := m.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		active, err := m.startedFrom(view, generation, dc)
		if err != nil {
			return err
		}

		// startedFrom found the effective record the operation started from.
		eff := *view.Effective

		switch {
		case !eff.Settled:
			return payloadSplit(eff.State.Generation, eff.State.Revision, active)
		case recoveredOn(view, eff, dc) >= view.Majority:
			x.Skip(dc + " does not need recovery on a majority of voters already")

			return nil
		}

		if err := m.follows(ctx, dc, active); err != nil {
			return err
		}

		rec := coord.RecordKey{Generation: generation, ActiveDatacenter: active}

		res, err := x.Write(ctx, "remove "+dc+" from needs_recovery", func(ctx context.Context) (coord.WriteResult, error) {
			return m.d.Coordinator.RemoveNeedsRecovery(ctx, rec, dc)
		})

		switch {
		case errors.Is(err, coord.ErrUnsettled):
			return fmt.Errorf("%w: %w", payloadSplit(eff.State.Generation, eff.State.Revision, active), err)
		case err != nil:
			return err
		case len(res.Accepted()) < res.Majority:
			return interrupt(fmt.Errorf("needs_recovery without %s is on %d voters, a majority of %d is needed: %w",
				dc, len(res.Accepted()), res.Majority, coord.ErrUncommitted))
		}

		return nil
	}
}

// recoveredOn counts the voters that hold rec at its revision without dc in
// needs_recovery.
func recoveredOn(view coord.View, rec coord.Record, dc string) int {
	n := 0

	for _, vs := range view.Voters {
		if vs.Status == coord.StatusPresent && coord.KeyOf(vs.State) == coord.KeyOf(rec.State) &&
			vs.State.Revision == rec.State.Revision && !slices.Contains(vs.State.NeedsRecovery, dc) {
			n++
		}
	}

	return n
}

// payloadSplit is the refusal of a change of generation of active, whose voters hold
// different payloads of revision and none of them on a majority.
func payloadSplit(generation, revision int64, active string) error {
	return fmt.Errorf("payload split: the voters hold different payloads of revision %d of generation %d of %s and none is "+
		"on a majority; needs_recovery changes only once one payload is chosen explicitly", revision, generation, active)
}
