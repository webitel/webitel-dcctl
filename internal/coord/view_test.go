package coord_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

func stateJSON(t *testing.T, s kv.State) []byte {
	t.Helper()

	b, err := json.Marshal(s)
	require.NoError(t, err)

	return b
}

// present is a voter holding s.
func present(t *testing.T, name string, kind coord.Kind, s kv.State) coord.VoterState {
	t.Helper()

	return coord.VoterState{
		Name: name, Kind: kind, Status: coord.StatusPresent,
		State: s, Raw: stateJSON(t, s), Version: coord.Version(s.Generation + 100),
	}
}

func kvAt(t *testing.T, name string, gen int64, dc string) coord.VoterState {
	t.Helper()

	return present(t, name, coord.KindKV, kv.State{Generation: gen, ActiveDatacenter: dc})
}

func kvForced(t *testing.T, name string, gen int64, dc string) coord.VoterState {
	t.Helper()

	return present(t, name, coord.KindKV, kv.State{Generation: gen, ActiveDatacenter: dc, Forced: true})
}

func arbAt(t *testing.T, gen int64, dc string) coord.VoterState {
	t.Helper()

	return present(t, coord.ArbiterName, coord.KindArbiter, kv.State{Generation: gen, ActiveDatacenter: dc})
}

func down(name string, kind coord.Kind) coord.VoterState {
	return coord.VoterState{Name: name, Kind: kind, Status: coord.StatusUnreachable, Reason: source.ReasonTimeout}
}

func absent(name string) coord.VoterState {
	return coord.VoterState{Name: name, Kind: coord.KindKV, Status: coord.StatusAbsent}
}

func uninitArbiter() coord.VoterState {
	return coord.VoterState{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUninitialized}
}

// summary is what a test compares of a [coord.View].
type summary struct {
	Effective    string
	Uncommitted  []string
	Undetermined []string
	Conflict     []string
	Stale        []string
	Quorum       bool
	Initialized  bool
}

func records(rs []coord.Record) []string {
	if len(rs) == 0 {
		return nil
	}

	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%d/%s", r.State.Generation, r.State.ActiveDatacenter))
	}

	return out
}

func summarize(v coord.View) summary {
	s := summary{
		Uncommitted:  records(v.Uncommitted),
		Undetermined: records(v.Undetermined),
		Conflict:     records(v.Conflict),
		Quorum:       v.Quorum,
		Initialized:  v.Initialized,
	}

	if v.Effective != nil {
		s.Effective = records([]coord.Record{*v.Effective})[0]
	}

	for _, vs := range v.Voters {
		if vs.Stale {
			s.Stale = append(s.Stale, vs.Name)
		}
	}

	return s
}

