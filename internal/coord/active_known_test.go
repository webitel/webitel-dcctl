package coord_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestActiveKnown(t *testing.T) {
	present := func(name string, generation int64, active string, forced bool) coord.VoterState {
		kind := coord.KindKV
		if name == coord.ArbiterName {
			kind = coord.KindArbiter
		}

		value, err := json.Marshal(kv.State{Generation: generation, ActiveDatacenter: active, Forced: forced})
		require.NoError(t, err)

		return coord.PresentState(name, kind, value, 1)
	}
	unreachable := func(name string) coord.VoterState {
		return coord.VoterState{Name: name, Kind: coord.KindKV, Status: coord.StatusUnreachable, Err: errors.New("down")}
	}
	invalid := func(name string) coord.VoterState {
		return coord.VoterState{Name: name, Kind: coord.KindKV, Status: coord.StatusInvalid}
	}

	tests := []struct {
		name   string
		voters []coord.VoterState
		want   bool
	}{
		{
			name:   "no_effective",
			voters: []coord.VoterState{unreachable("dc_a"), unreachable("dc_b"), present(coord.ArbiterName, 5, "dc_a", false)},
			want:   false,
		},
		{
			name:   "majority",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", false), present("dc_b", 5, "dc_a", false), unreachable(coord.ArbiterName)},
			want:   true,
		},
		{
			name:   "forced_others_answer",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", true), present("dc_b", 4, "dc_b", false), present(coord.ArbiterName, 4, "dc_b", false)},
			want:   true,
		},
		{
			name:   "forced_one_unreachable",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", true), present("dc_b", 4, "dc_b", false), unreachable(coord.ArbiterName)},
			want:   true,
		},
		{
			// The voters that do not answer may hold a newer generation committed by
			// them, which names another datacenter.
			name:   "forced_unreachable_majority",
			voters: []coord.VoterState{unreachable("dc_a"), present("dc_b", 5, "dc_a", true), unreachable(coord.ArbiterName)},
			want:   false,
		},
		{
			name:   "forced_invalid_and_unreachable_majority",
			voters: []coord.VoterState{invalid("dc_a"), present("dc_b", 5, "dc_a", true), unreachable(coord.ArbiterName)},
			want:   false,
		},
		{
			name:   "undetermined_same_datacenter",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", true), unreachable("dc_b"), present(coord.ArbiterName, 6, "dc_a", false)},
			want:   true,
		},
		{
			name:   "undetermined_other_datacenter",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", true), unreachable("dc_b"), present(coord.ArbiterName, 6, "dc_b", false)},
			want:   false,
		},
		{
			// The voter that does not answer and the arbiter may have
			// committed generation 5 on dc_b next to the forced one.
			name:   "forced_and_undetermined_of_its_generation",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", true), unreachable("dc_b"), present(coord.ArbiterName, 5, "dc_b", false)},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, coord.ActiveKnown(coord.Evaluate(tt.voters)))
		})
	}
}
