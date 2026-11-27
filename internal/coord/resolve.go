package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

// ResolveMode returns the mode in which [Coordinator.Resolve] writes to over the
// conflict of view in favor of winner, as [AdvanceMode] does for an advance. The
// errors are those Resolve returns before writing.
func ResolveMode(view View, winner, to kv.State) (WriteMode, error) {
	steps, err := resolveSteps(view, winner, to)
	if err != nil {
		return "", err
	}

	return modeOf(steps, view.Majority), nil
}

// TopGeneration returns the highest generation a voter of view holds, 0 when
// none holds dcctl/state. A resolve writes the generation above it.
func TopGeneration(view View) int64 {
	var top int64

	for _, vs := range view.Voters {
		if vs.Status == StatusPresent {
			top = max(top, vs.State.Generation)
		}
	}

	return top
}

// Resolve writes to over a conflict, the valid records of the highest valid
// generation with different active datacenters: winner is the record of the
// conflict the operator chose, read with a settled payload, and to the record
// that makes its active datacenter active, one generation above every
// generation a voter holds. It is the only write over a conflict; see
// [ErrConflict].
//
// The isolation of the other side is always confirmed, whatever the mode. The
// voters that can take to are those that answer with a record below it and the
// KVs without the key; the mode is [WriteQuorum] when they are a majority,
// [WriteForced] otherwise, and opts.Mode must match it ([ErrModeChanged]).
// Nothing is written when winner is no longer a side of a conflict with that
// payload ([ErrBaseNotFound], [ErrBaseChanged], [ErrUnsettled]) or a voter
// holds the generation of to or above it ([ErrSuperseded]).
//
// The voters of the winning side are written last, and only once every voter
// of another side took to: a write that stops half way never leaves another
// side the only valid record. A voter that holds to, as after an earlier
// Resolve of the same operation, counts as accepted, as in
// [Coordinator.Advance], and the conflict need not stand any more then.
//
// updated_at of to is replaced with the time of the write.
func (c *Coordinator) Resolve(ctx context.Context, winner, to kv.State, opts AdvanceOptions) (WriteResult, error) {
	to.NeedsRecovery = canonicalRecovery(to.NeedsRecovery)
	to.UpdatedAt = time.Now().UTC()

	if err := validateResolve(winner, to, opts); err != nil {
		return WriteResult{}, err
	}

	view, err := c.Read(ctx)
	if err != nil {
		return WriteResult{}, err
	}

	steps, err := resolveSteps(view, winner, to)
	if err != nil {
		return WriteResult{}, err
	}

	if mode := modeOf(steps, view.Majority); mode != opts.Mode {
		return WriteResult{}, fmt.Errorf("%s is a %s write now, not %s: %w", recordName(to), mode, opts.Mode, ErrModeChanged)
	}

	plans, forced, err := stepPlans(view, steps, to)
	if err != nil {
		return WriteResult{}, err
	}

	return c.judge(ctx, "conflict resolved", view, to, opts.Mode, forced, c.writeOtherSideFirst(ctx, view, plans, winner, to))
}

// errOtherSideFirst is why a voter of the winning side is not written: a voter of
// another side may still hold its record, which would then be the only one valid.
var errOtherSideFirst = errors.New("written only once every voter of the other sides holds the new record")

// writeOtherSideFirst writes plans in two rounds: first every voter but those of the
// winning side, then, once every voter of another side of the conflict took the write,
// the winning side. A write that stops between the rounds leaves the winner valid,
// never another side alone.
func (c *Coordinator) writeOtherSideFirst(ctx context.Context, view View, plans []voterPlan, winner, to kv.State) []VoterWrite {
	holds := func(s kv.State) bool { return holdsTarget(s, to) }
	first, second := slices.Clone(plans), slices.Clone(plans)

	for i, vs := range view.Voters {
		if vs.Status == StatusPresent && sameRecord(vs.State, winner) {
			first[i] = voterPlan{skip: errOtherSideFirst}
		} else {
			second[i] = voterPlan{skip: errOtherSideFirst}
		}
	}

	writes := c.execute(ctx, first, holds)

	for i, vs := range view.Voters {
		otherSide := vs.Status == StatusPresent && vs.State.Generation == winner.Generation && !sameRecord(vs.State, winner)
		if otherSide && writes[i].Outcome != WriteAccepted {
			return writes
		}
	}

	for i, w := range c.execute(ctx, second, holds) {
		if second[i].skip == nil {
			writes[i] = w
		}
	}

	return writes
}

