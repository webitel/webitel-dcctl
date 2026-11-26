package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

var (
	// ErrModeChanged means the voters no longer allow the mode the caller
	// decided on. Nothing is written; decide again on a new view.
	ErrModeChanged = errors.New("write mode changed")
	// ErrBaseNotFound means no voter holds the base record as the effective
	// or an undetermined one.
	ErrBaseNotFound = errors.New("base record not found")
	// ErrBaseChanged means the base record holds another payload than the
	// one the new record was built from.
	ErrBaseChanged = errors.New("base record changed")
	// ErrSuperseded means a voter holds another record of the base
	// generation or above it: another instance wrote it.
	ErrSuperseded = errors.New("voter holds a newer record")
)

// AdvanceOptions is how [Coordinator.Advance] writes.
type AdvanceOptions struct {
	// Mode is the mode the caller decided on with [AdvanceMode].
	Mode WriteMode
	// IsolationConfirmed confirms that the old active datacenter is
	// isolated. A forced write needs it.
	IsolationConfirmed bool
}

// advanceStep is what [Coordinator.Advance] does on one voter.
type advanceStep int

const (
	stepSkip advanceStep = iota
	// stepWrite writes the new record over the voter.
	stepWrite
	// stepHeld is a voter that holds the new record.
	stepHeld
	// stepForce is a voter that holds the new record without forced, which
	// a forced write sets within its generation.
	stepForce
)

// AdvanceMode returns the mode in which [Coordinator.Advance] writes to over
// the base record from on the voters of view. A preflight fills its mode
// from it on the view it reads; Advance decides the mode again on its own
// read and fails with [ErrModeChanged] when it differs.
//
// The voters that can take the write are those that hold the record of from
// at any revision, those behind it (a lower generation, a KV without the
// key, or another record of its generation that [Evaluate] marks stale) and
// those that hold to already. The mode is [WriteQuorum] when they
// are a majority of all voters, [WriteForced] otherwise. [View.Quorum] is
// not used: a voter that answers with another record cannot take the write.
// The errors are those Advance returns on the base before writing.
func AdvanceMode(view View, from, to kv.State) (WriteMode, error) {
	steps, err := advanceSteps(view, from, to)
	if err != nil {
		return "", err
	}

	return modeOf(steps, view.Majority), nil
}

// Advance writes to, the generation after from, on the voters in parallel,
// each by a CAS at the version of a read made right before. It returns
// which voters accepted it and whether it is committed; a record that is
// not comes with [ErrUncommitted] or [ErrUndetermined] and the result.
//
// The base from is the effective record, or for a failover the highest
// undetermined one, read with a settled payload: to is built from it.
// Nothing is written when the voters no longer hold it with that payload
// ([ErrBaseNotFound], [ErrBaseChanged], [ErrUnsettled]), when a voter holds
// another record of its generation or above ([ErrSuperseded]), when the
// highest generation is in conflict ([ErrConflict]), when the mode differs
// from opts.Mode ([ErrModeChanged]), or when a forced write has no isolation
// confirmation ([ErrConfirmationRequired]). to must have revision 0 and
// forced set exactly in [WriteForced].
//
// A voter is written when it holds the base record or is behind it, as
// [AdvanceMode] tells; a KV without the key gets it created, an arbiter
// never. A voter that holds to, as after an earlier Advance of the same
// operation, counts as accepted; in a forced write it gets forced set within
// the generation if it lacks it. Such a voter at revision 0 must hold the
// payload of to but for forced, or nothing is written ([ErrBaseChanged]):
// the voters would hold two payloads of one revision. To finish such a
// write, pass the record the voters hold as to. A voter whose outcome is
// unknown is not written again: the next read tells.
//
// updated_at of to is replaced with the time of the write.
func (c *Coordinator) Advance(ctx context.Context, from, to kv.State, opts AdvanceOptions) (WriteResult, error) {
	to.NeedsRecovery = canonicalRecovery(to.NeedsRecovery)
	to.UpdatedAt = time.Now().UTC()

	if err := validateAdvance(from, to, opts); err != nil {
		return WriteResult{}, err
	}

	view, err := c.Read(ctx)
	if err != nil {
		return WriteResult{}, err
	}

	steps, err := advanceSteps(view, from, to)
	if err != nil {
		return WriteResult{}, err
	}

	if mode := modeOf(steps, view.Majority); mode != opts.Mode {
		return WriteResult{}, fmt.Errorf("%s is a %s write now, not %s: %w", recordName(to), mode, opts.Mode, ErrModeChanged)
	}

	value, err := kv.EncodeState(to, nil)
	if err != nil {
		return WriteResult{}, err
	}

	plans := make([]voterPlan, len(steps))
	forced := opts.Mode == WriteForced

	for i, step := range steps {
		vs := view.Voters[i]

		switch step {
		case stepWrite:
			plans[i] = voterPlan{value: value, version: vs.Version}
		case stepHeld:
			plans[i] = voterPlan{held: true}
			forced = forced || vs.State.Forced
		case stepForce:
			s := vs.State
			s.Forced, s.Revision, s.UpdatedAt = true, s.Revision+1, to.UpdatedAt

			b, err := kv.EncodeState(s, vs.Raw)
			if err != nil {
				return WriteResult{}, fmt.Errorf("voter %s: %w", vs.Name, err)
			}

			plans[i] = voterPlan{value: b, version: vs.Version}
		case stepSkip:
			plans[i] = skipped(vs)
		}
	}

	res := WriteResult{Mode: opts.Mode, Majority: view.Majority}
	res.Voters = c.execute(ctx, plans, func(s kv.State) bool { return holdsTarget(s, to) })
	res.Commitment = commitment(res.Voters, view, forced)

	c.logWrite(ctx, "generation advanced", to, res)

	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("write %s: %w", recordName(to), err)
	}

	return res, resultError(res, recordName(to))
}

