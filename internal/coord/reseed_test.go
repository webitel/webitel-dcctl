package coord_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// 2 datacenters without an arbiter, the Consul of dc_b lost the key: the
// record of dc_a is on one voter of two, so no record is effective and
// nothing aligns dc_b.
func TestReseed(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, nil)
	co := newCoordinator(t, time.Second, a, b)

	view, res := readAlign(t, co)
	require.Nil(t, view.Effective)
	require.Empty(t, res)

	got, err := co.Reseed(t.Context(), "dc_b", g8)
	require.NoError(t, err)
	assert.Equal(t, coord.Alignment{Voter: "dc_b", Outcome: coord.AlignWritten}, got)

	_, calls := b.state()
	assert.Equal(t, []casCall{{value: raw(t, 8, "dc_a", false), version: 0}}, calls, "create only if absent")

	view, err = co.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, summary{Effective: "8/dc_a", Quorum: true, Initialized: true}, summarize(view))
}

func TestReseedUninitializedArbiter(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, nil)
	arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)
	arb.uninitialized = true

	got, err := newCoordinator(t, time.Second, a, b, arb).Reseed(t.Context(), "dc_b", g8)
	require.NoError(t, err)
	assert.Equal(t, coord.AlignWritten, got.Outcome)
	requireNoCAS(t, arb)
}

func TestReseedRefused(t *testing.T) {
	g8raw := raw(t, 8, "dc_a", false)
	g7raw := raw(t, 7, "dc_a", false)

	tests := []struct {
		name   string
		voters func() []*fakeVoter
		target string
		want   error
	}{
		{
			name: "arbiter",
			voters: func() []*fakeVoter {
				arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)
				arb.uninitialized = true

				return []*fakeVoter{newFake("dc_a", coord.KindKV, g8raw), newFake("dc_b", coord.KindKV, g8raw), arb}
			},
			target: coord.ArbiterName,
			want:   coord.ErrReseedRefused,
		},
		{
			name: "voter_unreachable",
			voters: func() []*fakeVoter {
				c := newFake("dc_c", coord.KindKV, nil)
				c.getErr = errUnreachable("10.0.0.3:8500")

				return []*fakeVoter{newFake("dc_a", coord.KindKV, g8raw), newFake("dc_b", coord.KindKV, nil), c}
			},
			target: "dc_b",
			want:   coord.ErrReseedRefused,
		},
		{
			name: "voter_invalid",
			voters: func() []*fakeVoter {
				return []*fakeVoter{
					newFake("dc_a", coord.KindKV, g8raw), newFake("dc_b", coord.KindKV, nil),
					newFake("dc_c", coord.KindKV, []byte(`{"generation":"9"}`)),
				}
			},
			target: "dc_b",
			want:   coord.ErrReseedRefused,
		},
		{
			name: "two_records",
			voters: func() []*fakeVoter {
				return []*fakeVoter{
					newFake("dc_a", coord.KindKV, g8raw), newFake("dc_b", coord.KindKV, nil),
					newFake("dc_c", coord.KindKV, g7raw),
				}
			},
			target: "dc_b",
			want:   coord.ErrReseedRefused,
		},
		{
			name: "target_holds_key",
			voters: func() []*fakeVoter {
				return []*fakeVoter{newFake("dc_a", coord.KindKV, g8raw), newFake("dc_b", coord.KindKV, g8raw)}
			},
			target: "dc_b",
			want:   coord.ErrReseedRefused,
		},
		{
			name: "no_record",
			voters: func() []*fakeVoter {
				return []*fakeVoter{newFake("dc_a", coord.KindKV, nil), newFake("dc_b", coord.KindKV, nil)}
			},
			target: "dc_b",
			want:   coord.ErrReseedRefused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			voters := tt.voters()

			_, err := newCoordinator(t, time.Second, voters...).Reseed(t.Context(), tt.target, g8)
			require.ErrorIs(t, err, tt.want)
			requireNoCAS(t, voters...)
		})
	}
}

func TestReseedNotConfirmed(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, nil)

	_, err := newCoordinator(t, time.Second, a, b).Reseed(t.Context(), "dc_b", kv.State{})
	require.ErrorIs(t, err, coord.ErrConfirmationRequired)
	requireNoCAS(t, a, b)
}

func TestReseedUnknownVoter(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))

	_, err := newCoordinator(t, time.Second, a).Reseed(t.Context(), "dc_x", g8)
	require.Error(t, err)
}

// The key was created between the read and the write.
func TestReseedCreatedMeanwhile(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, nil)
	b.beforeCAS = func(f *fakeVoter) { f.set(raw(t, 9, "dc_b", false)) }

	got, err := newCoordinator(t, time.Second, a, b).Reseed(t.Context(), "dc_b", g8)
	require.ErrorIs(t, err, coord.ErrVersionMismatch)
	assert.Equal(t, coord.AlignMismatch, got.Outcome)

	value, _ := b.state()
	assert.Equal(t, raw(t, 9, "dc_b", false), value)
}

// The operator confirmed the record shown to them; the voters hold another
// one now, such as a generation that an interrupted operation left.
func TestReseedOtherThanConfirmed(t *testing.T) {
	newer := g8
	newer.Revision = 1
	newer.PatroniTimeline = 4

	tests := []struct {
		name string
		held kv.State
	}{
		{name: "other_generation", held: switchTo(false)},
		{name: "other_revision", held: newer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, nil)
			b := newFake("dc_b", coord.KindKV, stateJSON(t, tt.held))

			_, err := newCoordinator(t, time.Second, a, b).Reseed(t.Context(), "dc_a", g8)
			require.ErrorIs(t, err, coord.ErrConfirmedRecordChanged)
			requireNoCAS(t, a, b)
		})
	}
}
