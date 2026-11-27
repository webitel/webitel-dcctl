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

// The sides of a conflict: two forced failovers of generation 8, one on each side of a
// partition.
var (
	sideA = kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op-fa", Forced: true, PatroniTimeline: 6}
	sideB = kv.State{Generation: 8, ActiveDatacenter: "dc_b", OpID: "op-fb", Forced: true, PatroniTimeline: 7}
)

// resolveTo is generation gen with dc_b active, written by the resolve op-r.
func resolveTo(gen int64, forced bool) kv.State {
	return kv.State{Generation: gen, ActiveDatacenter: "dc_b", OpID: "op-r", Forced: forced, PatroniTimeline: 7, NeedsRecovery: []string{"dc_a"}}
}

func conflictVoters(t *testing.T) (a, b, arb *fakeVoter) {
	t.Helper()

	return newFake("dc_a", coord.KindKV, stateJSON(t, sideA)), newFake("dc_b", coord.KindKV, stateJSON(t, sideB)),
		newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 7, "dc_a", false))
}

func TestResolveQuorum(t *testing.T) {
	a, b, arb := conflictVoters(t)
	co := newCoordinator(t, time.Second, a, b, arb)
	to := resolveTo(9, false)

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	require.Len(t, view.Conflict, 2)

	mode, err := coord.ResolveMode(view, sideB, to)
	require.NoError(t, err)
	assert.Equal(t, coord.WriteQuorum, mode)
	assert.Equal(t, int64(8), coord.TopGeneration(view))

	res, err := co.Resolve(t.Context(), sideB, to, forcedQuorum())
	require.NoError(t, err)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, []string{"dc_a", "dc_b", coord.ArbiterName}, res.Accepted())

	for _, f := range []*fakeVoter{a, b, arb} {
		assert.Equal(t, to, written(t, f), f.name)
	}

	view, err = co.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, view.Conflict)
	require.NotNil(t, view.Effective)
	assert.Equal(t, "dc_b", view.Effective.State.ActiveDatacenter)
}

// forcedQuorum is a quorum write with the isolation confirmed, as every resolve has it.
func forcedQuorum() coord.AdvanceOptions {
	return coord.AdvanceOptions{Mode: coord.WriteQuorum, IsolationConfirmed: true}
}

// Without a majority that can take it, the record is forced: of three datacenters and
// the arbiter, only the two sides answer.
func TestResolveForced(t *testing.T) {
	a, b, arb := conflictVoters(t)
	c := newFake("dc_c", coord.KindKV, nil)
	c.getErr = errUnreachable("dc_c")
	arb.getErr = errUnreachable("arbiter")
	co := newCoordinator(t, time.Second, a, b, c, arb)

	_, err := co.Resolve(t.Context(), sideB, resolveTo(9, false), forcedQuorum())
	require.ErrorIs(t, err, coord.ErrModeChanged)

	res, err := co.Resolve(t.Context(), sideB, resolveTo(9, true), forced)
	require.NoError(t, err)
	assert.Equal(t, coord.WriteForced, res.Mode)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, []string{"dc_a", "dc_b"}, res.Accepted())
}

