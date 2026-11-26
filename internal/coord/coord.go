package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// ErrUnsettled is the error of [AlignSkipped] when the effective record is
// not [Record.Settled]: an update within its generation has not reached
// enough voters to tell which payload to copy.
var ErrUnsettled = errors.New("update within the effective generation is not finished")

// AlignOutcome is what aligning one stale voter did.
type AlignOutcome string

const (
	AlignWritten AlignOutcome = "written"
	// AlignSkipped means the voter was not written. Err tells why:
	// [ErrUnsettled], [ErrUninitialized] for an arbiter found without
	// state, the decoding error of an invalid record, or nil when the voter
	// caught up.
	AlignSkipped AlignOutcome = "skipped"
	// AlignMismatch means the voter changed between the re-read and the
	// write.
	AlignMismatch AlignOutcome = "mismatch"
	// AlignUnknown means the write may have been applied; the next read
	// tells.
	AlignUnknown AlignOutcome = "unknown"
	AlignFailed  AlignOutcome = "failed"
)

// Alignment is the result of aligning one stale voter.
type Alignment struct {
	Voter   string
	Outcome AlignOutcome
	Err     error
}

// Coordinator reads and writes the shared state on the voters.
type Coordinator struct {
	voters  []Voter
	timeout time.Duration
	log     *logging.Logger
}

// New returns the coordinator of voters, which must be every configured
// voter: the majority is counted from them. timeout bounds each call to a
// voter.
func New(voters []Voter, timeout time.Duration, log *logging.Logger) (*Coordinator, error) {
	switch {
	case len(voters) == 0:
		return nil, errors.New("no voters")
	case timeout <= 0:
		return nil, errors.New("voter timeout must be positive")
	}

	names := make(map[string]bool, len(voters))

	for i, v := range voters {
		if v == nil {
			return nil, fmt.Errorf("voter %d is nil", i)
		}

		if names[v.Name()] {
			return nil, fmt.Errorf("duplicate voter %q", v.Name())
		}

		names[v.Name()] = true
	}

	return &Coordinator{voters: voters, timeout: timeout, log: log}, nil
}

// Read reads dcctl/state from every voter in parallel and evaluates it. It
// fails only when ctx ends, since a view of the voters read so far would
// count the rest as unreachable.
func (c *Coordinator) Read(ctx context.Context) (View, error) {
	states := make([]VoterState, len(c.voters))

	var wg sync.WaitGroup

	for i, v := range c.voters {
		wg.Go(func() { states[i] = c.readState(ctx, v) })
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		return View{}, fmt.Errorf("read voters: %w", err)
	}

	return Evaluate(states), nil
}

func (c *Coordinator) readState(ctx context.Context, v Voter) VoterState {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	vs := VoterState{Name: v.Name(), Kind: v.Kind()}

	e, err := v.Get(ctx, KeyState)

	switch {
	case errors.Is(err, ErrNotFound):
		vs.Status = StatusAbsent
	case errors.Is(err, ErrUninitialized):
		vs.Status, vs.Err = StatusUninitialized, err
	case err != nil:
		vs.Status, vs.Err = StatusUnreachable, err
		vs.Reason, _ = source.ReasonOf(err)
	default:
		return PresentState(vs.Name, vs.Kind, e.Value, e.Version)
	}

	return vs
}

// PresentState returns the state of a voter that holds dcctl/state as value at
// version: [StatusPresent], or [StatusInvalid] when the record cannot be decoded.
// It is for a record read without [Coordinator.Read].
func PresentState(name string, kind Kind, value []byte, version Version) VoterState {
	vs := VoterState{Name: name, Kind: kind, Raw: value, Version: version}

	if err := decodeState(value, &vs.State); err != nil {
		vs.Status, vs.Reason, vs.Err = StatusInvalid, source.ReasonInvalid, err
	} else {
		vs.Status = StatusPresent
	}

	return vs
}

func decodeState(b []byte, s *kv.State) error {
	if err := json.Unmarshal(b, s); err != nil {
		return fmt.Errorf("decode %s: %w", kv.StateKey, err)
	}

	switch {
	case s.ActiveDatacenter == "":
		return fmt.Errorf("%s has no active datacenter", kv.StateKey)
	case s.Revision < 0:
		return fmt.Errorf("%s has negative revision %d", kv.StateKey, s.Revision)
	}

	return nil
}

