package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// AbandonRequest abandons an interrupted operation.
type AbandonRequest struct {
	// Operation is the ID of the operation, without operations/.
	Operation string
	// Confirmations are those of the request. Isolation allows reverting the
	// new generation of the operation when voters that do not answer may hold
	// it, which would make it committed.
	Confirmations check.Confirmations
	Initiator     *dcctlv1.Initiator
}

// Abandon ends an interrupted operation ABANDONED without touching the
// infrastructure, and returns it as written. Any instance may abandon any
// operation, also one whose target datacenter is down.
//
// The preflight runs first, then the lock is taken for the operation and
// released at the end. An operation found in no copy is [*NotFoundError], one
// in another state [*NotInterruptedError]. The new generation of the
// operation is reverted to the one before when it is not committed; one that
// voters which do not answer may hold is reverted only with the isolation
// confirmed, otherwise the refusal of op.generation_determined, as the
// preflight gives it. A committed generation stays. A revert that does not
// finish fails Abandon and leaves the operation INTERRUPTED. Every revert is
// journaled as it ends.
func (e *Engine) Abandon(ctx context.Context, req AbandonRequest) (*dcctlv1.Operation, error) {
	if _, err := e.enter(); err != nil {
		return nil, err
	}

	defer e.leave()

	v, err := e.cfg.Preflight.Run(ctx, preflight.Request{
		Action: preflight.Abandon, Params: preflight.Params{Operation: req.Operation}, Confirmations: req.Confirmations,
	})
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}

	if err := answer.Refusal(v, nil, dcctlv1.Operation_MODE_UNSPECIFIED); err != nil {
		return nil, err
	}

	name := operationName(req.Operation)

	lk, err := e.cfg.Locker.Acquire(ctx, e.cfg.Self.lockHolder(req.Operation))
	if err != nil {
		return nil, err
	}

	defer e.release(ctx, lk, name)

	op, _, err := e.interrupted(ctx, name)
	if err != nil {
		return nil, err
	}

	var reverted map[string]any

	if op.GetNewGeneration() > 0 && op.GetTarget() != "" {
		if reverted, err = e.revert(ctx, op, req); err != nil {
			return nil, err
		}
	}

	if err := lk.Err(); err != nil {
		return nil, fmt.Errorf("abandon %s: %w", name, err)
	}

	abandoned, _, err := e.newRecord(op).save(ctx, func(op *dcctlv1.Operation) {
		op.State, op.EndTime = dcctlv1.Operation_ABANDONED, timestamppb.Now()
	})
	if err != nil {
		e.journalOperation(ctx, abandoned, eventAbandon, req.Initiator, "abandon not recorded", dcctlv1.JournalEntry_ERROR, err, reverted)

		return nil, err
	}

	e.journalEnd(ctx, abandoned, eventAbandon, req.Initiator, reverted)

	if err := e.cfg.Recorder.RecordOperation(ctx, abandoned); err != nil {
		e.cfg.Log.WarnCtx(ctx, "operation duration not recorded", logging.Str("operation", name), logging.Err(err))
	}

	e.cfg.Log.InfoCtx(ctx, "operation abandoned", logging.Str("operation", name), logging.Any("generation", reverted))

	return abandoned, nil
}

