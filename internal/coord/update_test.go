package coord_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// g9 is generation 9 on dc_b after a forced failover from dc_a.
var g9 = kv.State{
	Generation:       9,
	Revision:         2,
	ActiveDatacenter: "dc_b",
	OpID:             "op9",
	Forced:           true,
	PatroniTimeline:  3,
	NeedsRecovery:    []string{"dc_a", "dc_c"},
	UpdatedAt:        time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC),
}

var g9Key = coord.RecordKey{Generation: 9, ActiveDatacenter: "dc_b"}

// updated returns the record f holds after an update of g9, see written.
func updated(t *testing.T, f *fakeVoter) kv.State {
	t.Helper()

	value, _ := f.state()

	var s kv.State
	require.NoError(t, json.Unmarshal(value, &s))
	assert.NotEqual(t, g9.UpdatedAt, s.UpdatedAt, "%s: updated_at is the time of the update", f.name)

	return written(t, f)
}

func TestSetTimeline(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, g9))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, stateJSON(t, g9))

	res, err := newCoordinator(t, time.Second, a, b, arb).SetTimeline(t.Context(), g9Key, 4)
	require.NoError(t, err)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, coord.WriteForced, res.Mode, "the mode of the record")

	want := g9
	want.Revision = 3
	want.PatroniTimeline = 4
	want.UpdatedAt = time.Time{}

	for _, f := range []*fakeVoter{a, b, arb} {
		assert.Equal(t, want, updated(t, f), "forced and needs_recovery are kept")

		_, calls := f.state()
		require.Len(t, calls, 1)
		assert.Equal(t, coord.Version(10), calls[0].version)
	}
}

func TestNeedsRecovery(t *testing.T) {
	type change func(co *coord.Coordinator) (coord.WriteResult, error)

	add := func(dc string) change {
		return func(co *coord.Coordinator) (coord.WriteResult, error) {
			return co.AddNeedsRecovery(t.Context(), g9Key, dc)
		}
	}
	remove := func(dc string) change {
		return func(co *coord.Coordinator) (coord.WriteResult, error) {
			return co.RemoveNeedsRecovery(t.Context(), g9Key, dc)
		}
	}

	tests := []struct {
		name   string
		change change
		want   []string
	}{
		{name: "add_sorted", change: add("dc_b0"), want: []string{"dc_a", "dc_b0", "dc_c"}},
		{name: "add_present", change: add("dc_a"), want: []string{"dc_a", "dc_c"}},
		{name: "remove", change: remove("dc_a"), want: []string{"dc_c"}},
		{name: "remove_absent", change: remove("dc_x"), want: []string{"dc_a", "dc_c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, stateJSON(t, g9))
			b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))

			_, err := tt.change(newCoordinator(t, time.Second, a, b))
			require.NoError(t, err)

			want := g9
			want.Revision = 3
			want.NeedsRecovery = tt.want
			want.UpdatedAt = time.Time{}

			assert.Equal(t, want, updated(t, a))
			assert.Equal(t, want, updated(t, b))
		})
	}
}

// One caller marks dc_a recovered while another adds dc_d: each change
// applies to what the voters hold, so neither undoes the other.
func TestNeedsRecoveryChangesCompose(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, g9))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))
	co := newCoordinator(t, time.Second, a, b)

	_, err := co.RemoveNeedsRecovery(t.Context(), g9Key, "dc_a")
	require.NoError(t, err)

	_, err = co.AddNeedsRecovery(t.Context(), g9Key, "dc_d")
	require.NoError(t, err)

	got := updated(t, a)
	assert.Equal(t, []string{"dc_c", "dc_d"}, got.NeedsRecovery)
	assert.Equal(t, int64(4), got.Revision)
}

func TestUpdateKeepsUnknownFields(t *testing.T) {
	stored := []byte(`{"generation":9,"revision":2,"active_datacenter":"dc_b","op_id":"op9","forced":true,` +
		`"patroni_timeline":3,"updated_at":"2026-11-26T09:00:00Z","witness":"arbiter"}`)
	a := newFake("dc_a", coord.KindKV, stored)
	b := newFake("dc_b", coord.KindKV, stored)

	_, err := newCoordinator(t, time.Second, a, b).SetTimeline(t.Context(), g9Key, 4)
	require.NoError(t, err)

	value, _ := a.state()

	var fields map[string]any
	require.NoError(t, json.Unmarshal(value, &fields))
	assert.Equal(t, "arbiter", fields["witness"])
	assert.InDelta(t, 4, fields["patroni_timeline"], 0)
}

