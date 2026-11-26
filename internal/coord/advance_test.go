package coord_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// g8 is the record raw(t, 8, "dc_a", false) holds.
var g8 = kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op8"}

// switchTo returns generation 9 on dc_b written by the operation op-sw.
func switchTo(forced bool) kv.State {
	return kv.State{Generation: 9, ActiveDatacenter: "dc_b", OpID: "op-sw", Forced: forced, PatroniTimeline: 3}
}

var (
	quorum = coord.AdvanceOptions{Mode: coord.WriteQuorum}
	forced = coord.AdvanceOptions{Mode: coord.WriteForced, IsolationConfirmed: true}
)

func errUnreachable(addr string) error {
	return source.Classify(addr, errors.New("connection refused"))
}

func writeOutcomes(res coord.WriteResult) map[string]coord.WriteOutcome {
	out := make(map[string]coord.WriteOutcome, len(res.Voters))
	for _, w := range res.Voters {
		out[w.Voter] = w.Outcome
	}

	return out
}

// written returns the record f holds, with updated_at checked to be set, as
// every write sets it, and cleared.
func written(t *testing.T, f *fakeVoter) kv.State {
	t.Helper()

	value, _ := f.state()

	var s kv.State
	require.NoError(t, json.Unmarshal(value, &s))
	assert.False(t, s.UpdatedAt.IsZero(), "%s: updated_at is the time of the write", f.name)

	return withoutTime(s)
}

// withoutTime returns s without updated_at, which a write sets.
func withoutTime(s kv.State) kv.State {
	s.UpdatedAt = time.Time{}

	return s
}

func requireNoCAS(t *testing.T, voters ...*fakeVoter) {
	t.Helper()

	for _, f := range voters {
		_, calls := f.state()
		assert.Empty(t, calls, "%s got a write", f.name)
	}
}

func TestAdvanceQuorum(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b, arb)
	to := switchTo(false)

	res, err := co.Advance(t.Context(), g8, to, quorum)
	require.NoError(t, err)

	assert.Equal(t, coord.WriteQuorum, res.Mode)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, 2, res.Majority)
	assert.Equal(t, []string{"dc_a", "dc_b", coord.ArbiterName}, res.Accepted())

	for _, f := range []*fakeVoter{a, b, arb} {
		assert.Equal(t, to, written(t, f), f.name)

		_, calls := f.state()
		require.Len(t, calls, 1, f.name)
		assert.Equal(t, coord.Version(10), calls[0].version, f.name)
	}

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	require.NotNil(t, view.Effective)
	assert.Equal(t, to, withoutTime(view.Effective.State))
}

// A voter that drops between the read and the write leaves the new
// generation on a minority: it is uncommitted and the effective state stays
// at the old one.
func TestAdvanceVoterFailsMidWrite(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	b.casErr = errUnreachable("10.0.0.2:8500")
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	arb.casErr = errUnreachable("10.0.0.9:9443")
	co := newCoordinator(t, time.Second, a, b, arb)

	res, err := co.Advance(t.Context(), g8, switchTo(false), quorum)
	require.ErrorIs(t, err, coord.ErrUncommitted)

	assert.Equal(t, coord.Uncommitted, res.Commitment)
	assert.Equal(t, map[string]coord.WriteOutcome{
		"dc_a": coord.WriteAccepted, "dc_b": coord.WriteFailed, coord.ArbiterName: coord.WriteFailed,
	}, writeOutcomes(res))

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	require.NotNil(t, view.Effective)
	assert.Equal(t, g8, view.Effective.State)
	require.Len(t, view.Uncommitted, 1)
	assert.Equal(t, []string{"dc_a"}, view.Uncommitted[0].Voters)
}

// A write that may have been applied is neither for nor against: the
// generation is undetermined, and the write is not sent again or anywhere
// else.
func TestAdvanceOutcomeUnknown(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	b.casErr = fmt.Errorf("%w: connection reset", coord.ErrOutcomeUnknown)
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	arb.casErr = errUnreachable("10.0.0.9:9443")
	co := newCoordinator(t, time.Second, a, b, arb)

	res, err := co.Advance(t.Context(), g8, switchTo(false), quorum)
	require.ErrorIs(t, err, coord.ErrUndetermined)

	assert.Equal(t, coord.Undetermined, res.Commitment)
	assert.Equal(t, coord.WriteQuorum, res.Mode)
	assert.Equal(t, coord.WriteUnknown, writeOutcomes(res)["dc_b"])
	require.ErrorIs(t, res.Voters[1].Err, coord.ErrOutcomeUnknown)

	for _, f := range []*fakeVoter{a, b, arb} {
		_, calls := f.state()
		assert.Len(t, calls, 1, "%s got more than one write", f.name)
	}
}

func TestAdvanceVoterHangsMidWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
		b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
		b.blockCAS = true
		arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
		co := newCoordinator(t, 3*time.Second, a, b, arb)
		start := time.Now()

		res, err := co.Advance(t.Context(), g8, switchTo(false), quorum)
		require.NoError(t, err)

		assert.Equal(t, 3*time.Second, time.Since(start), "the write waits for the voter timeout only")
		assert.Equal(t, coord.Committed, res.Commitment)
		assert.Equal(t, coord.WriteUnknown, writeOutcomes(res)["dc_b"])
	})
}

// 2 datacenters without an arbiter, dc_b is down: only a forced write is
// possible, and only with the isolation confirmation.
func TestAdvanceForced(t *testing.T) {
	setup := func(t *testing.T) (*coord.Coordinator, *fakeVoter, *fakeVoter) {
		t.Helper()

		a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
		b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
		b.getErr = errUnreachable("10.0.0.2:8500")

		return newCoordinator(t, time.Second, a, b), a, b
	}

	t.Run("without_confirmation", func(t *testing.T) {
		co, a, b := setup(t)

		_, err := co.Advance(t.Context(), g8, switchTo(true), coord.AdvanceOptions{Mode: coord.WriteForced})
		require.ErrorIs(t, err, coord.ErrConfirmationRequired)
		requireNoCAS(t, a, b)
	})

	t.Run("quorum_requested", func(t *testing.T) {
		co, a, b := setup(t)

		_, err := co.Advance(t.Context(), g8, switchTo(false), quorum)
		require.ErrorIs(t, err, coord.ErrModeChanged)
		requireNoCAS(t, a, b)
	})

	t.Run("with_confirmation", func(t *testing.T) {
		co, a, b := setup(t)
		to := switchTo(true)

		res, err := co.Advance(t.Context(), g8, to, forced)
		require.NoError(t, err)

		assert.Equal(t, coord.WriteForced, res.Mode)
		assert.Equal(t, coord.Committed, res.Commitment)
		assert.Equal(t, []string{"dc_a"}, res.Accepted())
		assert.Equal(t, coord.WriteSkipped, writeOutcomes(res)["dc_b"])
		requireNoCAS(t, b)
		assert.Equal(t, to, written(t, a))
	})
}

// A majority can take the write, so it is a quorum write even when the
// caller asks for a forced one.
func TestAdvanceForcedWithMajority(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))

	_, err := newCoordinator(t, time.Second, a, b).Advance(t.Context(), g8, switchTo(true), forced)
	require.ErrorIs(t, err, coord.ErrModeChanged)
	requireNoCAS(t, a, b)
}

func TestAdvanceAnotherInstanceAdvancedFirst(t *testing.T) {
	other := stateJSON(t, kv.State{Generation: 9, ActiveDatacenter: "dc_c", OpID: "op-other"})

	t.Run("before_the_read", func(t *testing.T) {
		a := newFake("dc_a", coord.KindKV, other)
		b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
		c := newFake("dc_c", coord.KindKV, raw(t, 8, "dc_a", false))

		_, err := newCoordinator(t, time.Second, a, b, c).Advance(t.Context(), g8, switchTo(false), quorum)
		require.ErrorIs(t, err, coord.ErrSuperseded)
		requireNoCAS(t, a, b, c)
	})

	t.Run("between_the_read_and_the_write", func(t *testing.T) {
		a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
		b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
		c := newFake("dc_c", coord.KindKV, raw(t, 8, "dc_a", false))

		for _, f := range []*fakeVoter{a, b} {
			f.beforeCAS = func(f *fakeVoter) { f.set(other) }
		}

		res, err := newCoordinator(t, time.Second, a, b, c).Advance(t.Context(), g8, switchTo(false), quorum)
		require.ErrorIs(t, err, coord.ErrUncommitted)

		assert.Equal(t, map[string]coord.WriteOutcome{
			"dc_a": coord.WriteRejected, "dc_b": coord.WriteRejected, "dc_c": coord.WriteAccepted,
		}, writeOutcomes(res))
		require.ErrorIs(t, res.Voters[0].Err, coord.ErrVersionMismatch)

		value, _ := a.state()
		assert.Equal(t, other, value, "the record of the other instance was overwritten")
	})
}

