package answer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

func TestDryRun_allowed(t *testing.T) {
	quorumWarn := check.Check{ID: check.CoordQuorum, Result: check.Warn, Effect: check.WarnOnly, Detail: "1 of 3 voters answer"}
	checks := []check.Check{lockFree, othersLag, quorumWarn, isolation, divergedA}
	v := check.Decide(checks, check.Confirmations{Isolation: true, MaxDiscardBytes: 4096})
	require.Equal(t, check.Allowed, v.Outcome)

	got, err := answer.DryRun(v, plan, dcctlv1.Operation_FORCED)
	require.NoError(t, err)

	wantChecks, err := answer.Checks(checks)
	require.NoError(t, err)

	want := &dcctlv1.Operation{
		Checks: wantChecks,
		Steps:  pending(plan),
		Mode:   dcctlv1.Operation_FORCED,
		Warnings: []string{
			"patroni.others_follow_source in dc_c: dc_c streams from dc_b",
			"coord.quorum: 1 of 3 voters answer",
		},
		Confirmation: &dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 4096},
	}
	assert.True(t, proto.Equal(want, got), "DryRun() = %v; want %v", got, want)
	assert.Empty(t, got.GetName())

	for _, s := range plan {
		assert.Equal(t, dcctlv1.Operation_Step_STATE_UNSPECIFIED, s.GetState(), "plan is left unchanged")
	}
}

func TestDryRun_refusedAsTheRequest(t *testing.T) {
	tests := []struct {
		name   string
		checks []check.Check
		given  check.Confirmations
	}{
		{name: "preflight_failed", checks: []check.Check{lockFree, lagTooBig, isolation}},
		{name: "confirmation_required", checks: []check.Check{lockFree, isolation, divergedC}, given: check.Confirmations{Isolation: true}},
		{name: "lock_busy", checks: []check.Check{lockHeld, lagTooBig}},
		{name: "lock_unavailable", checks: []check.Check{lockUnknown, operationUnknown}},
		{name: "operation_running", checks: []check.Check{operationRunning}},
		{name: "leader_unavailable", checks: []check.Check{leaderDown}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := check.Decide(tt.checks, tt.given)

			op, err := answer.DryRun(v, plan, dcctlv1.Operation_QUORUM)
			assert.Nil(t, op)

			want := answer.Refusal(v, plan, dcctlv1.Operation_QUORUM)
			require.Error(t, want)
			requireSameStatus(t, status.Convert(want), status.Convert(err))
		})
	}
}

// requireSameStatus compares the unpacked details: the bytes of google.protobuf.Any
// differ with the order maps are marshaled in.
func requireSameStatus(t *testing.T, want, got *status.Status) {
	t.Helper()

	assert.Equal(t, want.Code(), got.Code())
	assert.Equal(t, want.Message(), got.Message())

	wantDetails, gotDetails := want.Details(), got.Details()
	require.Len(t, gotDetails, len(wantDetails))

	for i := range wantDetails {
		w, ok := wantDetails[i].(proto.Message)
		require.True(t, ok, "detail %d of want: %v", i, wantDetails[i])

		g, ok := gotDetails[i].(proto.Message)
		require.True(t, ok, "detail %d: %v", i, gotDetails[i])

		assert.True(t, proto.Equal(w, g), "detail %d = %v; want %v", i, g, w)
	}
}

func TestDryRun_factsStructCannotHold(t *testing.T) {
	bad := lockFree
	bad.Facts = map[string]any{"ch": make(chan int)}

	op, err := answer.DryRun(check.Decide([]check.Check{bad}, check.Confirmations{}), plan, dcctlv1.Operation_QUORUM)

	assert.Nil(t, op)
	require.Error(t, err)
	_, ok := status.FromError(err)
	assert.False(t, ok, "%v is not a gRPC status", err)
}

func TestWarnings(t *testing.T) {
	v := check.Decide([]check.Check{lockFree, othersLag, lagTooBig}, check.Confirmations{})

	assert.Equal(t, []string{"patroni.others_follow_source in dc_c: dc_c streams from dc_b"}, answer.Warnings(v))
	assert.Empty(t, answer.Warnings(check.Decide([]check.Check{lockFree}, check.Confirmations{})))
}