// revert reverts the new generation of op to the one before, when voters hold
// it as written by op, journals the revert and returns what it did.
func (e *Engine) revert(ctx context.Context, op *dcctlv1.Operation, req AbandonRequest) (map[string]any, error) {
	to := kv.State{
		Generation:       op.GetNewGeneration(),
		ActiveDatacenter: datacenterID(op.GetTarget()),
		OpID:             operationID(op.GetName()),
	}

	view, err := e.cfg.Coordinator.Read(ctx)
	if err != nil {
		return nil, err
	}

	if !holdsWrittenBy(view, to) {
		return map[string]any{"generation": to.Generation, "held": false}, nil
	}

	if committedRecord(view, to) {
		out := map[string]any{"generation": to.Generation, "committed": true}
		detail, result := revertResult(to, kv.State{}, nil, 0, true, nil)
		e.journalOperation(ctx, op, eventRevert, req.Initiator, detail, result, nil, out)

		return out, nil
	}

	from, ok := recordOf(view, op.GetPreviousGeneration())
	if !ok {
		return nil, fmt.Errorf("revert generation %d of %s: no voter holds generation %d to revert to",
			to.Generation, op.GetName(), op.GetPreviousGeneration())
	}

	writes, err := e.cfg.Coordinator.Revert(ctx, from, to, coord.RevertOptions{ConfirmUndetermined: req.Confirmations.Isolation})
	reverted := revertedOn(writes)
	out := map[string]any{
		"generation":    to.Generation,
		"generation_to": from.Generation,
		"reverted":      anySlice(reverted),
	}

	committed := errors.Is(err, coord.ErrCommitted)
	if committed {
		out["committed"] = true
		err = nil
	}

	detail, result := revertResult(to, from, reverted, len(writes), committed, err)
	e.journalOperation(ctx, op, eventRevert, req.Initiator, detail, result, err, out)

	switch {
	case errors.Is(err, coord.ErrConfirmationRequired):
		err = fmt.Errorf("revert generation %d of %s: %w", to.Generation, op.GetName(), err)
		if refused := refusal(preflight.GenerationDetermined(op, view), req.Confirmations); refused != nil {
			return nil, fmt.Errorf("%w: %w", refused, err)
		}

		return nil, err
	case err != nil:
		return nil, fmt.Errorf("revert generation %d of %s: %w", to.Generation, op.GetName(), err)
	}

	return out, nil
}

// revertResult tells in the journal what a revert of to back to from did:
// reverted holds the voters written back, of holders that held to.
func revertResult(to, from kv.State, reverted []string, holders int, committed bool, err error) (string, dcctlv1.JournalEntry_Result) {
	switch {
	case committed:
		return fmt.Sprintf("generation %d is committed and stays", to.Generation), dcctlv1.JournalEntry_WARNING
	case errors.Is(err, coord.ErrConfirmationRequired):
		return fmt.Sprintf("generation %d is not reverted: voters that do not answer may hold it, and the isolation is not confirmed",
			to.Generation), dcctlv1.JournalEntry_WARNING
	case err != nil:
		return fmt.Sprintf("generation %d reverted to %d on %d of %d voters that held it", to.Generation, from.Generation,
			len(reverted), holders), dcctlv1.JournalEntry_ERROR
	case len(reverted) == 0:
		return fmt.Sprintf("generation %d is not reverted: no voter that answers holds it", to.Generation), dcctlv1.JournalEntry_WARNING
	}

	return fmt.Sprintf("generation %d reverted to %d on %s", to.Generation, from.Generation, strings.Join(reverted, ", ")),
		dcctlv1.JournalEntry_OK
}

// committedRecord reports whether the record of to is valid on view, so a
// revert would not write it back.
func committedRecord(view coord.View, to kv.State) bool {
	if view.Effective != nil && coord.KeyOf(view.Effective.State) == coord.KeyOf(to) {
		return true
	}

	return slices.ContainsFunc(view.Conflict, func(r coord.Record) bool { return coord.KeyOf(r.State) == coord.KeyOf(to) })
}

// holdsWrittenBy reports whether a voter holds the record of to written by its
// operation.
func holdsWrittenBy(view coord.View, to kv.State) bool {
	for _, vs := range view.Voters {
		s := vs.State
		if vs.Status == coord.StatusPresent && s.Generation == to.Generation &&
			s.ActiveDatacenter == to.ActiveDatacenter && s.OpID == to.OpID {
			return true
		}
	}

	return false
}

// recordOf returns the record of generation that voters hold: the effective
// one, else one a voter holds.
func recordOf(view coord.View, generation int64) (kv.State, bool) {
	if eff := view.Effective; eff != nil && eff.State.Generation == generation {
		return eff.State, true
	}

	for _, list := range [][]coord.Record{view.Undetermined, view.Uncommitted} {
		for _, r := range list {
			if r.State.Generation == generation {
				return r.State, true
			}
		}
	}

	for _, vs := range view.Voters {
		if vs.Status == coord.StatusPresent && vs.State.Generation == generation {
			return vs.State, true
		}
	}

	return kv.State{}, false
}

func revertedOn(writes []coord.VoterWrite) []string {
	var out []string

	for _, w := range writes {
		if w.Outcome == coord.WriteAccepted {
			out = append(out, w.Voter)
		}
	}

	return out
}
