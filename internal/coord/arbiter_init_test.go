package coord_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// arbiterFake is an arbiter that creates its records as its initialization does.
type arbiterFake struct {
	*leaseFake

	// createUnknown fails the next create with an unknown outcome, after applying it.
	createUnknown bool
	// beforeCreate runs at the start of every create, as another caller would.
	beforeCreate func(f *arbiterFake)
	creates      []coord.Key
}

func newArbiterFake(state, lease []byte) *arbiterFake {
	f := &arbiterFake{leaseFake: newLeaseFake(coord.ArbiterName, coord.KindArbiter, state, lease)}
	f.uninitialized = state == nil

	return f
}

func (f *arbiterFake) Create(_ context.Context, key coord.Key, value []byte) error {
	if f.beforeCreate != nil {
		f.beforeCreate(f)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.creates = append(f.creates, key)

	switch _, ok := f.records[key]; {
	case f.down:
		return unreachable()
	case ok:
		return fmt.Errorf("%s exists: %w", key, coord.ErrVersionMismatch)
	case key == coord.KeyLease && f.uninitialized:
		return coord.ErrUninitialized
	}

	f.putLocked(key, value)
	f.uninitialized = false

	if f.createUnknown {
		f.createUnknown = false

		return fmt.Errorf("%w: connection reset", coord.ErrOutcomeUnknown)
	}

	return nil
}

func (f *arbiterFake) state(t *testing.T) (kv.State, bool) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	r, ok := f.records[coord.KeyState]
	if !ok {
		return kv.State{}, false
	}

	var s kv.State
	require.NoError(t, json.Unmarshal(r.value, &s))

	return s, true
}

func arbiterCoordinator(t *testing.T, a, b *leaseFake, arb *arbiterFake) *coord.Coordinator {
	t.Helper()

	c, err := coord.New([]coord.Voter{a, b, arb}, time.Second, quietLog())
	require.NoError(t, err)

	return c
}

var settled8 = kv.State{Generation: 8, Revision: 2, ActiveDatacenter: "dc_a", OpID: "op8", PatroniTimeline: 6, NeedsRecovery: []string{"dc_b"}}

func TestInitializeArbiterCopiesStateThenLease(t *testing.T) {
	state := stateJSON(t, settled8)
	a := newLeaseFake("dc_a", coord.KindKV, state, leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 41, Fencing: true}))
	b := newLeaseFake("dc_b", coord.KindKV, state, leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 40, Fencing: true}))
	arb := newArbiterFake(nil, nil)

	got, err := arbiterCoordinator(t, a, b, arb).InitializeArbiter(t.Context())
	require.NoError(t, err)

	assert.True(t, got.StateCreated)
	assert.True(t, got.LeaseCreated)
	assert.Equal(t, []coord.Key{coord.KeyState, coord.KeyLease}, arb.creates, "state first, then lease")

	s, ok := arb.state(t)
	require.True(t, ok)
	assert.Equal(t, settled8, s, "the arbiter gets the record as the KVs hold it")

	l, ok := arb.lease(t)
	require.True(t, ok)
	assert.Equal(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 41, Fencing: true}, l, "the lease of the highest counter")
}

// Without a lease of the state on the KVs, the arbiter gets one of its holder above
// every counter read.
func TestInitializeArbiterBuildsLease(t *testing.T) {
	state := stateJSON(t, settled8)
	a := newLeaseFake("dc_a", coord.KindKV, state, leaseJSON(t, kv.Lease{Holder: "dc_b", Generation: 7, Counter: 9}))
	b := newLeaseFake("dc_b", coord.KindKV, state, nil)
	arb := newArbiterFake(nil, nil)

	got, err := arbiterCoordinator(t, a, b, arb).InitializeArbiter(t.Context())
	require.NoError(t, err)
	assert.Equal(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 10}, got.Lease)
}

// A call after one whose lease write did not land finishes it; a call on an arbiter
// that holds both writes nothing.
func TestInitializeArbiterAgain(t *testing.T) {
	state := stateJSON(t, settled8)
	lease := leaseJSON(t, kv.Lease{Holder: "dc_a", Generation: 8, Counter: 3})
	a, b := newLeaseFake("dc_a", coord.KindKV, state, lease), newLeaseFake("dc_b", coord.KindKV, state, lease)
	arb := newArbiterFake(state, nil)

	co := arbiterCoordinator(t, a, b, arb)

	got, err := co.InitializeArbiter(t.Context())
	require.NoError(t, err)
	assert.False(t, got.StateCreated)
	assert.True(t, got.LeaseCreated)
	assert.Equal(t, []coord.Key{coord.KeyLease}, arb.creates)

	got, err = co.InitializeArbiter(t.Context())
	require.NoError(t, err)
	assert.False(t, got.StateCreated)
	assert.False(t, got.LeaseCreated)
	assert.Equal(t, []coord.Key{coord.KeyLease}, arb.creates)
}

