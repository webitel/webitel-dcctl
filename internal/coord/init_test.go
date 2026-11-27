package coord_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// first is the record an initialization with dc_a active writes.
var first = kv.State{Generation: 1, ActiveDatacenter: "dc_a", OpID: "op-init", PatroniTimeline: 4}

func emptyArbiter() *fakeVoter {
	f := newFake(coord.ArbiterName, coord.KindArbiter, nil)
	f.uninitialized = true

	return f
}

func TestInitializeCreatesOnEveryKV(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, nil)
	b := newFake("dc_b", coord.KindKV, nil)
	arb := emptyArbiter()
	co := newCoordinator(t, time.Second, a, b, arb)

	res, err := co.Initialize(t.Context(), first)
	require.NoError(t, err)

	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, []string{"dc_a", "dc_b"}, res.Accepted())
	assert.Equal(t, coord.WriteSkipped, writeOutcomes(res)[coord.ArbiterName])

	for _, f := range []*fakeVoter{a, b} {
		assert.Equal(t, first, written(t, f), f.name)

		_, calls := f.state()
		require.Len(t, calls, 1, f.name)
		assert.Equal(t, coord.Version(0), calls[0].version, "%s: the key is created, never written over", f.name)
	}

	requireNoCAS(t, arb)

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	require.NotNil(t, view.Effective)
	assert.True(t, coord.ActiveKnown(view))
}

// The state is created only where no voter holds it or may hold it.
func TestInitializeRefused(t *testing.T) {
	tests := []struct {
		name  string
		setup func(a, b, arb *fakeVoter)
	}{
		{name: "kv holds state", setup: func(_, b, _ *fakeVoter) { b.set(raw(t, 3, "dc_b", false)) }},
		{name: "kv does not answer", setup: func(_, b, _ *fakeVoter) { b.getErr = errUnreachable("dc_b") }},
		{name: "kv holds an invalid record", setup: func(_, b, _ *fakeVoter) { b.set([]byte("{")) }},
		{name: "arbiter holds state", setup: func(_, _, arb *fakeVoter) {
			arb.uninitialized = false
			arb.set(raw(t, 7, "dc_a", false))
		}},
		{name: "another operation created the record", setup: func(a, _, _ *fakeVoter) {
			s := first
			s.OpID = "op-other"
			a.set(stateJSON(t, s))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b, arb := newFake("dc_a", coord.KindKV, nil), newFake("dc_b", coord.KindKV, nil), emptyArbiter()
			tt.setup(a, b, arb)

			co := newCoordinator(t, time.Second, a, b, arb)

			_, err := co.Initialize(t.Context(), first)
			require.ErrorIs(t, err, coord.ErrInitialized)
			requireNoCAS(t, a, b, arb)
		})
	}
}

// An arbiter that does not answer may hold a record from before, which would sit next
// to the new one once it is back: init waits for it, whatever it holds.
func TestInitializeArbiterDown(t *testing.T) {
	tests := []struct {
		name  string
		value []byte
	}{
		{name: "stale forced state", value: raw(t, 7, "dc_b", true)},
		{name: "stale state", value: raw(t, 7, "dc_b", false)},
		{name: "empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := newFake("dc_a", coord.KindKV, nil), newFake("dc_b", coord.KindKV, nil)
			arb := newFake(coord.ArbiterName, coord.KindArbiter, tt.value)
			arb.getErr = errUnreachable("arbiter")

			_, err := newCoordinator(t, time.Second, a, b, arb).Initialize(t.Context(), first)
			require.ErrorIs(t, err, coord.ErrInitialized)
			assert.ErrorContains(t, err, "clear its data_dir")
			requireNoCAS(t, a, b, arb)
		})
	}
}

// A KV that drops between the read and the write leaves the record on one KV of
// two and the arbiter: it is not committed.
func TestInitializeVoterFailsMidWrite(t *testing.T) {
	a, b, arb := newFake("dc_a", coord.KindKV, nil), newFake("dc_b", coord.KindKV, nil), emptyArbiter()
	b.casErr = errUnreachable("dc_b")

	res, err := newCoordinator(t, time.Second, a, b, arb).Initialize(t.Context(), first)
	require.ErrorIs(t, err, coord.ErrUncommitted)
	assert.Equal(t, []string{"dc_a"}, res.Accepted())
}

// A run after one that wrote some KVs finishes the record: the KV that holds it
// counts, the other gets it created.
func TestInitializeAgain(t *testing.T) {
	a, b, arb := newFake("dc_a", coord.KindKV, stateJSON(t, first)), newFake("dc_b", coord.KindKV, nil), emptyArbiter()

	res, err := newCoordinator(t, time.Second, a, b, arb).Initialize(t.Context(), first)
	require.NoError(t, err)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, []string{"dc_a", "dc_b"}, res.Accepted())
	requireNoCAS(t, a)
	assert.Equal(t, first, written(t, b))

	other := first
	other.PatroniTimeline++

	_, err = newCoordinator(t, time.Second, a, newFake("dc_b", coord.KindKV, nil), emptyArbiter()).Initialize(t.Context(), other)
	require.ErrorIs(t, err, coord.ErrBaseChanged, "the voters would hold two payloads of revision 0")
}

func TestInitializeValidates(t *testing.T) {
	bad := []kv.State{
		{Generation: 2, ActiveDatacenter: "dc_a", OpID: "op"},
		{Generation: 1, Revision: 1, ActiveDatacenter: "dc_a", OpID: "op"},
		{Generation: 1, OpID: "op"},
		{Generation: 1, ActiveDatacenter: "dc_a"},
		{Generation: 1, ActiveDatacenter: "dc_a", OpID: "op", Forced: true},
		{Generation: 1, ActiveDatacenter: "dc_a", OpID: "op", NeedsRecovery: []string{"dc_a"}},
	}

	for i, s := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, nil)

			_, err := newCoordinator(t, time.Second, a).Initialize(t.Context(), s)
			require.Error(t, err)
			requireNoCAS(t, a)
		})
	}
}
