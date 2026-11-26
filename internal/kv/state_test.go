package kv_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestEncodeState(t *testing.T) {
	s := kv.State{
		Generation:       9,
		Revision:         2,
		ActiveDatacenter: "dc_b",
		OpID:             "op9",
		PatroniTimeline:  6,
		UpdatedAt:        time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
	}

	plain, err := json.Marshal(s)
	require.NoError(t, err)

	tests := []struct {
		name string
		prev string
		want string
	}{
		{name: "no_prev", want: string(plain)},
		{
			name: "keeps_unknown",
			prev: `{"generation":9,"revision":1,"active_datacenter":"dc_b","witness":"arbiter","quorum":{"voters":3}}`,
			want: string(plain[:len(plain)-1]) + `,"quorum":{"voters":3},"witness":"arbiter"}`,
		},
		{
			// encoding/json matches field names without case, so a kept
			// "Generation" would replace the generation of s when read.
			name: "known_field_in_other_case",
			prev: `{"Generation":12,"OP_ID":"op12","witness":"arbiter"}`,
			want: string(plain[:len(plain)-1]) + `,"witness":"arbiter"}`,
		},
		{
			name: "known_field_left_out_of_s",
			prev: `{"generation":9,"active_datacenter":"dc_b","needs_recovery":["dc_a"]}`,
			want: string(plain),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kv.EncodeState(s, []byte(tt.prev))
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestEncodeStatePrevNotObject(t *testing.T) {
	for _, prev := range []string{`[1]`, `{`, `null`} {
		_, err := kv.EncodeState(kv.State{Generation: 1, ActiveDatacenter: "dc_a"}, []byte(prev))
		require.Error(t, err, prev)
	}
}
