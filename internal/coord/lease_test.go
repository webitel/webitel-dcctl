package coord_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

type leaseRecord struct {
	value   []byte
	version coord.Version
}

// leaseFake is a voter holding dcctl/state and dcctl/lease the way a KV or
// an arbiter does.
type leaseFake struct {
	name string
	kind coord.Kind

	mu            sync.Mutex
	records       map[coord.Key]leaseRecord
	next          coord.Version
	uninitialized bool
	down          bool
	// leaseDown makes only the lease unreachable.
	leaseDown bool
	// hang makes lease calls wait for the end of their context.
	hang bool
	// unknown fails the next lease CASes with ErrOutcomeUnknown, after
	// applying them when unknownApplied is set.
	unknown        int
	unknownApplied bool
	// downAfterUnknown makes the lease unreachable after an unknown outcome.
	downAfterUnknown bool
	// beforeCAS runs at the start of every lease CAS, as another writer
	// would.
	beforeCAS func(f *leaseFake)
	calls     []string
}

func newLeaseFake(name string, kind coord.Kind, state, lease []byte) *leaseFake {
	f := &leaseFake{name: name, kind: kind, records: map[coord.Key]leaseRecord{}, next: 100}

	if state != nil {
		f.put(coord.KeyState, state)
	}

	if lease != nil {
		f.put(coord.KeyLease, lease)
	}

	return f
}

func (f *leaseFake) Name() string { return f.name }

func (f *leaseFake) Kind() coord.Kind { return f.kind }