func TestAdvanceOverOtherRecordOfBaseGeneration(t *testing.T) {
	lost := stateJSON(t, kv.State{Generation: 8, ActiveDatacenter: "dc_c", OpID: "op-x"})

	t.Run("base_on_majority", func(t *testing.T) {
		a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
		b := newFake("dc_b", coord.KindKV, lost)
		arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))

		res, err := newCoordinator(t, time.Second, a, b, arb).Advance(t.Context(), g8, switchTo(false), quorum)
		require.NoError(t, err)
		assert.Equal(t, []string{"dc_a", "dc_b", coord.ArbiterName}, res.Accepted(), "the record that lost is written over")
	})

	t.Run("forced_base", func(t *testing.T) {
		forcedBase := kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op8", Forced: true}
		a := newFake("dc_a", coord.KindKV, stateJSON(t, forcedBase))
		b := newFake("dc_b", coord.KindKV, lost)
		arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)
		arb.getErr = errUnreachable("10.0.0.9:9443")

		_, err := newCoordinator(t, time.Second, a, b, arb).Advance(t.Context(), forcedBase, switchTo(true), forced)
		require.ErrorIs(t, err, coord.ErrSuperseded, "with the arbiter the other record may be committed")
		requireNoCAS(t, a, b, arb)
	})
}

// Another instance aligned the voter with the new generation between the
// read and the write: the CAS fails, but the voter holds the record.
func TestAdvanceMismatchVoterHoldsTarget(t *testing.T) {
	to := switchTo(false)
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	b.beforeCAS = func(f *fakeVoter) { f.set(stateJSON(t, to)) }
	c := newFake("dc_c", coord.KindKV, raw(t, 8, "dc_a", false))

	res, err := newCoordinator(t, time.Second, a, b, c).Advance(t.Context(), g8, to, quorum)
	require.NoError(t, err)
	assert.Equal(t, []string{"dc_a", "dc_b", "dc_c"}, res.Accepted())
}

// An arbiter without state does not vote: in 2 datacenters with dc_b down
// the write is forced, and the arbiter is not written.
func TestAdvanceUninitializedArbiter(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, nil)
	b.getErr = errUnreachable("10.0.0.2:8500")
	arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)
	arb.uninitialized = true
	co := newCoordinator(t, time.Second, a, b, arb)

	_, err := co.Advance(t.Context(), g8, switchTo(false), quorum)
	require.ErrorIs(t, err, coord.ErrModeChanged)

	res, err := co.Advance(t.Context(), g8, switchTo(true), forced)
	require.NoError(t, err)
	assert.Equal(t, []string{"dc_a"}, res.Accepted())
	require.ErrorIs(t, res.Voters[2].Err, coord.ErrUninitialized)
	requireNoCAS(t, arb)
}

// An arbiter that answers without the record is never sent a create, even
// though a majority takes the write without it.
func TestAdvanceNeverCreatesArbiterRecord(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)

	res, err := newCoordinator(t, time.Second, a, b, arb).Advance(t.Context(), g8, switchTo(false), quorum)
	require.NoError(t, err)
	assert.Equal(t, coord.WriteSkipped, writeOutcomes(res)[coord.ArbiterName])
	requireNoCAS(t, arb)
}

// Voters behind the base take the new generation directly; a KV without the
// key gets it created, never an arbiter.
func TestAdvanceWritesVotersBehind(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	c := newFake("dc_c", coord.KindKV, raw(t, 8, "dc_a", false))
	d := newFake("dc_d", coord.KindKV, raw(t, 7, "dc_b", false))
	e := newFake("dc_e", coord.KindKV, nil)
	to := switchTo(false)

	res, err := newCoordinator(t, time.Second, a, b, c, d, e).Advance(t.Context(), g8, to, quorum)
	require.NoError(t, err)
	assert.Len(t, res.Accepted(), 5)

	for _, f := range []*fakeVoter{d, e} {
		assert.Equal(t, to, written(t, f), f.name)
	}

	_, calls := d.state()
	assert.Equal(t, coord.Version(10), calls[0].version)

	_, calls = e.state()
	assert.Equal(t, coord.Version(0), calls[0].version, "create only if absent")
}