// A record above the conflict that some voter holds, uncommitted, puts the new
// generation above it.
func TestResolveAboveEveryGeneration(t *testing.T) {
	a, b, arb := conflictVoters(t)
	arb.set(raw(t, 9, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b, arb)

	_, err := co.Resolve(t.Context(), sideB, resolveTo(9, false), forcedQuorum())
	require.ErrorIs(t, err, coord.ErrSuperseded)
	requireNoCAS(t, a, b, arb)

	_, err = co.Resolve(t.Context(), sideB, resolveTo(10, false), forcedQuorum())
	require.NoError(t, err)
	assert.Equal(t, resolveTo(10, false), written(t, arb))
}

func TestResolveRefused(t *testing.T) {
	changed := sideB
	changed.PatroniTimeline++

	tests := []struct {
		name   string
		winner kv.State
		opts   coord.AdvanceOptions
		setup  func(t *testing.T, a, b, arb *fakeVoter)
		want   error
	}{
		{name: "isolation not confirmed", winner: sideB, opts: quorum, want: coord.ErrConfirmationRequired},
		{name: "no conflict", winner: sideB, opts: forcedQuorum(), setup: func(t *testing.T, a, _, _ *fakeVoter) {
			t.Helper()
			a.set(stateJSON(t, sideB))
		}, want: coord.ErrBaseNotFound},
		{name: "winner changed", winner: changed, opts: forcedQuorum(), want: coord.ErrBaseChanged},
		{name: "not a side", winner: kv.State{Generation: 8, ActiveDatacenter: "dc_c", OpID: "x"}, opts: forcedQuorum(), want: coord.ErrBaseNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b, arb := conflictVoters(t)
			if tt.setup != nil {
				tt.setup(t, a, b, arb)
			}

			to := resolveTo(9, false)
			to.ActiveDatacenter = tt.winner.ActiveDatacenter

			_, err := newCoordinator(t, time.Second, a, b, arb).Resolve(t.Context(), tt.winner, to, tt.opts)
			require.ErrorIs(t, err, tt.want)
			requireNoCAS(t, a, b, arb)
		})
	}
}

// A resolve after one whose write reached some voters finishes it, also when the
// conflict is gone by then.
func TestResolveAgain(t *testing.T) {
	a, b, arb := conflictVoters(t)
	to := resolveTo(9, false)
	b.set(stateJSON(t, to))
	arb.set(stateJSON(t, to))

	co := newCoordinator(t, time.Second, a, b, arb)

	res, err := co.Resolve(t.Context(), sideB, to, forcedQuorum())
	require.NoError(t, err)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, to, written(t, a))
	requireNoCAS(t, b, arb)
}

// The other side is written first: the winning side is written only once it took the
// new record, so a write that stops half way leaves the winner valid.
func TestResolveWritesOtherSideFirst(t *testing.T) {
	t.Run("order", func(t *testing.T) {
		a, b, arb := conflictVoters(t)

		otherSideFirst := false

		b.beforeCAS = func(*fakeVoter) {
			value, _ := a.state()
			otherSideFirst = string(value) != string(stateJSON(t, sideA))
		}

		_, err := newCoordinator(t, time.Second, a, b, arb).Resolve(t.Context(), sideB, resolveTo(9, false), forcedQuorum())
		require.NoError(t, err)
		assert.True(t, otherSideFirst, "dc_a holds generation 9 before dc_b is written")
	})

	t.Run("winner side not written", func(t *testing.T) {
		a, b, arb := conflictVoters(t)
		b.casErr = errUnreachable("dc_b")
		co := newCoordinator(t, time.Second, a, b, arb)

		res, err := co.Resolve(t.Context(), sideB, resolveTo(9, false), forcedQuorum())
		require.NoError(t, err)
		assert.Equal(t, []string{"dc_a", coord.ArbiterName}, res.Accepted())

		view, err := co.Read(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "dc_b", view.Effective.State.ActiveDatacenter)
	})

	t.Run("other side not written", func(t *testing.T) {
		a, b, arb := conflictVoters(t)
		a.casErr = fmt.Errorf("%w: connection reset", coord.ErrOutcomeUnknown)
		co := newCoordinator(t, time.Second, a, b, arb)

		res, err := co.Resolve(t.Context(), sideB, resolveTo(9, false), forcedQuorum())
		require.ErrorIs(t, err, coord.ErrUndetermined)
		assert.Equal(t, coord.WriteSkipped, writeOutcomes(res)["dc_b"])
		requireNoCAS(t, b)

		view, err := co.Read(t.Context())
		require.NoError(t, err)
		assert.Len(t, view.Conflict, 2, "dc_a is never left the only valid record")
	})
}