func (f *leaseFake) Get(ctx context.Context, key coord.Key) (coord.Entry, error) {
	f.record("get %s", key)

	if err := f.wait(ctx, key); err != nil {
		return coord.Entry{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	r, ok := f.records[key]

	switch {
	case f.down, f.leaseDown && key == coord.KeyLease:
		return coord.Entry{}, unreachable()
	case f.uninitialized:
		return coord.Entry{}, coord.ErrUninitialized
	case !ok:
		return coord.Entry{}, coord.ErrNotFound
	}

	return coord.Entry{Value: slices.Clone(r.value), Version: r.version}, nil
}

func (f *leaseFake) CAS(ctx context.Context, key coord.Key, value []byte, version coord.Version) error {
	f.record("cas %s %d", key, version)

	if err := f.wait(ctx, key); err != nil {
		return err
	}

	f.mu.Lock()
	before := f.beforeCAS
	f.mu.Unlock()

	if before != nil && key == coord.KeyLease {
		before(f)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.down, f.leaseDown && key == coord.KeyLease:
		return unreachable()
	case f.kind == coord.KindArbiter && version == 0:
		return coord.ErrCreateRefused
	case f.uninitialized:
		return coord.ErrUninitialized
	}

	matches := f.records[key].version == version

	if key == coord.KeyLease && f.unknown > 0 {
		f.unknown--
		f.leaseDown = f.downAfterUnknown

		if f.unknownApplied && matches {
			f.putLocked(key, value)
		}

		return fmt.Errorf("%w: connection reset", coord.ErrOutcomeUnknown)
	}

	if !matches {
		return fmt.Errorf("%s at %d: %w", key, version, coord.ErrVersionMismatch)
	}

	f.putLocked(key, value)

	return nil
}

func (f *leaseFake) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// wait returns the error of a call on key that cannot start: ctx ended, or
// hang made it wait for that.
func (f *leaseFake) wait(ctx context.Context, key coord.Key) error {
	f.mu.Lock()
	hang := f.hang && key == coord.KeyLease
	f.mu.Unlock()

	if hang {
		<-ctx.Done()
	}

	if err := ctx.Err(); err != nil {
		return source.Classify("fake", err)
	}

	return nil
}

func (f *leaseFake) put(key coord.Key, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.putLocked(key, value)
}

func (f *leaseFake) putLocked(key coord.Key, value []byte) {
	f.next += 3
	f.records[key] = leaseRecord{value: slices.Clone(value), version: f.next}
}

func (f *leaseFake) set(fn func(f *leaseFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

// lease returns the lease f holds; ok is false without one.
func (f *leaseFake) lease(t *testing.T) (kv.Lease, bool) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	r, ok := f.records[coord.KeyLease]
	if !ok {
		return kv.Lease{}, false
	}

	var l kv.Lease
	require.NoError(t, json.Unmarshal(r.value, &l))

	return l, true
}

// leaseCalls returns the lease calls f received, in order.
func (f *leaseFake) leaseCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string

	for _, c := range f.calls {
		if strings.Contains(c, string(coord.KeyLease)) {
			out = append(out, c)
		}
	}

	return out
}

func (f *leaseFake) casCount() int {
	n := 0

	for _, c := range f.leaseCalls() {
		if strings.HasPrefix(c, "cas") {
			n++
		}
	}

	return n
}

func leaseJSON(t *testing.T, l kv.Lease) []byte {
	t.Helper()

	b, err := json.Marshal(l)
	require.NoError(t, err)

	return b
}

func leaseCoordinator(t *testing.T, voters ...*leaseFake) *coord.Coordinator {
	t.Helper()

	vs := make([]coord.Voter, 0, len(voters))
	for _, v := range voters {
		vs = append(vs, v)
	}

	c, err := coord.New(vs, time.Second, quietLog())
	require.NoError(t, err)

	return c
}

// threeVoters returns dc_a, dc_b and the arbiter, all at state and lease.
func threeVoters(t *testing.T, state []byte, lease kv.Lease) (a, b, arb *leaseFake) {
	t.Helper()

	l := leaseJSON(t, lease)

	return newLeaseFake("dc_a", coord.KindKV, state, l),
		newLeaseFake("dc_b", coord.KindKV, state, l),
		newLeaseFake(coord.ArbiterName, coord.KindArbiter, state, l)
}

var holderA = coord.LeaseHolder{Datacenter: "dc_a", Generation: 8, Fencing: true}

func leaseOutcomes(w coord.LeaseWrite) map[string]coord.LeaseWriteOutcome {
	out := make(map[string]coord.LeaseWriteOutcome, len(w.Voters))
	for _, v := range w.Voters {
		out[v.Voter] = v.Outcome
	}

	return out
}

func TestPresentLease(t *testing.T) {
	tests := []struct {
		name       string
		value      []byte
		wantStatus coord.Status
		wantLease  kv.Lease
	}{
		{
			name:       "valid",
			value:      []byte(`{"holder":"dc_a","generation":8,"counter":5,"fencing":true,"extra":1}`),
			wantStatus: coord.StatusPresent,
			wantLease:  kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5, Fencing: true},
		},
		{name: "bad_json", value: []byte(`{"holder":`), wantStatus: coord.StatusInvalid},
		{name: "wrong_type", value: []byte(`{"holder":"dc_a","counter":"5"}`), wantStatus: coord.StatusInvalid},
		{name: "no_holder", value: []byte(`{"generation":8,"counter":5}`), wantStatus: coord.StatusInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coord.PresentLease("dc_a", coord.KindKV, tt.value, 12)

			assert.Equal(t, "dc_a", got.Name)
			assert.Equal(t, coord.KindKV, got.Kind)
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, tt.wantLease, got.Lease)
			assert.Equal(t, coord.Version(12), got.Version)
		})
	}
}

func TestPresentLeaseInvalidReason(t *testing.T) {
	got := coord.PresentLease("dc_a", coord.KindKV, []byte(`{`), 12)

	assert.Equal(t, source.ReasonInvalid, got.Reason)
	require.Error(t, got.Err)
}

func TestAcquireLeaseMajority(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	b.put(coord.KeyLease, leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 3}))

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, 2, w.Majority)
	assert.Equal(t, 3, w.Written())

	want := kv.Lease{Holder: "dc_a", Generation: 8, Counter: 6, Fencing: true}

	for _, f := range []*leaseFake{a, b, arb} {
		got, ok := f.lease(t)
		require.True(t, ok, f.name)
		assert.Equal(t, want, got, f.name)
	}

	for _, v := range w.Voters {
		assert.Equal(t, int64(6), v.Counter, v.Voter)
	}
}

func TestAcquireLeaseOneVoterDown(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	b.set(func(f *leaseFake) { f.down = true })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, 2, w.Written())
	assert.Equal(t, map[string]coord.LeaseWriteOutcome{
		"dc_a": coord.LeaseWriteDone, "dc_b": coord.LeaseWriteFailed, coord.ArbiterName: coord.LeaseWriteDone,
	}, leaseOutcomes(w))
}