func validateResolve(winner, to kv.State, opts AdvanceOptions) error {
	switch opts.Mode {
	case WriteQuorum, WriteForced:
	default:
		return fmt.Errorf("unknown write mode %q", opts.Mode)
	}

	switch {
	case winner.Generation < 1 || winner.ActiveDatacenter == "":
		return errors.New("winner record needs a generation and an active datacenter")
	case to.Generation <= winner.Generation:
		return fmt.Errorf("generation %d is not above the conflict of generation %d", to.Generation, winner.Generation)
	case to.Revision != 0:
		return fmt.Errorf("a new generation has revision 0, not %d", to.Revision)
	case to.ActiveDatacenter != winner.ActiveDatacenter || to.OpID == "":
		return fmt.Errorf("new record needs the active datacenter of the winner, %s, and an operation", winner.ActiveDatacenter)
	case to.Forced != (opts.Mode == WriteForced):
		return fmt.Errorf("forced is %t in a %s write", to.Forced, opts.Mode)
	case !opts.IsolationConfirmed:
		return fmt.Errorf("resolving the conflict in favor of %s needs the isolation of the other side confirmed: %w",
			winner.ActiveDatacenter, ErrConfirmationRequired)
	}

	return validateRecovery(to.NeedsRecovery, to.ActiveDatacenter)
}

// resolveSteps returns the step of every voter of view for writing to over the
// conflict in favor of winner, or why nothing may be written.
func resolveSteps(view View, winner, to kv.State) ([]advanceStep, error) {
	resumed := false

	for _, vs := range view.Voters {
		resumed = resumed || vs.Status == StatusPresent && holdsRecordOf(vs.State, to)
	}

	base := findRecord(winner, view.Conflict)

	switch {
	case base == nil && resumed:
	case base == nil:
		return nil, fmt.Errorf("%s is not a side of a conflict: %w", recordName(winner), ErrBaseNotFound)
	case !base.Settled:
		return nil, fmt.Errorf("%s: %w", recordName(winner), ErrUnsettled)
	case !samePayload(base.State, winner):
		return nil, fmt.Errorf("%s is at revision %d: %w", recordName(winner), base.State.Revision, ErrBaseChanged)
	}

	steps := make([]advanceStep, len(view.Voters))

	for i, vs := range view.Voters {
		switch vs.Status {
		case StatusPresent:
			s := vs.State

			switch {
			case holdsRecordOf(s, to) && s.Revision == 0 && !sameTarget(s, to):
				return nil, fmt.Errorf("voter %s holds %s of the operation with another payload, resolve with it: %w",
					vs.Name, recordName(s), ErrBaseChanged)
			case holdsRecordOf(s, to) && (s.Forced || !to.Forced):
				steps[i] = stepHeld
			case holdsRecordOf(s, to):
				steps[i] = stepForce
			case s.Generation < to.Generation:
				steps[i] = stepWrite
			default:
				return nil, fmt.Errorf("voter %s holds %s: %w", vs.Name, recordName(s), ErrSuperseded)
			}
		case StatusAbsent:
			if vs.Kind == KindKV {
				steps[i] = stepWrite
			}
		case StatusUninitialized, StatusUnreachable, StatusInvalid, StatusStale:
		}
	}

	if !resumed {
		if top := TopGeneration(view); to.Generation != top+1 {
			return nil, fmt.Errorf("generation %d is not the one above generation %d a voter holds: %w", to.Generation, top, ErrSuperseded)
		}
	}

	return steps, nil
}