func validateAdvance(from, to kv.State, opts AdvanceOptions) error {
	switch opts.Mode {
	case WriteQuorum, WriteForced:
	default:
		return fmt.Errorf("unknown write mode %q", opts.Mode)
	}

	switch {
	case from.Generation < 1 || from.ActiveDatacenter == "":
		return errors.New("base record needs a generation and an active datacenter")
	case to.Generation != from.Generation+1:
		return fmt.Errorf("generation %d does not follow %d", to.Generation, from.Generation)
	case to.Revision != 0:
		return fmt.Errorf("a new generation has revision 0, not %d", to.Revision)
	case to.ActiveDatacenter == "" || to.OpID == "":
		return errors.New("new record needs an active datacenter and an operation")
	case to.Forced != (opts.Mode == WriteForced):
		return fmt.Errorf("forced is %t in a %s write", to.Forced, opts.Mode)
	case opts.Mode == WriteForced && !opts.IsolationConfirmed:
		return fmt.Errorf("forced %s needs the isolation of the old active datacenter confirmed: %w",
			recordName(to), ErrConfirmationRequired)
	}

	return validateRecovery(to.NeedsRecovery, to.ActiveDatacenter)
}

// advanceSteps returns the step of every voter of view for writing to over
// from, or why nothing may be written.
func advanceSteps(view View, from, to kv.State) ([]advanceStep, error) {
	if len(view.Conflict) > 0 {
		return nil, fmt.Errorf("generation %d: %w", view.Conflict[0].State.Generation, ErrConflict)
	}

	base := findRecord(from, effectiveList(view), view.Undetermined)
	resumed := slices.ContainsFunc(view.Voters, func(vs VoterState) bool {
		return vs.Status == StatusPresent && holdsRecordOf(vs.State, to)
	})

	switch {
	case base == nil && resumed:
		// The voters that held the base took to before; it is not built
		// again.
	case base == nil:
		return nil, fmt.Errorf("%s is neither effective nor undetermined: %w", recordName(from), ErrBaseNotFound)
	case !base.Settled:
		return nil, fmt.Errorf("%s: %w", recordName(from), ErrUnsettled)
	case !samePayload(base.State, from):
		return nil, fmt.Errorf("%s is at revision %d: %w", recordName(from), base.State.Revision, ErrBaseChanged)
	}

	steps := make([]advanceStep, len(view.Voters))

	for i, vs := range view.Voters {
		switch vs.Status {
		case StatusPresent:
			s := vs.State

			switch {
			case holdsRecordOf(s, to) && s.Revision == 0 && !sameTarget(s, to):
				return nil, fmt.Errorf("voter %s holds %s of the operation with another payload, advance with it: %w",
					vs.Name, recordName(s), ErrBaseChanged)
			case holdsRecordOf(s, to) && (s.Forced || !to.Forced):
				steps[i] = stepHeld
			case holdsRecordOf(s, to):
				steps[i] = stepForce
			case s.Generation < from.Generation, sameRecord(s, from), vs.Stale:
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

	return steps, nil
}

func modeOf(steps []advanceStep, majority int) WriteMode {
	writable := len(steps) - countSteps(steps, stepSkip)
	if writable >= majority {
		return WriteQuorum
	}

	return WriteForced
}

func countSteps(steps []advanceStep, step advanceStep) int {
	n := 0

	for _, s := range steps {
		if s == step {
			n++
		}
	}

	return n
}

// holdsRecordOf reports whether s is the record of to written by the same
// operation, at any revision.
func holdsRecordOf(s, to kv.State) bool {
	return sameRecord(s, to) && s.OpID == to.OpID
}

// sameTarget reports whether s, written by an earlier advance, has the
// payload of to, but for forced, which a forced advance sets within the
// generation.
func sameTarget(s, to kv.State) bool {
	s.Forced = to.Forced

	return samePayload(s, to)
}

// holdsTarget reports whether s holds what [Coordinator.Advance] writes for
// to.
func holdsTarget(s, to kv.State) bool {
	return holdsRecordOf(s, to) && (s.Forced || !to.Forced)
}

func (c *Coordinator) logWrite(ctx context.Context, msg string, s kv.State, res WriteResult) {
	attrs := []logging.Field{
		logging.Int64("generation", s.Generation),
		logging.Str("active_datacenter", s.ActiveDatacenter),
		logging.Str("mode", string(res.Mode)),
		logging.Str("commitment", string(res.Commitment)),
		logging.Int("accepted", len(res.Accepted())),
		logging.Int("majority", res.Majority),
	}

	if res.Commitment == Committed && !slices.ContainsFunc(res.Voters, func(w VoterWrite) bool {
		return w.Outcome == WriteUnknown
	}) {
		c.log.InfoCtx(ctx, msg, attrs...)

		return
	}

	c.log.WarnCtx(ctx, msg, attrs...)
}