// The base of a failover may be a generation that reached only some voters:
// writing over it on a majority commits the new one.
func TestAdvanceFromUndeterminedBase(t *testing.T) {
	g9 := kv.State{Generation: 9, ActiveDatacenter: "dc_a", OpID: "op9"}
	a := newFake("dc_a", coord.KindKV, stateJSON(t, g9))
	b := newFake("dc_b", coord.KindKV, nil)
	b.getErr = errUnreachable("10.0.0.2:8500")
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b, arb)
	to := kv.State{Generation: 10, ActiveDatacenter: "dc_a", OpID: "op-fo"}

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	require.Nil(t, view.Effective)

	mode, err := coord.AdvanceMode(view, g9, to)
	require.NoError(t, err)
	assert.Equal(t, coord.WriteQuorum, mode, "dc_a and the arbiter are a majority")

	res, err := co.Advance(t.Context(), g9, to, quorum)
	require.NoError(t, err)
	assert.Equal(t, []string{"dc_a", coord.ArbiterName}, res.Accepted())
}

func TestAdvanceBaseRefused(t *testing.T) {
	base := kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op8", PatroniTimeline: 4}
	recovering := base
	recovering.NeedsRecovery = []string{"dc_c"}

	tests := []struct {
		name   string
		voters [3][]byte
		from   kv.State
		want   error
	}{
		{
			name:   "unsettled",
			voters: [3][]byte{raw(t, 7, "dc_a", false), stateJSON(t, base), stateJSON(t, recovering)},
			from:   base,
			want:   coord.ErrUnsettled,
		},
		{
			name:   "changed",
			voters: [3][]byte{stateJSON(t, base), stateJSON(t, base), stateJSON(t, base)},
			from:   kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op8", PatroniTimeline: 3},
			want:   coord.ErrBaseChanged,
		},
		{
			name:   "not_held",
			voters: [3][]byte{stateJSON(t, base), stateJSON(t, base), stateJSON(t, base)},
			from:   kv.State{Generation: 7, ActiveDatacenter: "dc_a", OpID: "op7"},
			want:   coord.ErrBaseNotFound,
		},
		{
			name:   "uncommitted",
			voters: [3][]byte{raw(t, 9, "dc_c", false), stateJSON(t, base), stateJSON(t, base)},
			from:   kv.State{Generation: 9, ActiveDatacenter: "dc_c", OpID: "op9"},
			want:   coord.ErrBaseNotFound,
		},
		{
			name:   "conflict",
			voters: [3][]byte{raw(t, 8, "dc_a", true), raw(t, 8, "dc_b", true), raw(t, 7, "dc_a", false)},
			from:   kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op8", Forced: true},
			want:   coord.ErrConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, tt.voters[0])
			b := newFake("dc_b", coord.KindKV, tt.voters[1])
			c := newFake("dc_c", coord.KindKV, tt.voters[2])
			to := kv.State{Generation: tt.from.Generation + 1, ActiveDatacenter: "dc_b", OpID: "op-sw"}

			_, err := newCoordinator(t, time.Second, a, b, c).Advance(t.Context(), tt.from, to, quorum)
			require.ErrorIs(t, err, tt.want)
			requireNoCAS(t, a, b, c)
		})
	}
}

