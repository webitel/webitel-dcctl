package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// TestRefusal_factsOfCoordinationChecks pins the facts the refusal reads to what the
// coordination checks write.
func TestRefusal_factsOfCoordinationChecks(t *testing.T) {
	tests := []struct {
		name     string
		snap     func(t *testing.T) *collect.Snapshot
		action   Action
		params   Params
		code     codes.Code
		reason   apierr.Reason
		metadata map[string]string
	}{
		{
			name: "no_kv_answers",
			snap: snap(func(s *collect.Snapshot) {
				for i := range s.Voters {
					s.Voters[i].KV = down[collect.KV]()
				}
			}),
			code: codes.Unavailable, reason: apierr.ReasonLockUnavailable,
			metadata: map[string]string{"datacenters": "dc_a,dc_b"},
		},
		{
			name: "lock_held",
			snap: snap(func(s *collect.Snapshot) { s.Voters[1].KV.Data.Lock = lockEntry("s-1") }),
			code: codes.Aborted, reason: apierr.ReasonLockBusy,
			metadata: map[string]string{"holder": "dcctl-dc_a", "datacenter": dcA, "operation": "operations/op-9"},
		},
		{
			name: "operation_running",
			snap: withOp(operation("op-2", dcctlv1.Operation_RUNNING, dcB, dcB), nil),
			code: codes.FailedPrecondition, reason: apierr.ReasonOperationRunning,
			metadata: map[string]string{"operation": "operations/op-2"},
		},
		{
			name: "operation_copy_unreadable",
			snap: snap(func(s *collect.Snapshot) {
				s.Voters[0].KV.Data.Op = &consul.KVEntry{Key: kv.OpKey, Value: []byte("{")}
			}),
			code: codes.FailedPrecondition, reason: apierr.ReasonPreflightFailed,
		},
		{
			name: "another_datacenter_leads",
			snap: snap(nil), action: Demote, params: Params{Datacenter: dcA},
			code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable,
			metadata: map[string]string{"datacenters": dcA},
		},
		{
			name: "leading_instance_critical",
			snap: snap(func(s *collect.Snapshot) {
				d, _ := s.Datacenter(dcB)
				d.Services.Data.Dcctl[0].Status = "critical"
			}),
			code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable,
			metadata: map[string]string{"instance": "dcctl-" + dcB},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, params := tt.action, tt.params
			if action == "" {
				action, params = Switch, Params{Datacenter: dcB}
			}

			v := verdict(t, newInput(tt.snap(t), action, params), check.Confirmations{})

			st, ok := status.FromError(answer.Refusal(v, nil, dcctlv1.Operation_MODE_UNSPECIFIED))
			require.True(t, ok)
			assert.Equal(t, tt.code, st.Code(), st.Message())

			var info *errdetails.ErrorInfo

			for _, d := range st.Details() {
				if d, ok := d.(*errdetails.ErrorInfo); ok {
					info = d
				}
			}

			require.NotNil(t, info)
			assert.Equal(t, string(tt.reason), info.GetReason())
			assert.Equal(t, tt.metadata, info.GetMetadata())
		})
	}
}