func TestEvaluate(t *testing.T) {
	const (
		a = "dc_a"
		b = "dc_b"
		c = "dc_c"
	)

	kind := coord.KindKV

	tests := []struct {
		name   string
		voters []coord.VoterState
		want   summary
	}{
		// warm_standby, 2 datacenters: majority 2 of 2.
		{
			name:   "two_dc/all_reachable",
			voters: []coord.VoterState{kvAt(t, a, 8, a), kvAt(t, b, 8, a)},
			want:   summary{Effective: "8/dc_a", Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc/one_unreachable",
			voters: []coord.VoterState{kvAt(t, a, 8, a), down(b, kind)},
			want:   summary{Undetermined: []string{"8/dc_a"}, Initialized: true},
		},
		{
			// An ordinary outage, not an interrupted operation.
			name:   "two_dc/both_at_8_one_unreachable",
			voters: []coord.VoterState{down(a, kind), kvAt(t, b, 8, a)},
			want:   summary{Undetermined: []string{"8/dc_a"}, Initialized: true},
		},
		{
			name:   "two_dc/one_unreachable_forced",
			voters: []coord.VoterState{down(a, kind), kvForced(t, b, 8, b)},
			want:   summary{Effective: "8/dc_b", Initialized: true},
		},
		{
			name:   "two_dc/conflict",
			voters: []coord.VoterState{kvForced(t, a, 8, a), kvForced(t, b, 8, b)},
			want:   summary{Conflict: []string{"8/dc_a", "8/dc_b"}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc/uncommitted",
			voters: []coord.VoterState{kvAt(t, a, 9, b), kvAt(t, b, 8, a)},
			want:   summary{Uncommitted: []string{"9/dc_b", "8/dc_a"}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc/stale",
			voters: []coord.VoterState{kvAt(t, a, 7, a), kvForced(t, b, 8, b)},
			want:   summary{Effective: "8/dc_b", Stale: []string{a}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc/absent",
			voters: []coord.VoterState{absent(a), kvForced(t, b, 8, b)},
			want:   summary{Effective: "8/dc_b", Stale: []string{a}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc/lost_key",
			voters: []coord.VoterState{kvAt(t, a, 8, a), absent(b)},
			want:   summary{Uncommitted: []string{"8/dc_a"}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc/no_state",
			voters: []coord.VoterState{absent(a), absent(b)},
			want:   summary{Quorum: true},
		},
		{
			name:   "two_dc/no_state_one_unreachable",
			voters: []coord.VoterState{absent(a), down(b, kind)},
			want:   summary{},
		},

		// warm_standby, 2 datacenters and the arbiter: majority 2 of 3.
		{
			name:   "two_dc_arbiter/all_reachable",
			voters: []coord.VoterState{kvAt(t, a, 8, a), kvAt(t, b, 8, a), arbAt(t, 8, a)},
			want:   summary{Effective: "8/dc_a", Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/one_unreachable",
			voters: []coord.VoterState{kvAt(t, a, 8, a), down(b, kind), arbAt(t, 8, a)},
			want:   summary{Effective: "8/dc_a", Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/arbiter_unreachable",
			voters: []coord.VoterState{kvAt(t, a, 8, a), kvAt(t, b, 8, a), down(coord.ArbiterName, coord.KindArbiter)},
			want:   summary{Effective: "8/dc_a", Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/conflict",
			voters: []coord.VoterState{kvForced(t, a, 8, a), kvForced(t, b, 8, b), down(coord.ArbiterName, coord.KindArbiter)},
			want:   summary{Conflict: []string{"8/dc_a", "8/dc_b"}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/uncommitted",
			voters: []coord.VoterState{kvAt(t, a, 9, b), kvAt(t, b, 8, a), arbAt(t, 8, a)},
			want:   summary{Effective: "8/dc_a", Uncommitted: []string{"9/dc_b"}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/stale_arbiter",
			voters: []coord.VoterState{kvAt(t, a, 8, a), kvAt(t, b, 8, a), arbAt(t, 7, b)},
			want:   summary{Effective: "8/dc_a", Stale: []string{coord.ArbiterName}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/uninitialized_arbiter",
			voters: []coord.VoterState{kvAt(t, a, 8, a), kvAt(t, b, 8, a), uninitArbiter()},
			want:   summary{Effective: "8/dc_a", Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/uninitialized_arbiter_one_unreachable",
			voters: []coord.VoterState{kvAt(t, a, 8, a), down(b, kind), uninitArbiter()},
			want:   summary{Undetermined: []string{"8/dc_a"}, Initialized: true},
		},
		{
			// The arbiter may hold 9, making it committed: abandoning it
			// would revert a committed generation.
			name:   "two_dc_arbiter/arbiter_unreachable_may_hold_higher",
			voters: []coord.VoterState{kvAt(t, a, 9, b), kvAt(t, b, 8, a), down(coord.ArbiterName, coord.KindArbiter)},
			want:   summary{Undetermined: []string{"9/dc_b", "8/dc_a"}, Quorum: true, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/datacenter_unreachable_may_hold_higher",
			voters: []coord.VoterState{kvAt(t, a, 9, b), down(b, kind), arbAt(t, 8, a)},
			want:   summary{Undetermined: []string{"9/dc_b", "8/dc_a"}, Quorum: true, Initialized: true},
		},
		{
			name: "two_dc_arbiter/arbiter_without_state_does_not_vote",
			voters: []coord.VoterState{
				kvAt(t, a, 8, a), down(b, kind),
				{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusAbsent},
			},
			want: summary{Undetermined: []string{"8/dc_a"}, Initialized: true},
		},
		{
			name:   "two_dc_arbiter/no_state",
			voters: []coord.VoterState{absent(a), absent(b), uninitArbiter()},
			want:   summary{Quorum: true},
		},

		// warm_standby, 3 datacenters: majority 2 of 3.
		{
			name:   "three_dc/all_reachable",
			voters: []coord.VoterState{kvAt(t, a, 8, b), kvAt(t, b, 8, b), kvAt(t, c, 8, b)},
			want:   summary{Effective: "8/dc_b", Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/one_unreachable",
			voters: []coord.VoterState{kvAt(t, a, 8, b), kvAt(t, b, 8, b), down(c, kind)},
			want:   summary{Effective: "8/dc_b", Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/conflict",
			voters: []coord.VoterState{kvForced(t, a, 8, a), kvForced(t, b, 8, b), down(c, kind)},
			want:   summary{Conflict: []string{"8/dc_a", "8/dc_b"}, Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/conflict_and_uncommitted",
			voters: []coord.VoterState{kvForced(t, a, 8, a), kvForced(t, b, 8, b), kvAt(t, c, 9, c)},
			want: summary{
				Conflict: []string{"8/dc_a", "8/dc_b"}, Uncommitted: []string{"9/dc_c"},
				Quorum: true, Initialized: true,
			},
		},
		{
			name:   "three_dc/uncommitted",
			voters: []coord.VoterState{kvAt(t, a, 9, c), kvAt(t, b, 8, b), kvAt(t, c, 8, b)},
			want:   summary{Effective: "8/dc_b", Uncommitted: []string{"9/dc_c"}, Quorum: true, Initialized: true},
		},
		{
			name: "three_dc/invalid_may_hold_higher",
			voters: []coord.VoterState{
				kvAt(t, a, 9, c), kvAt(t, b, 8, b),
				{Name: c, Kind: kind, Status: coord.StatusInvalid, Reason: source.ReasonInvalid},
			},
			want: summary{Undetermined: []string{"9/dc_c", "8/dc_b"}, Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/stale",
			voters: []coord.VoterState{kvAt(t, a, 8, b), kvAt(t, b, 8, b), kvAt(t, c, 7, c)},
			want:   summary{Effective: "8/dc_b", Stale: []string{c}, Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/forced_lower_generation_is_stale",
			voters: []coord.VoterState{kvAt(t, a, 9, a), kvAt(t, b, 9, a), kvForced(t, c, 8, c)},
			want:   summary{Effective: "9/dc_a", Stale: []string{c}, Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/absent",
			voters: []coord.VoterState{kvAt(t, a, 8, b), absent(b), kvAt(t, c, 8, b)},
			want:   summary{Effective: "8/dc_b", Stale: []string{b}, Quorum: true, Initialized: true},
		},
		{
			name:   "three_dc/no_state",
			voters: []coord.VoterState{absent(a), absent(b), absent(c)},
			want:   summary{Quorum: true},
		},

		// stretch: one voter, majority 1 of 1.
		{
			name:   "stretch/reachable",
			voters: []coord.VoterState{kvAt(t, coord.StretchName, 8, a)},
			want:   summary{Effective: "8/dc_a", Quorum: true, Initialized: true},
		},
		{
			name:   "stretch/unreachable",
			voters: []coord.VoterState{down(coord.StretchName, kind)},
			want:   summary{},
		},
		{
			name:   "stretch/forced",
			voters: []coord.VoterState{kvForced(t, coord.StretchName, 8, b)},
			want:   summary{Effective: "8/dc_b", Quorum: true, Initialized: true},
		},
		{
			name:   "stretch/no_state",
			voters: []coord.VoterState{absent(coord.StretchName)},
			want:   summary{Quorum: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coord.Evaluate(tt.voters)
			assert.Equal(t, tt.want, summarize(got))
			assert.Equal(t, len(tt.voters)/2+1, got.Majority)
		})
	}
}

func TestEvaluateRecordVoters(t *testing.T) {
	v := coord.Evaluate([]coord.VoterState{
		kvAt(t, "dc_a", 9, "dc_b"), kvAt(t, "dc_b", 8, "dc_a"), arbAt(t, 8, "dc_a"),
	})

	require.NotNil(t, v.Effective)
	assert.Equal(t, []string{"dc_b", coord.ArbiterName}, v.Effective.Voters)
	require.Len(t, v.Uncommitted, 1)
	assert.Equal(t, []string{"dc_a"}, v.Uncommitted[0].Voters)
}

// Updates within a generation (timeline, needs_recovery, uncommitted to
// forced) reach voters one by one; until they all have it, the voters hold
// one record with different payloads, which must keep its majority.
func TestEvaluateSameGenerationDivergentPayload(t *testing.T) {
	base := kv.State{Generation: 8, ActiveDatacenter: "dc_b", OpID: "op1", PatroniTimeline: 4}
	updated := base
	updated.PatroniTimeline = 5
	updated.NeedsRecovery = []string{"dc_a"}

	t.Run("most_common_payload", func(t *testing.T) {
		v := coord.Evaluate([]coord.VoterState{
			present(t, "dc_a", coord.KindKV, updated),
			present(t, "dc_b", coord.KindKV, base),
			present(t, coord.ArbiterName, coord.KindArbiter, base),
		})

		require.NotNil(t, v.Effective)
		assert.Equal(t, base, v.Effective.State)
		assert.Equal(t, stateJSON(t, base), v.Effective.Raw)
		assert.True(t, v.Effective.Settled, "the payload is on a majority of all voters")
		assert.Equal(t, []string{"dc_a", "dc_b", coord.ArbiterName}, v.Effective.Voters)
		assert.Empty(t, v.Uncommitted)
	})

	t.Run("majority_without_any_equal_payload", func(t *testing.T) {
		v := coord.Evaluate([]coord.VoterState{
			present(t, "dc_a", coord.KindKV, updated),
			present(t, "dc_b", coord.KindKV, base),
		})

		require.NotNil(t, v.Effective, "the generation keeps its majority")
		assert.Equal(t, updated, v.Effective.State, "a tie goes to the first voter")
		assert.False(t, v.Effective.Settled)
	})

	// Failover dc_a -> dc_c with dc_a down: 9 is on dc_b and dc_c, and
	// needs_recovery reached only dc_c. Neither payload may be taken as
	// the one to align dc_a with.
	t.Run("tie_with_stale_voter", func(t *testing.T) {
		g9 := kv.State{Generation: 9, ActiveDatacenter: "dc_c", OpID: "op9"}
		recovering := g9
		recovering.NeedsRecovery = []string{"dc_a"}

		v := coord.Evaluate([]coord.VoterState{
			kvAt(t, "dc_a", 8, "dc_a"),
			present(t, "dc_b", coord.KindKV, g9),
			present(t, "dc_c", coord.KindKV, recovering),
		})

		require.NotNil(t, v.Effective)
		assert.Equal(t, int64(9), v.Effective.State.Generation)
		assert.False(t, v.Effective.Settled)
		assert.True(t, v.Voters[0].Stale)
	})

	t.Run("forced_first", func(t *testing.T) {
		forced := base
		forced.Forced = true

		v := coord.Evaluate([]coord.VoterState{
			present(t, "dc_a", coord.KindKV, base),
			present(t, "dc_b", coord.KindKV, base),
			present(t, coord.ArbiterName, coord.KindArbiter, forced),
		})

		require.NotNil(t, v.Effective)
		assert.Equal(t, forced, v.Effective.State)
		assert.False(t, v.Effective.Settled, "the forced flag reached one voter of three")
	})

	t.Run("single_forced_voter", func(t *testing.T) {
		forced := base
		forced.Forced = true

		v := coord.Evaluate([]coord.VoterState{
			down("dc_a", coord.KindKV),
			present(t, "dc_b", coord.KindKV, forced),
		})

		require.NotNil(t, v.Effective)
		assert.True(t, v.Effective.Settled, "every voter of the record has its payload")
	})

	t.Run("updated_at_is_not_payload", func(t *testing.T) {
		later := base
		later.UpdatedAt = later.UpdatedAt.AddDate(0, 0, 1)

		v := coord.Evaluate([]coord.VoterState{
			present(t, "dc_a", coord.KindKV, later),
			present(t, "dc_b", coord.KindKV, base),
			present(t, coord.ArbiterName, coord.KindArbiter, base),
		})

		require.NotNil(t, v.Effective)
		assert.Equal(t, later, v.Effective.State, "equal payloads, so the first voter wins")
		assert.True(t, v.Effective.Settled)
	})
}

func TestEvaluateInvalidRecord(t *testing.T) {
	invalid := coord.VoterState{Name: "dc_c", Kind: coord.KindKV, Status: coord.StatusInvalid, Reason: source.ReasonInvalid}

	t.Run("not_counted_and_not_stale", func(t *testing.T) {
		v := coord.Evaluate([]coord.VoterState{kvAt(t, "dc_a", 8, "dc_b"), kvAt(t, "dc_b", 8, "dc_b"), invalid})
		assert.Equal(t, summary{Effective: "8/dc_b", Quorum: true, Initialized: true}, summarize(v))
	})

	t.Run("initialized_without_readable_state", func(t *testing.T) {
		v := coord.Evaluate([]coord.VoterState{absent("dc_a"), absent("dc_b"), invalid})
		assert.True(t, v.Initialized)
		assert.Nil(t, v.Effective)
	})
}

func TestEvaluateDoesNotModifyInput(t *testing.T) {
	in := []coord.VoterState{kvAt(t, "dc_a", 7, "dc_a"), kvForced(t, "dc_b", 8, "dc_b")}

	v := coord.Evaluate(in)

	assert.True(t, v.Voters[0].Stale)
	assert.False(t, in[0].Stale)
}