// Advance again after a crash or an interrupted write: the voters that hold
// the target count, the rest are written.
func TestAdvanceAgain(t *testing.T) {
	t.Run("same_mode", func(t *testing.T) {
		to := switchTo(false)
		a := newFake("dc_a", coord.KindKV, stateJSON(t, to))
		b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
		arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))

		res, err := newCoordinator(t, time.Second, a, b, arb).Advance(t.Context(), g8, to, quorum)
		require.NoError(t, err)
		assert.Equal(t, []string{"dc_a", "dc_b", coord.ArbiterName}, res.Accepted())
		requireNoCAS(t, a)
	})

	t.Run("uncommitted_made_forced", func(t *testing.T) {
		held := switchTo(false)
		held.PatroniTimeline = 4
		held.Revision = 1
		a := newFake("dc_a", coord.KindKV, stateJSON(t, held))
		b := newFake("dc_b", coord.KindKV, nil)
		b.getErr = errUnreachable("10.0.0.2:8500")
		co := newCoordinator(t, time.Second, a, b)

		res, err := co.Advance(t.Context(), g8, switchTo(true), forced)
		require.NoError(t, err)
		assert.Equal(t, coord.Committed, res.Commitment)

		want := held
		want.Forced = true
		want.Revision = 2

		assert.Equal(t, want, written(t, a), "the payload held is kept, forced within its generation")

		view, err := co.Read(t.Context())
		require.NoError(t, err)
		require.NotNil(t, view.Effective)
		assert.Equal(t, want, withoutTime(view.Effective.State))
	})
}

func TestAdvanceValidates(t *testing.T) {
	tests := []struct {
		name string
		from kv.State
		to   kv.State
		opts coord.AdvanceOptions
	}{
		{name: "generation_not_next", from: g8, to: kv.State{Generation: 10, ActiveDatacenter: "dc_b", OpID: "op"}, opts: quorum},
		{name: "revision_set", from: g8, to: kv.State{Generation: 9, Revision: 1, ActiveDatacenter: "dc_b", OpID: "op"}, opts: quorum},
		{name: "no_datacenter", from: g8, to: kv.State{Generation: 9, OpID: "op"}, opts: quorum},
		{name: "no_operation", from: g8, to: kv.State{Generation: 9, ActiveDatacenter: "dc_b"}, opts: quorum},
		{name: "forced_flag_in_quorum", from: g8, to: switchTo(true), opts: quorum},
		{name: "quorum_flag_in_forced", from: g8, to: switchTo(false), opts: forced},
		{name: "unknown_mode", from: g8, to: switchTo(false), opts: coord.AdvanceOptions{Mode: "auto"}},
		{
			name: "active_needs_recovery", from: g8, opts: quorum,
			to: kv.State{Generation: 9, ActiveDatacenter: "dc_b", OpID: "op", NeedsRecovery: []string{"dc_b"}},
		},
		{name: "no_base", from: kv.State{}, to: kv.State{Generation: 1, ActiveDatacenter: "dc_b", OpID: "op"}, opts: quorum},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
			b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))

			_, err := newCoordinator(t, time.Second, a, b).Advance(t.Context(), tt.from, tt.to, tt.opts)
			require.Error(t, err)
			requireNoCAS(t, a, b)
		})
	}
}

// needs_recovery is a set: written sorted and without duplicates.
func TestAdvanceSortsNeedsRecovery(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	to := switchTo(false)
	to.NeedsRecovery = []string{"dc_c", "dc_a", "dc_c"}

	_, err := newCoordinator(t, time.Second, a, b).Advance(t.Context(), g8, to, quorum)
	require.NoError(t, err)

	want := to
	want.NeedsRecovery = []string{"dc_a", "dc_c"}

	assert.Equal(t, want, written(t, a))
}

// A record written again for the same operation must be the one the voters
// hold: two payloads of one revision would not be ordered.
func TestAdvanceAgainWithOtherPayload(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))

	again := switchTo(false)
	again.NeedsRecovery = []string{"dc_c"}

	_, err := newCoordinator(t, time.Second, a, b, arb).Advance(t.Context(), g8, again, quorum)
	require.ErrorIs(t, err, coord.ErrBaseChanged)
	requireNoCAS(t, a, b, arb)
}

// A forced record held by a voter is valid, so a quorum write that reaches
// no other voter is still committed.
func TestAdvanceAgainHeldForced(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(true)))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	b.casErr = errUnreachable("10.0.0.2:8500")
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	arb.casErr = errUnreachable("10.0.0.9:9443")

	res, err := newCoordinator(t, time.Second, a, b, arb).Advance(t.Context(), g8, switchTo(false), quorum)
	require.NoError(t, err)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, []string{"dc_a"}, res.Accepted())
}

