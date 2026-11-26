package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

var (
	// ErrConfirmationRequired means the write needs a confirmation the
	// caller did not pass. Nothing is written.
	ErrConfirmationRequired = errors.New("confirmation required")
	// ErrConflict means the highest valid generation has conflicting
	// records; only resolving the conflict writes over it.
	ErrConflict = errors.New("records of the highest valid generation conflict")
	// ErrUncommitted means the record is on fewer voters than it needs and
	// cannot reach them even if every voter that did not answer took it.
	ErrUncommitted = errors.New("record is not committed")
	// ErrUndetermined means the record may be committed: the voters whose
	// write outcome is unknown, or that did not answer, decide.
	ErrUndetermined = errors.New("record may not be committed")
)

// WriteMode is how a generation is written.
type WriteMode string

const (
	// WriteQuorum writes on voters that are a majority; the record is not
	// forced.
	WriteQuorum WriteMode = "quorum"
	// WriteForced writes on voters that are not a majority; the record is
	// forced and the caller confirms that the old active datacenter is
	// isolated.
	WriteForced WriteMode = "forced"
)

// WriteOutcome is what a write did on one voter.
type WriteOutcome string

const (
	// WriteAccepted means the voter holds what was written: it took the
	// write, or it was found holding it.
	WriteAccepted WriteOutcome = "accepted"
	// WriteRejected means the voter changed since it was read and holds
	// something else.
	WriteRejected WriteOutcome = "rejected"
	// WriteUnknown means the write may have been applied; the next read
	// tells.
	WriteUnknown WriteOutcome = "unknown"
	WriteFailed  WriteOutcome = "failed"
	// WriteSkipped means the voter was not written: it did not answer, its
	// read is stale, its record cannot be decoded, it is an arbiter without
	// state, or it holds no record the write applies to. Err tells which.
	WriteSkipped WriteOutcome = "skipped"
)

// VoterWrite is the result of a write on one voter.
type VoterWrite struct {
	Voter   string
	Outcome WriteOutcome
	Err     error
}

// Commitment is whether a written record is valid.
type Commitment string

const (
	// Committed is a record on a majority of voters, or a forced one.
	Committed Commitment = "committed"
	// Uncommitted is a record that cannot be committed even if every voter
	// that did not answer, or whose outcome is unknown, took it.
	Uncommitted Commitment = "uncommitted"
	// Undetermined is a record that the voters whose outcome is unknown, or
	// that did not answer, would make committed.
	Undetermined Commitment = "undetermined"
)

// WriteResult is the result of writing dcctl/state to every voter.
type WriteResult struct {
	Mode WriteMode
	// Voters is in the order of the configured voters.
	Voters     []VoterWrite
	Majority   int
	Commitment Commitment
}

// Accepted returns the voters that accepted the write, in the order of the
// configured voters.
func (r WriteResult) Accepted() []string {
	var out []string

	for _, w := range r.Voters {
		if w.Outcome == WriteAccepted {
			out = append(out, w.Voter)
		}
	}

	return out
}

// voterPlan is what a write does on one voter. Without value the voter is
// accepted when held is set and skipped with skip otherwise.
type voterPlan struct {
	value   []byte
	version Version
	held    bool
	skip    error
}

// skipped returns the plan of a voter that is not written, with why.
func skipped(vs VoterState) voterPlan {
	if vs.Err != nil {
		return voterPlan{skip: vs.Err}
	}

	return voterPlan{skip: fmt.Errorf("voter %s is %s", vs.Name, vs.Status)}
}

// execute writes plans, one per voter, in parallel. holds tells whether a
// voter whose CAS failed on the version holds what was written anyway, as
// when another instance aligned it in between; it is read once for that,
// and never written again.
func (c *Coordinator) execute(ctx context.Context, plans []voterPlan, holds func(kv.State) bool) []VoterWrite {
	out := make([]VoterWrite, len(c.voters))

	var wg sync.WaitGroup

	for i, v := range c.voters {
		p := plans[i]
		out[i].Voter = v.Name()

		switch {
		case p.value != nil:
			wg.Go(func() { out[i] = c.writeVoter(ctx, v, p, holds) })
		case p.held:
			out[i].Outcome = WriteAccepted
		default:
			out[i].Outcome, out[i].Err = WriteSkipped, p.skip
		}
	}

	wg.Wait()

	return out
}