func TestAcquireLeaseMajorityDown(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	b.set(func(f *leaseFake) { f.leaseDown = true })
	arb.set(func(f *leaseFake) { f.leaseDown = true })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.ErrorIs(t, err, coord.ErrLeaseMinority)

	assert.Equal(t, 1, w.Written())
	assert.Equal(t, map[string]coord.LeaseWriteOutcome{
		"dc_a": coord.LeaseWriteDone, "dc_b": coord.LeaseWriteFailed, coord.ArbiterName: coord.LeaseWriteFailed,
	}, leaseOutcomes(w))
}

func TestAcquireLeaseUninitializedArbiterSkipped(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	arb.set(func(f *leaseFake) { f.uninitialized = true })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteSkipped, leaseOutcomes(w)[coord.ArbiterName])
	require.ErrorIs(t, w.Voters[2].Err, coord.ErrUninitialized)
	assert.Zero(t, arb.casCount())
}

func TestAcquireLeaseCreatesOnKVNotArbiter(t *testing.T) {
	state := raw(t, 8, "dc_a", false)
	a := newLeaseFake("dc_a", coord.KindKV, state, leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5}))
	b := newLeaseFake("dc_b", coord.KindKV, state, nil)
	arb := newLeaseFake(coord.ArbiterName, coord.KindArbiter, state, nil)

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, map[string]coord.LeaseWriteOutcome{
		"dc_a": coord.LeaseWriteDone, "dc_b": coord.LeaseWriteDone, coord.ArbiterName: coord.LeaseWriteSkipped,
	}, leaseOutcomes(w))

	got, ok := b.lease(t)
	require.True(t, ok)
	assert.Equal(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 6, Fencing: true}, got)

	assert.Equal(t, []string{"get lease"}, arb.leaseCalls())
}

func TestAcquireLeaseOutcomeUnknownApplied(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	a.set(func(f *leaseFake) { f.unknown, f.unknownApplied = 1, true })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteDone, leaseOutcomes(w)["dc_a"])
	assert.Equal(t, []string{"get lease", "cas lease 106", "get lease"}, a.leaseCalls())

	got, _ := a.lease(t)
	assert.Equal(t, int64(6), got.Counter)
}

func TestAcquireLeaseOutcomeUnknownNotApplied(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	a.set(func(f *leaseFake) { f.unknown = 1 })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteDone, leaseOutcomes(w)["dc_a"])
	// The second CAS uses the version of the read after the unknown outcome.
	assert.Equal(t, []string{"get lease", "cas lease 106", "get lease", "cas lease 106"}, a.leaseCalls())
}

func TestAcquireLeaseOutcomeUnknownEveryAttempt(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	a.set(func(f *leaseFake) { f.unknown = 10 })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteUnknown, leaseOutcomes(w)["dc_a"])
	assert.Equal(t, 3, a.casCount())
	require.ErrorIs(t, w.Voters[0].Err, coord.ErrOutcomeUnknown)
}

func TestAcquireLeaseOutcomeUnknownThenUnreachable(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	a.set(func(f *leaseFake) { f.unknown, f.downAfterUnknown = 1, true })

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteUnknown, leaseOutcomes(w)["dc_a"])
	require.ErrorIs(t, w.Voters[0].Err, coord.ErrOutcomeUnknown)
	assert.Equal(t, 1, a.casCount())
}

func TestAcquireLeaseCASConflict(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})

	other := leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 40})

	a.set(func(f *leaseFake) {
		f.beforeCAS = func(f *leaseFake) {
			f.put(coord.KeyLease, other)
			f.set(func(f *leaseFake) { f.beforeCAS = nil })
		}
	})

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteDone, leaseOutcomes(w)["dc_a"])

	got, _ := a.lease(t)
	assert.Equal(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 41, Fencing: true}, got)
	assert.Equal(t, int64(41), w.Voters[0].Counter)
}

func TestAcquireLeaseConflictEveryAttempt(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	a.set(func(f *leaseFake) {
		f.beforeCAS = func(f *leaseFake) {
			l, _ := f.lease(t)
			l.Counter++
			f.put(coord.KeyLease, leaseJSON(t, l))
		}
	})

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteMismatch, leaseOutcomes(w)["dc_a"])
	assert.Equal(t, 3, a.casCount())
	require.ErrorIs(t, w.Voters[0].Err, coord.ErrVersionMismatch)
}

