package coord_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// abandon is the revert an abandoned switchover dc_a -> dc_b makes.
func abandon(t *testing.T, co *coord.Coordinator, confirm bool) ([]coord.VoterWrite, error) {
	t.Helper()

	return co.Revert(t.Context(), g8, switchTo(false), coord.RevertOptions{ConfirmUndetermined: confirm})
}

func TestRevertUncommitted(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b, arb)

	res, err := abandon(t, co, false)
	require.NoError(t, err)
	assert.Equal(t, []coord.VoterWrite{{Voter: "dc_a", Outcome: coord.WriteAccepted}}, res)

	_, calls := a.state()
	assert.Equal(t, []casCall{{value: raw(t, 8, "dc_a", false), version: 10}}, calls)
	requireNoCAS(t, b, arb)

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, summary{Effective: "8/dc_a", Quorum: true, Initialized: true}, summarize(view))
}

// dc_b does not answer and may hold generation 9 too, which would make it
// committed: only a confirmed revert writes over it.
func TestRevertUndetermined(t *testing.T) {
	setup := func(t *testing.T) (*coord.Coordinator, *fakeVoter, *fakeVoter) {
		t.Helper()

		a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
		b := newFake("dc_b", coord.KindKV, nil)
		b.getErr = errUnreachable("10.0.0.2:8500")
		arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))

		return newCoordinator(t, time.Second, a, b, arb), a, arb
	}

	t.Run("not_confirmed", func(t *testing.T) {
		co, a, arb := setup(t)

		_, err := abandon(t, co, false)
		require.ErrorIs(t, err, coord.ErrConfirmationRequired)
		requireNoCAS(t, a, arb)
	})

	t.Run("confirmed", func(t *testing.T) {
		co, a, arb := setup(t)

		res, err := abandon(t, co, true)
		require.NoError(t, err)
		assert.Equal(t, []coord.VoterWrite{{Voter: "dc_a", Outcome: coord.WriteAccepted}}, res)

		value, _ := a.state()
		assert.Equal(t, raw(t, 8, "dc_a", false), value, "the base as the arbiter holds it")
		requireNoCAS(t, arb)
	})
}

// dc_b answers without the key: it may have taken generation 9 and lost its
// KV since, so the revert needs the confirmation too.
func TestRevertAfterKeyLoss(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, nil)
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b, arb)

	_, err := abandon(t, co, false)
	require.ErrorIs(t, err, coord.ErrConfirmationRequired)
	requireNoCAS(t, a, b, arb)

	res, err := abandon(t, co, true)
	require.NoError(t, err)
	assert.Equal(t, []coord.VoterWrite{{Voter: "dc_a", Outcome: coord.WriteAccepted}}, res)
}

// The voters of the base hold two payloads of one revision: there is no
// payload to put back.
func TestRevertUnsettledBase(t *testing.T) {
	base := kv.State{Generation: 8, ActiveDatacenter: "dc_a", OpID: "op8", PatroniTimeline: 4}
	other := base
	other.NeedsRecovery = []string{"dc_c"}

	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, base))
	c := newFake("dc_c", coord.KindKV, stateJSON(t, other))

	_, err := abandon(t, newCoordinator(t, time.Second, a, b, c), false)
	require.ErrorIs(t, err, coord.ErrUnsettled)
	requireNoCAS(t, a, b, c)
}

func TestRevertValidRecordRefused(t *testing.T) {
	tests := []struct {
		name   string
		voters [2][]byte
	}{
		{name: "committed", voters: [2][]byte{stateJSON(t, switchTo(false)), stateJSON(t, switchTo(false))}},
		{name: "forced", voters: [2][]byte{stateJSON(t, switchTo(true)), raw(t, 8, "dc_a", false)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, tt.voters[0])
			b := newFake("dc_b", coord.KindKV, tt.voters[1])

			_, err := abandon(t, newCoordinator(t, time.Second, a, b), true)
			require.ErrorIs(t, err, coord.ErrCommitted)
			requireNoCAS(t, a, b)
		})
	}
}

