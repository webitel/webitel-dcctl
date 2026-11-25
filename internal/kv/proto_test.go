package kv_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestEncodeProtoUsesProtoNames(t *testing.T) {
	b, err := kv.EncodeProto(&dcctlv1.Operation{
		Name:          "operations/1",
		State:         dcctlv1.Operation_RUNNING,
		RequestId:     "9e2b7c41-0a5d-4f6e-8b3a-1c4d2e5f6a70",
		NewGeneration: 8,
	})
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"name": "operations/1",
		"state": "RUNNING",
		"request_id": "9e2b7c41-0a5d-4f6e-8b3a-1c4d2e5f6a70",
		"new_generation": "8"
	}`, string(b))
}

func TestDecodeProtoDiscardsUnknown(t *testing.T) {
	var op dcctlv1.Operation

	err := kv.DecodeProto([]byte(`{"name":"operations/1","state":"PAUSED","future":{"a":1}}`), &op)
	require.NoError(t, err)

	assert.Equal(t, "operations/1", op.GetName())
	assert.Equal(t, dcctlv1.Operation_STATE_UNSPECIFIED, op.GetState(), "a state this dcctl does not know")
}

func TestPreserveUnknown(t *testing.T) {
	tests := []struct {
		name string
		prev string
		cur  string
		want string
	}{
		{
			name: "top_level",
			prev: `{"name":"operations/1","state":"RUNNING","future":{"a":1}}`,
			cur:  `{"name":"operations/1","state":"INTERRUPTED"}`,
			want: `{"name":"operations/1","state":"INTERRUPTED","future":{"a":1}}`,
		},
		{
			name: "nested_message",
			prev: `{"leader":{"node":"n1","zone":"z1"},"switch_datacenter":{"name":"datacenters/dc_b","speed":2}}`,
			cur:  `{"leader":{"node":"n2"},"switch_datacenter":{"name":"datacenters/dc_b"}}`,
			want: `{"leader":{"node":"n2","zone":"z1"},"switch_datacenter":{"name":"datacenters/dc_b","speed":2}}`,
		},
		{
			name: "list_by_index",
			prev: `{"steps":[{"index":0,"retries":1},{"index":1,"retries":2},{"index":2,"retries":3}]}`,
			cur:  `{"steps":[{"state":"SUCCEEDED"},{"index":1}]}`,
			want: `{"steps":[{"state":"SUCCEEDED","retries":1},{"index":1,"retries":2}]}`,
		},
		{
			name: "list_insert_by_identity",
			prev: `{"steps":[{"type":"APPS_PREFLIGHT"},{"index":1,"type":"PATRONI_DEMOTE","retry":"demote"}]}`,
			cur:  `{"steps":[{"type":"APPS_PREFLIGHT"},{"index":1,"type":"APPS_STOP"},{"index":2,"type":"PATRONI_DEMOTE"}]}`,
			want: `{"steps":[{"type":"APPS_PREFLIGHT"},{"index":1,"type":"APPS_STOP"},{"index":2,"type":"PATRONI_DEMOTE"}]}`,
		},
		{
			name: "list_index_as_string",
			prev: `{"steps":[{"index":"1","retry":"x"}]}`,
			cur:  `{"steps":[{"index":1}]}`,
			want: `{"steps":[{"index":1,"retry":"x"}]}`,
		},
		{
			name: "list_without_match",
			prev: `{"steps":[{"index":5,"retry":"x"}]}`,
			cur:  `{"steps":[{"index":1}]}`,
			want: `{"steps":[{"index":1}]}`,
		},
		{
			name: "check_by_id_and_datacenter",
			prev: `{"checks":[{"id":"db.lag","datacenter":"datacenters/dc_b","seen":1},{"id":"db.lag","datacenter":"datacenters/dc_c","seen":2}]}`,
			cur:  `{"checks":[{"id":"db.lag","datacenter":"datacenters/dc_c"},{"id":"db.lag","datacenter":"datacenters/dc_b"}]}`,
			want: `{"checks":[{"id":"db.lag","datacenter":"datacenters/dc_c","seen":2},{"id":"db.lag","datacenter":"datacenters/dc_b","seen":1}]}`,
		},
		{
			name: "duplicate_identity_skipped",
			prev: `{"steps":[{"index":1,"retry":"a"},{"index":1,"retry":"b"}]}`,
			cur:  `{"steps":[{"index":1}]}`,
			want: `{"steps":[{"index":1}]}`,
		},
		{
			name: "unknown_enum_value_kept",
			prev: `{"state":"RUNNING","steps":[{"type":"PATRONI_NEW_THING","state":"RUNNING"}]}`,
			cur:  `{"state":"INTERRUPTED","steps":[{"state":"RUNNING"}]}`,
			want: `{"state":"INTERRUPTED","steps":[{"type":"PATRONI_NEW_THING","state":"RUNNING"}]}`,
		},
		{
			name: "unknown_enum_number_kept",
			prev: `{"mode":7}`,
			cur:  `{}`,
			want: `{"mode":7}`,
		},
		{
			name: "known_enum_cleared_stays_cleared",
			prev: `{"mode":"FORCED","state":"RUNNING"}`,
			cur:  `{}`,
			want: `{}`,
		},
		{
			name: "known_field_cleared_stays_cleared",
			prev: `{"name":"operations/1","warnings":["w"],"error":{"reason":"X"}}`,
			cur:  `{"name":"operations/1"}`,
			want: `{"name":"operations/1"}`,
		},
		{
			name: "known_by_json_name",
			prev: `{"requestId":"r","newGeneration":"8"}`,
			cur:  `{}`,
			want: `{}`,
		},
		{
			name: "cur_wins",
			prev: `{"future":1}`,
			cur:  `{"future":2}`,
			want: `{"future":2}`,
		},
		{
			name: "timestamp_untouched",
			prev: `{"create_time":"2026-11-25T10:00:00Z"}`,
			cur:  `{"create_time":"2026-11-25T11:00:00Z"}`,
			want: `{"create_time":"2026-11-25T11:00:00Z"}`,
		},
		{
			name: "big_number_kept",
			prev: `{"future":12345678901234567890}`,
			cur:  `{}`,
			want: `{"future":12345678901234567890}`,
		},
	}

	desc := (&dcctlv1.Operation{}).ProtoReflect().Descriptor()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kv.PreserveUnknown(desc, []byte(tt.prev), []byte(tt.cur))
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
		})
	}
}

func TestPreserveUnknownNothingToKeep(t *testing.T) {
	desc := (&dcctlv1.Operation{}).ProtoReflect().Descriptor()
	cur := []byte(`{"name": "operations/1"}`)

	got, err := kv.PreserveUnknown(desc, []byte(`{"name":"operations/1","state":"RUNNING"}`), cur)
	require.NoError(t, err)
	assert.Equal(t, cur, got, "cur is returned as is")

	got, err = kv.PreserveUnknown(desc, nil, cur)
	require.NoError(t, err)
	assert.Equal(t, cur, got)
}

func TestPreserveUnknownInvalid(t *testing.T) {
	desc := (&dcctlv1.Operation{}).ProtoReflect().Descriptor()

	_, err := kv.PreserveUnknown(desc, []byte(`{"name":`), []byte(`{}`))
	require.Error(t, err)

	_, err = kv.PreserveUnknown(desc, []byte(`{}`), []byte(`[1]`))
	require.Error(t, err)
}
