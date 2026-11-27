package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

var (
	// ErrNoArbiter means no arbiter is configured.
	ErrNoArbiter = errors.New("no arbiter is configured")
	// ErrArbiterInitialized means the arbiter holds another state than the one the
	// KVs agree on. It is not written over: an arbiter is filled once, after its
	// data_dir is cleared.
	ErrArbiterInitialized = errors.New("arbiter is initialized")
	// ErrStatesDiffer means the KVs do not all hold one payload of dcctl/state, or
	// one of them does not answer: the arbiter must not take a side.
	ErrStatesDiffer = errors.New("KVs do not hold one state")
)

// ArbiterInit is what [Coordinator.InitializeArbiter] gave the arbiter.
type ArbiterInit struct {
	State kv.State
	Lease kv.Lease
	// StateCreated and LeaseCreated tell the records this call created; a record
	// the arbiter held already, as after an earlier call, is not.
	StateCreated, LeaseCreated bool
}

// InitializeArbiter gives an empty arbiter the state every KV holds and the
// current lease: dcctl/state first, then dcctl/lease, each created only when
// the arbiter holds no such record. The arbiter votes from the first write on;
// until the second, its lease is not found, which the lease handles.
//
// Every KV must answer with the same payload of dcctl/state, otherwise
// [ErrStatesDiffer]. The lease is the one of the generation and active
// datacenter of that state with the highest counter a KV holds; without one, a
// lease of that holder with a counter above every one read.
//
// An arbiter that holds the same state already, as after an earlier call, gets
// only what it lacks. One that holds another state is refused with
// [ErrArbiterInitialized] and nothing is written. So is one that holds a lease
// of a higher generation or of another holder than the state: the arbiter shows
// its lease only once it holds a state, so this refusal can come after the
// state is written, and its data_dir is cleared before it is initialized
// again. A write whose outcome is unknown comes with [ErrOutcomeUnknown]: call
// again, it reads what the arbiter holds then.
func (c *Coordinator) InitializeArbiter(ctx context.Context) (ArbiterInit, error) {
	arb := c.arbiter()
	if arb == nil {
		return ArbiterInit{}, ErrNoArbiter
	}

	view, err := c.Read(ctx)
	if err != nil {
		return ArbiterInit{}, err
	}

	seed, err := arbiterSeed(view)
	if err != nil {
		return ArbiterInit{}, err
	}

	out := ArbiterInit{State: seed.State}

	if out.StateCreated, err = c.createState(ctx, arb, view, seed); err != nil {
		return out, err
	}

	leases := c.readLeases(ctx)
	out.Lease = arbiterLease(seed.State, leases)

	if out.LeaseCreated, err = c.createLease(ctx, arb, leases, out.Lease, seed.State); err != nil {
		return out, err
	}

	c.log.InfoCtx(ctx, "arbiter initialized",
		logging.Int64("generation", seed.State.Generation), logging.Str("active_datacenter", seed.State.ActiveDatacenter),
		logging.Int64("lease_counter", out.Lease.Counter), logging.Bool("state_created", out.StateCreated),
		logging.Bool("lease_created", out.LeaseCreated))

	return out, nil
}

// arbiter returns the arbiter among the voters, nil when there is none.
func (c *Coordinator) arbiter() Creator {
	for _, v := range c.voters {
		if cr, ok := v.(Creator); ok && v.Kind() == KindArbiter {
			return cr
		}
	}

	return nil
}

// arbiterSeed returns the state of view the arbiter gets: the payload every KV
// holds.
func arbiterSeed(view View) (VoterState, error) {
	var seed *VoterState

	for i, vs := range view.Voters {
		switch {
		case vs.Kind != KindKV:
		case vs.Status != StatusPresent:
			return VoterState{}, fmt.Errorf("%s of %s is %s: %w", kv.StateKey, vs.Name, vs.Status, ErrStatesDiffer)
		case seed == nil:
			seed = &view.Voters[i]
		case !samePayload(seed.State, vs.State):
			return VoterState{}, fmt.Errorf("%s holds %s revision %d, %s holds %s revision %d: %w", seed.Name, recordName(seed.State),
				seed.State.Revision, vs.Name, recordName(vs.State), vs.State.Revision, ErrStatesDiffer)
		}
	}

	if seed == nil {
		return VoterState{}, fmt.Errorf("no KV holds %s: %w", kv.StateKey, ErrStatesDiffer)
	}

	return *seed, nil
}

// createState creates dcctl/state of seed on arb unless it holds that payload
// already, and tells whether it created it.
func (c *Coordinator) createState(ctx context.Context, arb Creator, view View, seed VoterState) (bool, error) {
	i := slices.IndexFunc(view.Voters, func(vs VoterState) bool { return vs.Name == arb.Name() })

	held, err := arbiterHolds(view.Voters[i], seed.State)
	if err != nil || held {
		return false, err
	}

	err = c.create(ctx, arb, KeyState, seed.Raw)
	if !errors.Is(err, ErrVersionMismatch) {
		return err == nil, err
	}

	// Another call created it in between.
	if held, err = arbiterHolds(c.readState(ctx, arb), seed.State); err == nil && !held {
		err = fmt.Errorf("arbiter holds another %s than the KVs: %w", kv.StateKey, ErrArbiterInitialized)
	}

	return false, err
}