// Another operation wrote the same generation and datacenter on dc_b; it is
// not this revert's to undo.
func TestRevertOnlyItsOperation(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, kv.State{Generation: 9, ActiveDatacenter: "dc_b", OpID: "op-other"}))
	c := newFake("dc_c", coord.KindKV, raw(t, 8, "dc_a", false))
	d := newFake("dc_d", coord.KindKV, raw(t, 8, "dc_a", false))
	e := newFake("dc_e", coord.KindKV, raw(t, 8, "dc_a", false))

	res, err := abandon(t, newCoordinator(t, time.Second, a, b, c, d, e), false)
	require.NoError(t, err)
	assert.Equal(t, []coord.VoterWrite{{Voter: "dc_a", Outcome: coord.WriteAccepted}}, res)
	requireNoCAS(t, b, c, d, e)
}

// No voter that answers holds the base: it is written as the caller passes
// it.
func TestRevertBaseNotHeld(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, nil)
	b.getErr = errUnreachable("10.0.0.2:8500")

	_, err := abandon(t, newCoordinator(t, time.Second, a, b), true)
	require.NoError(t, err)

	value, _ := a.state()
	assert.Equal(t, stateJSON(t, g8), value)
}

func TestRevertIncomplete(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	a.casErr = errUnreachable("10.0.0.1:8500")
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))

	res, err := abandon(t, newCoordinator(t, time.Second, a, b, arb), false)
	require.ErrorIs(t, err, coord.ErrRevertIncomplete)
	require.Len(t, res, 1)
	assert.Equal(t, coord.WriteFailed, res[0].Outcome)
}

// Another instance reverted the voter between the read and the write.
func TestRevertMismatchVoterReverted(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	a.beforeCAS = func(f *fakeVoter) { f.set(raw(t, 8, "dc_a", false)) }
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false))

	res, err := abandon(t, newCoordinator(t, time.Second, a, b, arb), false)
	require.NoError(t, err)
	assert.Equal(t, []coord.VoterWrite{{Voter: "dc_a", Outcome: coord.WriteAccepted}}, res)
}

func TestRevertNothingHeld(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))

	res, err := abandon(t, newCoordinator(t, time.Second, a, b), false)
	require.NoError(t, err)
	assert.Empty(t, res)
	requireNoCAS(t, a, b)
}

func TestRevertValidates(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false)))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b)

	for name, to := range map[string]kv.State{
		"generation_not_next": {Generation: 10, ActiveDatacenter: "dc_b", OpID: "op-sw"},
		"no_operation":        {Generation: 9, ActiveDatacenter: "dc_b"},
		"no_datacenter":       {Generation: 9, OpID: "op-sw"},
	} {
		_, err := co.Revert(t.Context(), g8, to, coord.RevertOptions{})
		require.Error(t, err, name)
	}

	requireNoCAS(t, a, b)
}

func TestRevertNeedsConfirmation(t *testing.T) {
	tests := []struct {
		name   string
		voters func(t *testing.T) []*fakeVoter
		want   bool
	}{
		{
			name: "uncommitted",
			voters: func(t *testing.T) []*fakeVoter {
				t.Helper()

				return []*fakeVoter{
					newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false))),
					newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false)),
					newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false)),
				}
			},
			want: false,
		},
		{
			name: "undetermined",
			voters: func(t *testing.T) []*fakeVoter {
				t.Helper()

				b := newFake("dc_b", coord.KindKV, nil)
				b.getErr = errUnreachable("10.0.0.2:8500")

				return []*fakeVoter{
					newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false))),
					b,
					newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false)),
				}
			},
			want: true,
		},
		{
			// dc_b may have taken generation 9 and lost its key since.
			name: "kv_without_key",
			voters: func(t *testing.T) []*fakeVoter {
				t.Helper()

				return []*fakeVoter{
					newFake("dc_a", coord.KindKV, stateJSON(t, switchTo(false))),
					newFake("dc_b", coord.KindKV, nil),
					newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false)),
				}
			},
			want: true,
		},
		{
			name: "not_held",
			voters: func(t *testing.T) []*fakeVoter {
				t.Helper()

				return []*fakeVoter{
					newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false)),
					newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false)),
					newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 8, "dc_a", false)),
				}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, err := newCoordinator(t, time.Second, tt.voters(t)...).Read(t.Context())
			require.NoError(t, err)

			assert.Equal(t, tt.want, coord.RevertNeedsConfirmation(view, switchTo(false)))
		})
	}
}
