package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

var (
	// ErrCommitted means the record to revert is valid: on a majority of
	// voters or forced. Nothing is written.
	ErrCommitted = errors.New("record is committed")
	// ErrRevertIncomplete means some voters that hold the reverted record
	// were not written; the result tells which.
	ErrRevertIncomplete = errors.New("record not reverted on every voter that holds it")
)

// RevertOptions is how [Coordinator.Revert] reverts.
type RevertOptions struct {
	// ConfirmUndetermined allows reverting a record that the voters that
	// do not answer may hold too, which would make it committed.
	ConfirmUndetermined bool
}

// Revert writes from back over to, the generation after it, on the voters
// that hold to as written by its operation, after the operation that wrote
// it was abandoned. It returns what happened on each of them; when one was
// not reverted, the result comes with [ErrRevertIncomplete]. Any instance
// may run it.
//
// to must be uncommitted: on fewer voters than a majority even if every
// voter that does not answer, and every KV that answers without the key,
// held it. A KV without the key counts here, unlike in [Evaluate], as it
// may have taken to and lost its key since. A record those voters would make
// committed is reverted only with opts.ConfirmUndetermined, otherwise
// [ErrConfirmationRequired]; a valid one never ([ErrCommitted]).
// Nothing is written then. A record above which the voters moved on, or
// that no voter that answers holds, is not reverted and gives an empty
// result.
//
// The value written is the base record as a voter holds it, with its
// revision and the fields this dcctl does not know, when one does and its
// payload is settled; otherwise from as given. Each write is a CAS at the
// version just read.
func (c *Coordinator) Revert(ctx context.Context, from, to kv.State, opts RevertOptions) ([]VoterWrite, error) {
	switch {
	case from.Generation < 1 || from.ActiveDatacenter == "":
		return nil, errors.New("base record needs a generation and an active datacenter")
	case to.Generation != from.Generation+1:
		return nil, fmt.Errorf("generation %d does not follow %d", to.Generation, from.Generation)
	case to.ActiveDatacenter == "" || to.OpID == "":
		return nil, errors.New("reverted record needs an active datacenter and an operation")
	}

	view, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}

	uncommitted := findRecord(to, view.Uncommitted)

	switch {
	case findRecord(to, effectiveList(view), view.Conflict) != nil:
		return nil, fmt.Errorf("%s: %w", recordName(to), ErrCommitted)
	case findRecord(to, view.Undetermined) != nil, uncommitted != nil && mayHaveTaken(view, *uncommitted):
		if !opts.ConfirmUndetermined {
			return nil, fmt.Errorf("%s may be committed on voters that do not answer or lost the key: %w",
				recordName(to), ErrConfirmationRequired)
		}
	case uncommitted == nil:
		return nil, nil
	}

	value, err := revertValue(view, from)
	if err != nil {
		return nil, err
	}

	plans := make([]voterPlan, len(view.Voters))
	holders := make([]bool, len(view.Voters))

	for i, vs := range view.Voters {
		if vs.Status == StatusPresent && holdsRecordOf(vs.State, to) {
			plans[i], holders[i] = voterPlan{value: value, version: vs.Version}, true
		} else {
			plans[i] = skipped(vs)
		}
	}

	writes := c.execute(ctx, plans, func(s kv.State) bool { return sameRecord(s, from) })

	var (
		out      []VoterWrite
		reverted = 0
	)

	for i, w := range writes {
		if !holders[i] {
			continue
		}

		out = append(out, w)

		if w.Outcome == WriteAccepted {
			reverted++
		}
	}

	c.log.InfoCtx(ctx, "generation reverted",
		logging.Int64("generation", to.Generation), logging.Str("active_datacenter", to.ActiveDatacenter),
		logging.Int64("generation_to", from.Generation), logging.Int("holders", len(out)),
		logging.Int("reverted", reverted))

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("revert %s: %w", recordName(to), err)
	}

	if reverted < len(out) {
		return out, fmt.Errorf("%s reverted on %d of %d voters: %w", recordName(to), reverted, len(out), ErrRevertIncomplete)
	}

	return out, nil
}

// mayHaveTaken reports whether r may be on a majority counting, besides the
// voters that may hold any record, the KVs without the key: such a KV may
// have taken r and lost its key since.
func mayHaveTaken(view View, r Record) bool {
	n := len(r.Voters)

	for _, vs := range view.Voters {
		if vs.Status.Unknown() || vs.Status == StatusAbsent && vs.Kind == KindKV {
			n++
		}
	}

	return n >= view.Majority
}

// revertValue returns what [Coordinator.Revert] writes back for from.
func revertValue(view View, from kv.State) ([]byte, error) {
	base := findRecord(from, effectiveList(view), view.Undetermined, view.Uncommitted)

	switch {
	case base == nil:
		return kv.EncodeState(from, nil)
	case !base.Settled:
		return nil, fmt.Errorf("%s: %w", recordName(from), ErrUnsettled)
	}

	return base.Raw, nil
}