// arbiterHolds reports whether the arbiter read as vs holds s already; an arbiter
// that holds anything else, or is not read, fails.
func arbiterHolds(vs VoterState, s kv.State) (bool, error) {
	switch vs.Status {
	case StatusUninitialized, StatusAbsent:
		return false, nil
	case StatusPresent:
		if samePayload(vs.State, s) {
			return true, nil
		}

		return false, fmt.Errorf("arbiter holds %s revision %d, the KVs %s revision %d: %w", recordName(vs.State), vs.State.Revision,
			recordName(s), s.Revision, ErrArbiterInitialized)
	case StatusInvalid:
		return false, fmt.Errorf("arbiter holds a %s that cannot be decoded: %w", kv.StateKey, ErrArbiterInitialized)
	case StatusUnreachable, StatusStale:
	}

	return false, fmt.Errorf("arbiter %s is %s: %w", kv.StateKey, vs.Status, notRead(vs.Err))
}

// notRead is why a record is not read: err, or that it is not read when err does
// not tell.
func notRead(err error) error {
	if err == nil {
		return errors.New("not read")
	}

	return err
}

// readLeases reads dcctl/lease from every voter in parallel.
func (c *Coordinator) readLeases(ctx context.Context) []VoterLease {
	out := make([]VoterLease, len(c.voters))

	var wg sync.WaitGroup

	for i, v := range c.voters {
		wg.Go(func() { out[i] = c.readLease(ctx, v) })
	}

	wg.Wait()

	return out
}

// arbiterLease returns the lease the arbiter gets with s, see
// [Coordinator.InitializeArbiter].
func arbiterLease(s kv.State, leases []VoterLease) kv.Lease {
	var (
		best    *kv.Lease
		counter int64
	)

	for i, l := range leases {
		if l.Kind != KindKV || l.Status != StatusPresent {
			continue
		}

		counter = max(counter, l.Lease.Counter)

		if l.Lease.Generation == s.Generation && l.Lease.Holder == s.ActiveDatacenter && (best == nil || l.Lease.Counter > best.Counter) {
			best = &leases[i].Lease
		}
	}

	if best != nil {
		return *best
	}

	return kv.Lease{Holder: s.ActiveDatacenter, Generation: s.Generation, Counter: counter + 1}
}

// createLease creates lease, the lease of the state s, on arb unless it holds a lease
// already, and tells whether it created it. A lease of s the arbiter holds was written
// by an earlier initialization, and its renewals keep it; a lease of a higher
// generation or of another holder is left from before and refuses the initialization.
func (c *Coordinator) createLease(ctx context.Context, arb Creator, leases []VoterLease, lease kv.Lease, s kv.State) (bool, error) {
	i := slices.IndexFunc(c.voters, func(v Voter) bool { return v.Name() == arb.Name() })

	switch cur := leases[i]; cur.Status {
	case StatusPresent:
		return false, leaseOf(cur.Lease, s)
	case StatusAbsent:
	case StatusUninitialized, StatusInvalid, StatusUnreachable, StatusStale:
		return false, fmt.Errorf("arbiter %s is %s: %w", kv.LeaseKey, cur.Status, notRead(cur.Err))
	}

	value, err := json.Marshal(lease)
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", kv.LeaseKey, err)
	}

	err = c.create(ctx, arb, KeyLease, value)
	if !errors.Is(err, ErrVersionMismatch) {
		return err == nil, err
	}

	// Another call created it in between.
	cur := c.readLease(ctx, arb)
	if cur.Status != StatusPresent {
		return false, fmt.Errorf("arbiter %s is %s: %w", kv.LeaseKey, cur.Status, notRead(cur.Err))
	}

	return false, leaseOf(cur.Lease, s)
}

// leaseOf checks that l, a lease the arbiter holds, may stay with the state s: of its
// holder and not above its generation.
func leaseOf(l kv.Lease, s kv.State) error {
	if l.Generation > s.Generation || l.Holder != s.ActiveDatacenter {
		return fmt.Errorf("arbiter holds %s of %s at generation %d, the state names %s at generation %d: %w", kv.LeaseKey,
			l.Holder, l.Generation, s.ActiveDatacenter, s.Generation, ErrArbiterInitialized)
	}

	return nil
}

func (c *Coordinator) create(ctx context.Context, arb Creator, key Key, value []byte) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if err := arb.Create(ctx, key, value); err != nil {
		return fmt.Errorf("create arbiter %s: %w", key, err)
	}

	return nil
}
