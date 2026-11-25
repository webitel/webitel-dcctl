package kv_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata")

var (
	sampleState = kv.State{
		Generation:       8,
		ActiveDatacenter: "dc_b",
		OpID:             "4f0c9a52-7d1e-4b8a-9f3c-2a6e5d7b1c08",
		Forced:           true,
		PatroniTimeline:  5,
		NeedsRecovery:    []string{"dc_a"},
		UpdatedAt:        time.Date(2026, 11, 23, 15, 30, 0, 0, time.UTC),
	}
	sampleLease = kv.Lease{
		Holder:     "dc_b",
		Generation: 8,
		Counter:    1042,
		Fencing:    false,
	}
)

func TestGolden(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		decoded any
	}{
		{name: "state", value: &sampleState, decoded: &kv.State{}},
		{name: "state_minimal", value: &kv.State{
			Generation:       1,
			ActiveDatacenter: "dc_a",
			OpID:             "9e2b7c41-0a5d-4f6e-8b3a-1c4d2e5f6a70",
			PatroniTimeline:  1,
			UpdatedAt:        time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC),
		}, decoded: &kv.State{}},
		{name: "lease", value: &sampleLease, decoded: &kv.Lease{}},
		{name: "lock", value: &kv.Lock{
			Instance:   "dcctl-b1",
			Datacenter: "dc_b",
			Operation:  "4f0c9a52-7d1e-4b8a-9f3c-2a6e5d7b1c08",
		}, decoded: &kv.Lock{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("testdata", tt.name+".json")

			got, err := json.MarshalIndent(tt.value, "", "  ")
			require.NoError(t, err)

			got = append(got, '\n')

			if *update {
				require.NoError(t, os.WriteFile(path, got, 0o600))
			}

			want, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))

			require.NoError(t, json.Unmarshal(want, tt.decoded))
			assert.Equal(t, tt.value, tt.decoded)
		})
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	tests := []struct {
		name    string
		want    any
		decoded any
	}{
		{name: "state_unknown_fields", want: &sampleState, decoded: &kv.State{}},
		{name: "lease_unknown_fields", want: &sampleLease, decoded: &kv.Lease{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tt.name+".json"))
			require.NoError(t, err)

			require.NoError(t, json.Unmarshal(data, tt.decoded))
			assert.Equal(t, tt.want, tt.decoded)
		})
	}
}
