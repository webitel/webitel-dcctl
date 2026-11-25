package answer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		name string
		in   check.Check
		want *dcctlv1.Check
	}{
		{
			name: "pass_block",
			in:   check.Check{ID: check.CoordLockFree, Result: check.Pass, Effect: check.Block, Detail: "the lock is free"},
			want: &dcctlv1.Check{Id: "coord.lock_free", Result: dcctlv1.Check_PASS, Effect: dcctlv1.Check_BLOCK, Detail: "the lock is free"},
		},
		{
			name: "fail_confirm",
			in: check.Check{
				ID: check.PatroniDivergence, Result: check.Fail, Effect: check.Confirm,
				Confirmation: check.ConfirmMaxDiscardBytes, Datacenter: "dc_a", DiscardBytes: new(int64(4096)),
			},
			want: &dcctlv1.Check{
				Id: "patroni.divergence", Result: dcctlv1.Check_FAIL, Effect: dcctlv1.Check_CONFIRM,
				Confirmation: "max_discard_bytes", Datacenter: "datacenters/dc_a",
			},
		},
		{
			name: "warn_warn_only",
			in:   check.Check{ID: check.NomadTargetNodes, Result: check.Warn, Effect: check.WarnOnly},
			want: &dcctlv1.Check{Id: "nomad.target_nodes", Result: dcctlv1.Check_WARN, Effect: dcctlv1.Check_WARN_ONLY},
		},
		{
			name: "skip_without_effect",
			in:   check.Check{ID: check.VerifyJobs, Result: check.Skip},
			want: &dcctlv1.Check{Id: "verify.jobs", Result: dcctlv1.Check_SKIP},
		},
		{
			name: "unknown_result_and_effect",
			in:   check.Check{ID: "x.y", Result: "maybe", Effect: "later"},
			want: &dcctlv1.Check{Id: "x.y"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := answer.Check(tt.in)
			require.NoError(t, err)
			assert.True(t, proto.Equal(tt.want, got), "Check() = %v; want %v", got, tt.want)
		})
	}
}

func TestCheck_facts(t *testing.T) {
	facts := map[string]any{
		"lag_bytes": int64(52428800), "holder": "dcctl-dc_a", "reachable": false,
		"voters": []any{map[string]any{"name": "dc_a", "generation": 7}},
	}

	got, err := answer.Check(check.Check{ID: check.PatroniTargetLag, Result: check.Fail, Facts: facts})
	require.NoError(t, err)

	want, err := structpb.NewStruct(facts)
	require.NoError(t, err)
	assert.True(t, proto.Equal(want, got.GetFacts()), "facts = %v; want %v", got.GetFacts(), want)
}

func TestCheck_factsStructCannotHold(t *testing.T) {
	_, err := answer.Check(check.Check{ID: check.PatroniTargetLag, Facts: map[string]any{"ch": make(chan int)}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "patroni.target_lag")
}

func TestChecks(t *testing.T) {
	got, err := answer.Checks([]check.Check{
		{ID: check.CoordLockFree, Result: check.Pass},
		{ID: check.CoordNoOperation, Result: check.Fail},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "coord.lock_free", got[0].GetId())
	assert.Equal(t, "coord.no_operation", got[1].GetId())

	_, err = answer.Checks([]check.Check{{ID: check.CoordQuorum, Facts: map[string]any{"f": func() {}}}})
	require.Error(t, err)
}

func TestConfirmation(t *testing.T) {
	assert.Nil(t, answer.Confirmation(check.Confirmations{}))

	got := answer.Confirmation(check.Confirmations{Isolation: true, MaxDiscardBytes: 1024})
	want := &dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 1024}
	assert.True(t, proto.Equal(want, got), "Confirmation() = %v; want %v", got, want)
}
