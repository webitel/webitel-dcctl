package coord_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestBase(t *testing.T) {
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

	// want is generation/active of the base, empty for none.
	tests := []struct {
		name     string
		voters   []coord.VoterState
		failover bool
		want     string
	}{
		{
			name:   "effective",
			voters: []coord.VoterState{present("dc_a", 5, "dc_a", false), present("dc_b", 5, "dc_a", false), unreachable(coord.ArbiterName)},
			want:   "5/dc_a",
		},
		{
			name:     "failover_effective",
			voters:   []coord.VoterState{present("dc_a", 5, "dc_a", false), present("dc_b", 5, "dc_a", false), unreachable(coord.ArbiterName)},
			failover: true,
			want:     "5/dc_a",
		},
		{
			name:     "failover_undetermined_above",
			voters:   []coord.VoterState{present("dc_a", 5, "dc_a", false), unreachable("dc_b"), present(coord.ArbiterName, 6, "dc_b", false)},
			failover: true,
			want:     "6/dc_b",
		},
		{
			// A forced record and an undetermined record of the same generation:
			// the failover starts from the undetermined one, as the preflight
			// does.
			name:     "failover_undetermined_same_generation_as_forced",
			voters:   []coord.VoterState{present("dc_a", 6, "dc_a", true), unreachable("dc_b"), present(coord.ArbiterName, 6, "dc_b", false)},
			failover: true,
			want:     "6/dc_b",
		},
		{
			name:   "switch_not_resolved",
			voters: []coord.VoterState{present("dc_a", 6, "dc_a", true), unreachable("dc_b"), present(coord.ArbiterName, 6, "dc_b", false)},
			want:   "",
		},
		{
			name: "failover_undetermined_conflict",
			voters: []coord.VoterState{
				present("dc_a", 6, "dc_a", false), unreachable("dc_b"), present(coord.ArbiterName, 6, "dc_b", false),
			},
			failover: true,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			if r := coord.Base(coord.Evaluate(tt.voters), tt.failover); r != nil {
				got = fmt.Sprintf("%d/%s", r.State.Generation, r.State.ActiveDatacenter)
			}

			assert.Equal(t, tt.want, got)
		})
	}
}