// Align writes the effective state of view to its stale voters and to the
// voters behind it within its generation, in parallel, and returns what
// happened on each. It does nothing without an
// effective state, so neither an uncommitted record nor a side of a
// conflict is ever propagated, and writes nothing while the effective
// record is not settled.
//
// Every voter is read again right before its write, and the write uses the
// version of that read, never one from view. The voter is written only when
// what it holds then may be written over by the rules the view gave it:
// a record or payload committed since the view is kept. The value written is the raw
// record of the effective state, so fields this version of dcctl does not
// know are kept. A KV without the key gets it created; an arbiter is never
// created or initialized. Align fails only when view is not of these
// voters or ctx ends.
func (c *Coordinator) Align(ctx context.Context, view View) ([]Alignment, error) {
	if len(view.Voters) != len(c.voters) {
		return nil, fmt.Errorf("view has %d voters, coordinator %d", len(view.Voters), len(c.voters))
	}

	for i, vs := range view.Voters {
		if vs.Name != c.voters[i].Name() {
			return nil, fmt.Errorf("view voter %q is not voter %q", vs.Name, c.voters[i].Name())
		}
	}

	if view.Effective == nil || len(view.Conflict) > 0 {
		return nil, nil
	}

	results := make([]*Alignment, len(c.voters))
	rules := view.overwrite()

	var wg sync.WaitGroup

	for i, vs := range view.Voters {
		if !vs.Stale && !vs.Behind {
			continue
		}

		if !view.Effective.Settled {
			results[i] = &Alignment{Voter: vs.Name, Outcome: AlignSkipped, Err: ErrUnsettled}

			continue
		}

		wg.Go(func() {
			a := c.alignVoter(ctx, c.voters[i], view.Effective, rules)
			results[i] = &a
		})
	}

	wg.Wait()

	var out []Alignment

	for _, a := range results {
		if a != nil {
			out = append(out, *a)
		}
	}

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("align voters: %w", err)
	}

	return out, nil
}

// alignVoter writes effective to v when v, read again, holds what rules
// allow to write over.
func (c *Coordinator) alignVoter(ctx context.Context, v Voter, effective *Record, rules overwrite) Alignment {
	a := Alignment{Voter: v.Name()}
	fresh := c.readState(ctx, v)

	var version Version

	switch fresh.Status {
	case StatusPresent:
		if !rules.allows(fresh.State) {
			a.Outcome = AlignSkipped

			return a
		}

		version = fresh.Version
	case StatusAbsent:
		if v.Kind() != KindKV {
			a.Outcome = AlignSkipped

			return a
		}
	case StatusUninitialized, StatusInvalid:
		a.Outcome, a.Err = AlignSkipped, fresh.Err

		return a
	case StatusUnreachable:
		a.Outcome, a.Err = AlignFailed, fresh.Err

		return a
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	a.Outcome, a.Err = alignOutcome(v.CAS(ctx, KeyState, effective.Raw, version))

	switch a.Outcome {
	case AlignWritten:
		c.log.InfoCtx(ctx, "voter aligned",
			logging.Str("voter", v.Name()),
			logging.Int64("generation_from", fresh.State.Generation),
			logging.Int64("revision_from", fresh.State.Revision),
			logging.Int64("generation", effective.State.Generation),
			logging.Int64("revision", effective.State.Revision))
	case AlignUnknown:
		c.log.WarnCtx(ctx, "voter alignment outcome is unknown",
			logging.Str("voter", v.Name()), logging.Err(a.Err))
	case AlignSkipped, AlignMismatch, AlignFailed:
	}

	return a
}

// alignOutcome returns the outcome of a CAS that copies a record to a voter.
func alignOutcome(err error) (AlignOutcome, error) {
	switch {
	case err == nil:
		return AlignWritten, nil
	case errors.Is(err, ErrVersionMismatch):
		return AlignMismatch, err
	case errors.Is(err, ErrOutcomeUnknown):
		return AlignUnknown, err
	}

	return AlignFailed, err
}