func TestAcquireLeaseSuperseded(t *testing.T) {
	tests := []struct {
		name  string
		lease kv.Lease
	}{
		{name: "higher_generation", lease: kv.Lease{Holder: "dc_b", Generation: 9, Counter: 1}},
		{name: "other_holder_same_generation", lease: kv.Lease{Holder: "dc_b", Generation: 8, Counter: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
			b.put(coord.KeyLease, leaseJSON(t, tt.lease))

			w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
			require.NoError(t, err)

			assert.Equal(t, coord.LeaseWriteSuperseded, leaseOutcomes(w)["dc_b"])
			assert.Zero(t, b.casCount())

			got, _ := b.lease(t)
			assert.Equal(t, tt.lease, got)
		})
	}
}

func TestAcquireLeaseLeavesInvalidLease(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	b.put(coord.KeyLease, []byte(`{"holder":`))

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
	require.NoError(t, err)

	assert.Equal(t, coord.LeaseWriteSkipped, leaseOutcomes(w)["dc_b"])
	require.Error(t, w.Voters[1].Err)
	assert.Zero(t, b.casCount())
}

func TestAcquireLeaseStopsRetryingWhenContextEnds(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	a.set(func(f *leaseFake) {
		f.beforeCAS = func(f *leaseFake) {
			cancel()
			f.put(coord.KeyLease, leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 40}))
		}
	})

	_, err := leaseCoordinator(t, a, b, arb).AcquireLease(ctx, holderA)
	require.ErrorIs(t, err, context.Canceled)

	assert.Equal(t, []string{"get lease", "cas lease 106"}, a.leaseCalls())
}

func TestAcquireLeaseNotHolder(t *testing.T) {
	tests := []struct {
		name  string
		state []byte
	}{
		{name: "other_datacenter", state: raw(t, 8, "dc_b", false)},
		{name: "higher_generation", state: raw(t, 9, "dc_a", false)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b, arb := threeVoters(t, tt.state, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})

			_, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
			require.ErrorIs(t, err, coord.ErrNotLeaseHolder)

			for _, f := range []*leaseFake{a, b, arb} {
				assert.Zero(t, f.casCount(), f.name)
			}
		})
	}
}

func TestAcquireLeaseUndetermined(t *testing.T) {
	l := kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5}

	tests := []struct {
		name   string
		voters func() (a, b, arb *leaseFake)
	}{
		{name: "no_quorum", voters: func() (a, b, arb *leaseFake) {
			a, b, arb = threeVoters(t, raw(t, 8, "dc_a", false), l)
			b.down, arb.down = true, true

			return a, b, arb
		}},
		{name: "conflict", voters: func() (a, b, arb *leaseFake) {
			a, b, arb = threeVoters(t, raw(t, 8, "dc_a", true), l)
			b.put(coord.KeyState, raw(t, 8, "dc_b", true))

			arb.down = true

			return a, b, arb
		}},
		{name: "own_generation_not_committed", voters: func() (a, b, arb *leaseFake) {
			a, b, arb = threeVoters(t, raw(t, 7, "dc_b", false), l)
			a.put(coord.KeyState, raw(t, 8, "dc_a", false))

			return a, b, arb
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b, arb := tt.voters()

			_, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(), holderA)
			require.ErrorIs(t, err, coord.ErrLeaseHolderUndetermined)

			for _, f := range []*leaseFake{a, b, arb} {
				assert.Zero(t, f.casCount(), f.name)
			}
		})
	}
}

func TestAcquireLeaseNewGeneration(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_b", false), kv.Lease{Holder: "dc_a", Generation: 7, Counter: 90, Fencing: true})

	w, err := leaseCoordinator(t, a, b, arb).AcquireLease(t.Context(),
		coord.LeaseHolder{Datacenter: "dc_b", Generation: 8})
	require.NoError(t, err)

	assert.Equal(t, 3, w.Written())

	got, _ := arb.lease(t)
	assert.Equal(t, kv.Lease{Holder: "dc_b", Generation: 8, Counter: 91}, got)
}

func TestAcquireLeaseValidates(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	c := leaseCoordinator(t, a, b, arb)

	for _, h := range []coord.LeaseHolder{{Generation: 8}, {Datacenter: "dc_a"}} {
		_, err := c.AcquireLease(t.Context(), h)
		require.Error(t, err)
	}

	assert.Zero(t, a.casCount())
}