func (c *Coordinator) writeVoter(ctx context.Context, v Voter, p voterPlan, holds func(kv.State) bool) VoterWrite {
	w := VoterWrite{Voter: v.Name()}

	casCtx, cancel := context.WithTimeout(ctx, c.timeout)
	err := v.CAS(casCtx, KeyState, p.value, p.version)

	cancel()

	switch {
	case err == nil:
		w.Outcome = WriteAccepted
	case errors.Is(err, ErrVersionMismatch):
		if fresh := c.readState(ctx, v); fresh.Status == StatusPresent && holds(fresh.State) {
			w.Outcome = WriteAccepted
		} else {
			w.Outcome, w.Err = WriteRejected, err
		}
	case errors.Is(err, ErrOutcomeUnknown):
		w.Outcome, w.Err = WriteUnknown, err
	default:
		w.Outcome, w.Err = WriteFailed, err
	}

	return w
}

// commitment judges the record that writes put on the voters of view as
// [Evaluate] would: a voter whose outcome is unknown, or that may hold any
// record when read, may hold it too. A forced record is valid on any voter.
func commitment(writes []VoterWrite, view View, forced bool) Commitment {
	accepted, unknown := 0, 0

	for i, w := range writes {
		switch {
		case w.Outcome == WriteAccepted:
			accepted++
		case w.Outcome == WriteUnknown, view.Voters[i].Status.Unknown():
			unknown++
		}
	}

	need := view.Majority
	if forced {
		need = 1
	}

	switch {
	case accepted >= need:
		return Committed
	case accepted+unknown >= need:
		return Undetermined
	}

	return Uncommitted
}

// resultError returns the error of a write of record that ended with res,
// nil when it is committed.
func resultError(res WriteResult, record string) error {
	n := len(res.Accepted())

	switch res.Commitment {
	case Uncommitted:
		return fmt.Errorf("%s on %d of %d voters: %w", record, n, len(res.Voters), ErrUncommitted)
	case Undetermined:
		return fmt.Errorf("%s on %d of %d voters, the rest unknown: %w", record, n, len(res.Voters), ErrUndetermined)
	case Committed:
	}

	return nil
}

// canonicalRecovery returns needs_recovery sorted and without duplicates,
// as it is compared between voters.
func canonicalRecovery(dcs []string) []string {
	if len(dcs) == 0 {
		return nil
	}

	return slices.Compact(slices.Sorted(slices.Values(dcs)))
}

// validateRecovery checks the datacenters waiting for recovery of a record
// that makes active active.
func validateRecovery(dcs []string, active string) error {
	for _, dc := range dcs {
		switch dc {
		case "":
			return errors.New("needs_recovery has an empty datacenter")
		case active:
			return fmt.Errorf("active datacenter %s cannot wait for recovery", active)
		}
	}

	return nil
}

// recordName names the record of s in errors and logs.
func recordName(s kv.State) string {
	return fmt.Sprintf("%s generation %d of %s", kv.StateKey, s.Generation, s.ActiveDatacenter)
}

// findRecord returns the record of s among rs, nil when there is none.
func findRecord(s kv.State, rs ...[]Record) *Record {
	for _, list := range rs {
		if i := slices.IndexFunc(list, func(r Record) bool { return sameRecord(r.State, s) }); i >= 0 {
			return &list[i]
		}
	}

	return nil
}

// effectiveList returns the effective record of v as a list, for [findRecord].
func effectiveList(v View) []Record {
	if v.Effective == nil {
		return nil
	}

	return []Record{*v.Effective}
}
