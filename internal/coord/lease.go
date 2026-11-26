package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// leaseWriteAttempts bounds the re-reads of one voter whose lease keeps
// changing or whose write has an unknown outcome.
const leaseWriteAttempts = 3

var (
	// ErrNotLeaseHolder means the effective state names another datacenter
	// or a higher generation than the holder: the lease is not its to write.
	ErrNotLeaseHolder = errors.New("not the holder of the effective state")
	// ErrLeaseHolderUndetermined means the voters do not tell whether the
	// holder is in effect: there is no effective state, or it is below the
	// generation of the holder. Nothing is written.
	ErrLeaseHolderUndetermined = errors.New("effective holder is undetermined")
	// ErrLeaseMinority means the lease was written on fewer than a majority
	// of voters.
	ErrLeaseMinority = errors.New("lease written on fewer than a majority of voters")
)

// VoterLease is the result of reading dcctl/lease from one voter. Status
// has the meaning it has for dcctl/state; Lease and Version are set for
// [StatusPresent]. A read that may not be current, such as one from a
// Consul that answered without a known leader, is passed as
// [StatusStale]: it may hold an older lease than the voter does.
type VoterLease struct {
	Name   string
	Kind   Kind
	Status Status
	// Reason is why the voter is unreachable or its record invalid.
	Reason  source.Reason
	Err     error
	Lease   kv.Lease
	Version Version
}

// PresentLease returns the lease value read from a voter at version. A
// value that cannot be decoded or names no holder is [StatusInvalid].
func PresentLease(name string, kind Kind, value []byte, version Version) VoterLease {
	vl := VoterLease{Name: name, Kind: kind, Status: StatusPresent, Version: version}

	if err := decodeLease(value, &vl.Lease); err != nil {
		vl.Status, vl.Reason, vl.Err, vl.Lease = StatusInvalid, source.ReasonInvalid, err, kv.Lease{}
	}

	return vl
}

func decodeLease(b []byte, l *kv.Lease) error {
	if err := json.Unmarshal(b, l); err != nil {
		return fmt.Errorf("decode %s: %w", kv.LeaseKey, err)
	}

	if l.Holder == "" {
		return fmt.Errorf("%s has no holder", kv.LeaseKey)
	}

	return nil
}

func (c *Coordinator) readLease(ctx context.Context, v Voter) VoterLease {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	e, err := v.Get(ctx, KeyLease)

	switch {
	case errors.Is(err, ErrNotFound):
		return VoterLease{Name: v.Name(), Kind: v.Kind(), Status: StatusAbsent}
	case errors.Is(err, ErrUninitialized):
		return VoterLease{Name: v.Name(), Kind: v.Kind(), Status: StatusUninitialized, Err: err}
	case err != nil:
		reason, _ := source.ReasonOf(err)

		return VoterLease{Name: v.Name(), Kind: v.Kind(), Status: StatusUnreachable, Reason: reason, Err: err}
	}

	return PresentLease(v.Name(), v.Kind(), e.Value, e.Version)
}

// LeaseHolder is the datacenter that holds the lease at the generation of
// its effective state.
type LeaseHolder struct {
	Datacenter string
	Generation int64
	// Fencing is set when the holder has fencing rules enabled.
	Fencing bool
}

func (h LeaseHolder) validate() error {
	switch {
	case h.Datacenter == "":
		return errors.New("lease holder needs a datacenter")
	case h.Generation < 1:
		return fmt.Errorf("lease holder generation %d must be positive", h.Generation)
	}

	return nil
}

// LeaseWriteOutcome is what writing the lease to one voter did.
type LeaseWriteOutcome string

const (
	LeaseWriteDone LeaseWriteOutcome = "written"
	// LeaseWriteSuperseded means the voter holds the lease of a higher
	// generation, or of another holder at the same generation, and was not
	// written.
	LeaseWriteSuperseded LeaseWriteOutcome = "superseded"
	// LeaseWriteSkipped means the voter cannot take the lease: an arbiter
	// without state or without the record, or a record that cannot be
	// decoded.
	LeaseWriteSkipped LeaseWriteOutcome = "skipped"
	// LeaseWriteMismatch means the lease kept changing between the re-read
	// and the write.
	LeaseWriteMismatch LeaseWriteOutcome = "mismatch"
	// LeaseWriteUnknown means the last write may have been applied; the next
	// read tells.
	LeaseWriteUnknown LeaseWriteOutcome = "unknown"
	LeaseWriteFailed  LeaseWriteOutcome = "failed"
)

// LeaseVoterWrite is the result of writing the lease to one voter.
type LeaseVoterWrite struct {
	Voter   string
	Outcome LeaseWriteOutcome
	// Counter is the counter of the last value sent to the voter.
	Counter int64
	Err     error
}

// LeaseWrite is the result of writing the lease to every voter.
type LeaseWrite struct {
	// Voters is in the order of the configured voters.
	Voters   []LeaseVoterWrite
	Majority int
}

// Written returns the number of voters that took the lease.
func (w LeaseWrite) Written() int {
	n := 0

	for _, v := range w.Voters {
		if v.Outcome == LeaseWriteDone {
			n++
		}
	}

	return n
}