// The voters behind the record within its generation, and the stale ones,
// get the update too.
func TestUpdateWritesVotersBehind(t *testing.T) {
	older := g9
	older.Revision = 1
	a := newFake("dc_a", coord.KindKV, stateJSON(t, older))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))
	c := newFake("dc_c", coord.KindKV, stateJSON(t, g9))
	d := newFake("dc_d", coord.KindKV, raw(t, 8, "dc_a", false))
	e := newFake("dc_e", coord.KindKV, nil)

	res, err := newCoordinator(t, time.Second, a, b, c, d, e).SetTimeline(t.Context(), g9Key, 4)
	require.NoError(t, err)
	assert.Len(t, res.Accepted(), 5)

	for _, f := range []*fakeVoter{a, b, c, d, e} {
		assert.Equal(t, int64(3), updated(t, f).Revision, f.name)
	}

	_, calls := e.state()
	assert.Equal(t, coord.Version(0), calls[0].version, "create only if absent")
}

func TestUpdateRefused(t *testing.T) {
	other := g9
	other.NeedsRecovery = []string{"dc_a"}

	tests := []struct {
		name   string
		voters [3][]byte
		key    coord.RecordKey
		want   error
	}{
		{
			name:   "not_effective",
			voters: [3][]byte{stateJSON(t, g9), stateJSON(t, g9), stateJSON(t, g9)},
			key:    coord.RecordKey{Generation: 8, ActiveDatacenter: "dc_a"},
			want:   coord.ErrBaseNotFound,
		},
		{
			name:   "unsettled",
			voters: [3][]byte{stateJSON(t, g9), stateJSON(t, other), raw(t, 8, "dc_a", false)},
			key:    g9Key,
			want:   coord.ErrUnsettled,
		},
		{
			name:   "conflict",
			voters: [3][]byte{raw(t, 9, "dc_b", true), raw(t, 9, "dc_c", true), raw(t, 8, "dc_a", false)},
			key:    g9Key,
			want:   coord.ErrConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake("dc_a", coord.KindKV, tt.voters[0])
			b := newFake("dc_b", coord.KindKV, tt.voters[1])
			c := newFake("dc_c", coord.KindKV, tt.voters[2])

			_, err := newCoordinator(t, time.Second, a, b, c).SetTimeline(t.Context(), tt.key, 4)
			require.ErrorIs(t, err, tt.want)
			requireNoCAS(t, a, b, c)
		})
	}
}

func TestUpdateValidates(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, stateJSON(t, g9))
	co := newCoordinator(t, time.Second, a)

	_, err := co.SetTimeline(t.Context(), g9Key, 0)
	require.Error(t, err)

	_, err = co.AddNeedsRecovery(t.Context(), g9Key, "dc_b")
	require.Error(t, err, "the active datacenter does not wait for recovery")

	_, err = co.AddNeedsRecovery(t.Context(), g9Key, "")
	require.Error(t, err)

	_, err = co.RemoveNeedsRecovery(t.Context(), g9Key, "")
	require.Error(t, err)

	requireNoCAS(t, a)
}

// A forced record lives on the voters that answer; an update of it on one
// of them is committed.
func TestUpdateForcedRecordOnOneVoter(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, nil)
	a.getErr = errUnreachable("10.0.0.1:8500")
	b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))

	res, err := newCoordinator(t, time.Second, a, b).SetTimeline(t.Context(), g9Key, 4)
	require.NoError(t, err)
	assert.Equal(t, coord.Committed, res.Commitment)
	assert.Equal(t, []string{"dc_b"}, res.Accepted())
}

func TestUpdateOnMinority(t *testing.T) {
	quorumRecord := g9
	quorumRecord.Forced = false
	a := newFake("dc_a", coord.KindKV, stateJSON(t, quorumRecord))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, quorumRecord))
	b.casErr = errUnreachable("10.0.0.2:8500")
	arb := newFake(coord.ArbiterName, coord.KindArbiter, stateJSON(t, quorumRecord))
	arb.casErr = errUnreachable("10.0.0.9:9443")

	res, err := newCoordinator(t, time.Second, a, b, arb).SetTimeline(t.Context(), g9Key, 4)
	require.ErrorIs(t, err, coord.ErrUncommitted)
	assert.Equal(t, coord.WriteQuorum, res.Mode)
	assert.Equal(t, []string{"dc_a"}, res.Accepted())
}

// Another instance aligned the voter with the update between the read and
// the write; updated_at is not compared.
func TestUpdateMismatchVoterHoldsUpdate(t *testing.T) {
	updated := g9
	updated.Revision = 3
	updated.PatroniTimeline = 4

	a := newFake("dc_a", coord.KindKV, stateJSON(t, g9))
	a.beforeCAS = func(f *fakeVoter) { f.set(stateJSON(t, updated)) }
	b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))

	res, err := newCoordinator(t, time.Second, a, b).SetTimeline(t.Context(), g9Key, 4)
	require.NoError(t, err)
	assert.Equal(t, []string{"dc_a", "dc_b"}, res.Accepted())
}