// A state write with an unknown outcome stops the call; the next one reads it there.
func TestInitializeArbiterOutcomeUnknown(t *testing.T) {
	state := stateJSON(t, settled8)
	a, b := newLeaseFake("dc_a", coord.KindKV, state, nil), newLeaseFake("dc_b", coord.KindKV, state, nil)
	arb := newArbiterFake(nil, nil)
	arb.createUnknown = true

	co := arbiterCoordinator(t, a, b, arb)

	_, err := co.InitializeArbiter(t.Context())
	require.ErrorIs(t, err, coord.ErrOutcomeUnknown)

	_, hasLease := arb.lease(t)
	assert.False(t, hasLease, "no lease before the state is known to be there")

	got, err := co.InitializeArbiter(t.Context())
	require.NoError(t, err)
	assert.False(t, got.StateCreated)
	assert.True(t, got.LeaseCreated)
}

// A lease left on the arbiter from before, of another holder or of a higher generation
// than the state, refuses the initialization; the arbiter shows it only once it holds
// the state. A lower lease of the holder stays for its renewal to raise.
func TestInitializeArbiterLeftLease(t *testing.T) {
	tests := []struct {
		name  string
		lease kv.Lease
		held  []byte
		want  error
	}{
		{name: "another holder", lease: kv.Lease{Holder: "dc_b", Generation: 8, Counter: 3}, want: coord.ErrArbiterInitialized},
		{name: "higher generation", lease: kv.Lease{Holder: "dc_a", Generation: 9, Counter: 3}, want: coord.ErrArbiterInitialized},
		{name: "older lease of another holder", lease: kv.Lease{Holder: "dc_b", Generation: 7, Counter: 3}, want: coord.ErrArbiterInitialized},
		{name: "resumed with another holder", lease: kv.Lease{Holder: "dc_b", Generation: 8}, held: stateJSON(t, settled8), want: coord.ErrArbiterInitialized},
		{name: "lower generation of the holder", lease: kv.Lease{Holder: "dc_a", Generation: 7, Counter: 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := stateJSON(t, settled8)
			a, b := newLeaseFake("dc_a", coord.KindKV, state, nil), newLeaseFake("dc_b", coord.KindKV, state, nil)
			arb := newArbiterFake(tt.held, leaseJSON(t, tt.lease))
			arb.uninitialized = tt.held == nil

			got, err := arbiterCoordinator(t, a, b, arb).InitializeArbiter(t.Context())
			require.ErrorIs(t, err, tt.want)
			assert.False(t, got.LeaseCreated)

			l, ok := arb.lease(t)
			require.True(t, ok)
			assert.Equal(t, tt.lease, l, "the lease is never written over")
		})
	}
}

func TestInitializeArbiterRefused(t *testing.T) {
	other := settled8
	other.PatroniTimeline++

	tests := []struct {
		name  string
		setup func(a, b *leaseFake, arb *arbiterFake)
		want  error
	}{
		{name: "kv down", setup: func(_, b *leaseFake, _ *arbiterFake) { b.down = true }, want: coord.ErrStatesDiffer},
		{name: "kv without state", setup: func(_, b *leaseFake, _ *arbiterFake) {
			b.set(func(f *leaseFake) { delete(f.records, coord.KeyState) })
		}, want: coord.ErrStatesDiffer},
		{name: "kvs differ", setup: func(_, b *leaseFake, _ *arbiterFake) { b.put(coord.KeyState, stateJSON(t, other)) }, want: coord.ErrStatesDiffer},
		{name: "arbiter holds another state", setup: func(_, _ *leaseFake, arb *arbiterFake) {
			arb.put(coord.KeyState, stateJSON(t, kv.State{Generation: 3, ActiveDatacenter: "dc_b", OpID: "op3"}))
			arb.uninitialized = false
		}, want: coord.ErrArbiterInitialized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := stateJSON(t, settled8)
			a, b := newLeaseFake("dc_a", coord.KindKV, state, nil), newLeaseFake("dc_b", coord.KindKV, state, nil)
			arb := newArbiterFake(nil, nil)
			tt.setup(a, b, arb)

			_, err := arbiterCoordinator(t, a, b, arb).InitializeArbiter(t.Context())
			require.ErrorIs(t, err, tt.want)
			assert.Empty(t, arb.creates)
		})
	}
}

// Another call that creates the state first with the same record is no refusal; with
// another record it is.
func TestInitializeArbiterRace(t *testing.T) {
	tests := []struct {
		name string
		them kv.State
		want error
	}{
		{name: "same record", them: settled8},
		{name: "another record", them: kv.State{Generation: 9, ActiveDatacenter: "dc_b", OpID: "op9"}, want: coord.ErrArbiterInitialized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := stateJSON(t, settled8)
			a, b := newLeaseFake("dc_a", coord.KindKV, state, nil), newLeaseFake("dc_b", coord.KindKV, state, nil)
			arb := newArbiterFake(nil, nil)
			arb.beforeCreate = func(f *arbiterFake) {
				f.beforeCreate = nil
				f.put(coord.KeyState, stateJSON(t, tt.them))
				f.set(func(f *leaseFake) { f.uninitialized = false })
			}

			got, err := arbiterCoordinator(t, a, b, arb).InitializeArbiter(t.Context())
			require.ErrorIs(t, err, tt.want)
			assert.False(t, got.StateCreated)
		})
	}
}

func TestInitializeArbiterNone(t *testing.T) {
	state := stateJSON(t, settled8)

	c, err := coord.New([]coord.Voter{newLeaseFake("dc_a", coord.KindKV, state, nil)}, time.Second, quietLog())
	require.NoError(t, err)

	_, err = c.InitializeArbiter(t.Context())
	require.ErrorIs(t, err, coord.ErrNoArbiter)
}
