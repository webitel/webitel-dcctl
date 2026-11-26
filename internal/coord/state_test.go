package coord_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/source"
)

func TestPresentState(t *testing.T) {
	value := `{"generation":4,"active_datacenter":"dc_a"}`

	vs := coord.PresentState("dc_a", coord.KindKV, []byte(value), 7)

	require.NoError(t, vs.Err)
	assert.Equal(t, "dc_a", vs.Name)
	assert.Equal(t, coord.KindKV, vs.Kind)
	assert.Equal(t, coord.StatusPresent, vs.Status)
	assert.Equal(t, coord.Version(7), vs.Version)
	assert.Equal(t, value, string(vs.Raw))
	assert.Equal(t, int64(4), vs.State.Generation)
	assert.Equal(t, "dc_a", vs.State.ActiveDatacenter)
}

func TestPresentState_invalid(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"not_json", `{`},
		{"no_active_datacenter", `{"generation":4}`},
		{"negative_revision", `{"generation":4,"revision":-1,"active_datacenter":"dc_a"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := coord.PresentState("dc_a", coord.KindArbiter, []byte(tt.value), 7)

			require.Error(t, vs.Err)
			assert.Equal(t, coord.StatusInvalid, vs.Status)
			assert.Equal(t, source.ReasonInvalid, vs.Reason)
			assert.Equal(t, coord.KindArbiter, vs.Kind)
			assert.Equal(t, coord.Version(7), vs.Version)
			assert.Equal(t, tt.value, string(vs.Raw))
		})
	}
}

func TestEvaluate_voting(t *testing.T) {
	present := coord.PresentState("dc_a", coord.KindKV, []byte(`{"generation":4,"active_datacenter":"dc_a"}`), 1)

	v := coord.Evaluate([]coord.VoterState{
		present,
		{Name: "dc_b", Kind: coord.KindKV, Status: coord.StatusAbsent},
		{Name: "dc_c", Kind: coord.KindKV, Status: coord.StatusUnreachable},
		{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUninitialized},
	})

	assert.Equal(t, 2, v.Voting, "a KV without the key votes, an arbiter without state does not")
	assert.Equal(t, 3, v.Majority)
	assert.False(t, v.Quorum)
}