// dc_c did not answer the read and may hold the record; dc_b took another
// one in between; only dc_a took it.
func TestAdvanceUnknownRejectedAccepted(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	b.beforeCAS = func(f *fakeVoter) { f.set(raw(t, 9, "dc_c", false)) }
	c := newFake("dc_c", coord.KindKV, nil)
	c.getErr = errUnreachable("10.0.0.3:8500")

	res, err := newCoordinator(t, time.Second, a, b, c).Advance(t.Context(), g8, switchTo(false), quorum)
	require.ErrorIs(t, err, coord.ErrUndetermined)
	assert.Equal(t, coord.Undetermined, res.Commitment)
	assert.Equal(t, map[string]coord.WriteOutcome{
		"dc_a": coord.WriteAccepted, "dc_b": coord.WriteRejected, "dc_c": coord.WriteSkipped,
	}, writeOutcomes(res))
}

// The context ended while the voters were written: the result is returned
// with the error.
func TestAdvanceCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	a.beforeCAS = func(*fakeVoter) { cancel() }
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))

	res, err := newCoordinator(t, time.Second, a, b).Advance(ctx, g8, switchTo(false), quorum)
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, res.Voters, 2)
}

func TestAdvanceMode(t *testing.T) {
	to := switchTo(false)
	stale := kvAt(t, "dc_b", 8, "dc_a")
	stale.Status = coord.StatusStale

	tests := []struct {
		name   string
		voters []coord.VoterState
		want   coord.WriteMode
	}{
		{
			name:   "all_reachable",
			voters: []coord.VoterState{present(t, "dc_a", coord.KindKV, g8), present(t, "dc_b", coord.KindKV, g8)},
			want:   coord.WriteQuorum,
		},
		{
			name: "one_of_three_down",
			voters: []coord.VoterState{
				present(t, "dc_a", coord.KindKV, g8), down("dc_b", coord.KindKV), present(t, coord.ArbiterName, coord.KindArbiter, g8),
			},
			want: coord.WriteQuorum,
		},
		{
			name:   "one_of_two_down",
			voters: []coord.VoterState{present(t, "dc_a", coord.KindKV, g8), down("dc_b", coord.KindKV)},
			want:   coord.WriteForced,
		},
		{
			name:   "stale_read_does_not_count",
			voters: []coord.VoterState{present(t, "dc_a", coord.KindKV, g8), stale},
			want:   coord.WriteForced,
		},
		{
			name:   "key_missing_counts",
			voters: []coord.VoterState{present(t, "dc_a", coord.KindKV, g8), absent("dc_b"), present(t, "dc_c", coord.KindKV, g8)},
			want:   coord.WriteQuorum,
		},
		{
			name: "invalid_does_not_count",
			voters: []coord.VoterState{
				present(t, "dc_a", coord.KindKV, g8), present(t, "dc_b", coord.KindKV, g8),
				{Name: "dc_c", Kind: coord.KindKV, Status: coord.StatusInvalid},
				down("dc_d", coord.KindKV),
				down("dc_e", coord.KindKV),
			},
			want: coord.WriteForced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := coord.AdvanceMode(coord.Evaluate(tt.voters), g8, to)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Two instances advance from the same base at once: the CAS of each voter
// lets one through, so at most one of them commits.
func TestAdvanceConcurrent(t *testing.T) {
	for range 20 {
		a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
		b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
		arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
		first := newCoordinator(t, time.Second, a, b, arb)
		second := newCoordinator(t, time.Second, a, b, arb)

		toB := switchTo(false)
		toC := kv.State{Generation: 9, ActiveDatacenter: "dc_c", OpID: "op-other"}

		var (
			wg         sync.WaitGroup
			resB, resC coord.WriteResult
			errB, errC error
		)

		wg.Go(func() { resB, errB = first.Advance(t.Context(), g8, toB, quorum) })
		wg.Go(func() { resC, errC = second.Advance(t.Context(), g8, toC, quorum) })
		wg.Wait()

		committedB := errB == nil && resB.Commitment == coord.Committed
		committedC := errC == nil && resC.Commitment == coord.Committed
		assert.False(t, committedB && committedC, "both committed")

		view, err := first.Read(t.Context())
		require.NoError(t, err)
		assert.Empty(t, view.Conflict)
	}
}