// AcquireLease writes the lease of h to every voter in parallel: the holder,
// the generation, fencing and a counter above every counter read. The
// operation that makes h active takes the lease with it, and
// [Coordinator.RenewLease] renews it the same way.
//
// It writes nothing unless the effective state names h.Datacenter at
// h.Generation, see [ErrNotLeaseHolder] and [ErrLeaseHolderUndetermined].
// A voter is written only when it holds a lower generation, the lease of h
// or no lease; a KV without the key gets it created, an arbiter never. Each
// write is a CAS at the version of the latest read, and a voter whose lease
// changed or whose write outcome is unknown is read again before the next
// CAS. The lease is taken when a majority of voters took it; otherwise the
// result comes with [ErrLeaseMinority].
func (c *Coordinator) AcquireLease(ctx context.Context, h LeaseHolder) (LeaseWrite, error) {
	if err := h.validate(); err != nil {
		return LeaseWrite{}, err
	}

	if err := c.checkHolder(ctx, h); err != nil {
		return LeaseWrite{}, err
	}

	reads := make([]VoterLease, len(c.voters))

	var wg sync.WaitGroup

	for i, v := range c.voters {
		wg.Go(func() { reads[i] = c.readLease(ctx, v) })
	}

	wg.Wait()

	var counter int64

	for _, r := range reads {
		if r.Status == StatusPresent {
			counter = max(counter, r.Lease.Counter)
		}
	}

	w := LeaseWrite{Voters: make([]LeaseVoterWrite, len(c.voters)), Majority: len(c.voters)/2 + 1}

	for i, v := range c.voters {
		wg.Go(func() { w.Voters[i] = c.writeLease(ctx, v, h, reads[i], counter+1) })
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		return w, fmt.Errorf("write %s: %w", kv.LeaseKey, err)
	}

	if n := w.Written(); n < w.Majority {
		return w, fmt.Errorf("%s of %s at generation %d on %d of %d voters: %w",
			kv.LeaseKey, h.Datacenter, h.Generation, n, len(c.voters), ErrLeaseMinority)
	}

	return w, nil
}

// checkHolder reads dcctl/state and tells whether h is the effective holder.
func (c *Coordinator) checkHolder(ctx context.Context, h LeaseHolder) error {
	view, err := c.Read(ctx)
	if err != nil {
		return err
	}

	e := view.Effective

	switch {
	case e == nil:
		return fmt.Errorf("no effective %s: %w", kv.StateKey, ErrLeaseHolderUndetermined)
	case e.State.Generation > h.Generation,
		e.State.Generation == h.Generation && e.State.ActiveDatacenter != h.Datacenter:
		return fmt.Errorf("effective %s is %s at generation %d, not %s at %d: %w", kv.StateKey,
			e.State.ActiveDatacenter, e.State.Generation, h.Datacenter, h.Generation, ErrNotLeaseHolder)
	case e.State.Generation < h.Generation:
		return fmt.Errorf("effective %s is at generation %d, below %d: %w", kv.StateKey,
			e.State.Generation, h.Generation, ErrLeaseHolderUndetermined)
	}

	return nil
}

// writeLease writes the lease of h with at least counter to v, starting
// from the read cur.
func (c *Coordinator) writeLease(ctx context.Context, v Voter, h LeaseHolder, cur VoterLease, counter int64) LeaseVoterWrite {
	res := LeaseVoterWrite{Voter: v.Name()}

	var sent *kv.Lease

	for attempt := range leaseWriteAttempts {
		if attempt > 0 {
			cur = c.readLease(ctx, v)
		}

		var version Version

		switch cur.Status {
		case StatusPresent:
			if sent != nil && cur.Lease == *sent {
				res.Outcome, res.Err = LeaseWriteDone, nil

				return res
			}

			if l := cur.Lease; l.Generation > h.Generation || l.Generation == h.Generation && l.Holder != h.Datacenter {
				res.Outcome, res.Err = LeaseWriteSuperseded, nil

				return res
			}

			version, counter = cur.Version, max(counter, cur.Lease.Counter+1)
		case StatusAbsent:
			if v.Kind() != KindKV {
				res.Outcome, res.Err = LeaseWriteSkipped, ErrNotFound

				return res
			}
		case StatusUninitialized, StatusInvalid:
			res.Outcome, res.Err = LeaseWriteSkipped, cur.Err

			return res
		case StatusUnreachable, StatusStale:
			// After an unknown outcome the voter may still hold what was sent.
			if sent == nil {
				res.Outcome = LeaseWriteFailed
			}

			res.Err = errors.Join(res.Err, cur.Err)

			return res
		}

		lease := kv.Lease{Holder: h.Datacenter, Generation: h.Generation, Counter: counter, Fencing: h.Fencing}
		res.Counter = counter

		err := c.casLease(ctx, v, lease, version)

		switch {
		case err == nil:
			res.Outcome, res.Err = LeaseWriteDone, nil

			return res
		case errors.Is(err, ErrVersionMismatch):
			res.Outcome, res.Err = LeaseWriteMismatch, err
		case errors.Is(err, ErrOutcomeUnknown):
			res.Outcome, res.Err, sent = LeaseWriteUnknown, err, &lease
		default:
			res.Outcome, res.Err = LeaseWriteFailed, err

			return res
		}

		if ctx.Err() != nil {
			return res
		}
	}

	return res
}

func (c *Coordinator) casLease(ctx context.Context, v Voter, lease kv.Lease, version Version) error {
	value, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("encode %s: %w", kv.LeaseKey, err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	return v.CAS(ctx, KeyLease, value, version)
}
