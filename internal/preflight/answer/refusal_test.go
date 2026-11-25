package answer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

var (
	lockFree = check.Check{ID: check.CoordLockFree, Result: check.Pass, Effect: check.Block, Detail: "the lock is free"}

	operationRunning = check.Check{
		ID: check.CoordNoOperation, Result: check.Fail, Effect: check.Block,
		Detail: "operation operations/op-2 is running",
		Facts: map[string]any{
			"op_id": "operations/op-2", "status": "running", "leader": "dcctl-dc_b",
			"leader_datacenter": "dc_b", "kv": "dc_a",
		},
	}
	operationUnreadable = check.Check{
		ID: check.CoordNoOperation, Result: check.Fail, Effect: check.Block,
		Detail: "dcctl/op in dc_a cannot be read: invalid", Facts: map[string]any{"kv": "dc_a", "reason": "invalid"},
	}
	operationUnknown = check.Check{
		ID: check.CoordNoOperation, Result: check.Fail, Effect: check.Block,
		Detail: "no Consul KV answers, an operation may be running",
		Facts:  map[string]any{"reachable": false, "unreachable": []any{"dc_a", "dc_b"}},
	}

	lockHeld = check.Check{
		ID: check.CoordLockFree, Result: check.Fail, Effect: check.Block,
		Detail: `dcctl/lock in dc_b is held by "dcctl-dc_a" of dc_a`,
		Facts:  map[string]any{"kv": "dc_b", "holder": "dcctl-dc_a", "datacenter": "dc_a", "operation": "op-9"},
	}
	lockUnknown = check.Check{
		ID: check.CoordLockFree, Result: check.Fail, Effect: check.Block,
		Detail: "no Consul KV answers, the lock may be held",
		Facts:  map[string]any{"reachable": false, "unreachable": []any{"dc_a", "dc_b"}},
	}

	leaderDown = check.Check{
		ID: check.CoordLeaderReachable, Result: check.Fail, Effect: check.Block,
		Detail: "the leading instance is critical in Consul",
		Facts:  map[string]any{"instance": "dcctl-dc_b", "datacenter": "dc_b", "error": "critical"},
	}
	leaderElsewhere = check.Check{
		ID: check.CoordLeaderReachable, Result: check.Fail, Effect: check.Block,
		Detail: "this instance does not lead the action, its datacenter is dc_b",
		Facts: map[string]any{
			"instance": "dcctl-dc_b", "datacenter": "dc_b", "leader_datacenters": []any{"dc_a", "dc_c"},
		},
	}

	opNotInterrupted = check.Check{
		ID: check.OpInterrupted, Result: check.Fail, Effect: check.Block,
		Detail: "operations/op-2 is running", Facts: map[string]any{"operation": "operations/op-2", "state": "RUNNING"},
	}
	opNotFound = check.Check{
		ID: check.OpInterrupted, Result: check.Fail, Effect: check.Block,
		Detail: "operations/op-2 is not found", Facts: map[string]any{"operation": "operations/op-2", "found": false},
	}

	lagTooBig = check.Check{
		ID: check.PatroniTargetLag, Result: check.Fail, Effect: check.Block, Datacenter: "dc_b",
		Detail: "lag 52428800 B exceeds 1048576 B",
		Facts:  map[string]any{"lag_bytes": 52428800, "limit_bytes": 1048576},
	}
	notPaused = check.Check{
		ID: check.PatroniNotPaused, Result: check.Fail, Effect: check.Block, Datacenter: "dc_b",
		Detail: "pause is set",
	}
	othersLag = check.Check{
		ID: check.PatroniOthersFollowSource, Result: check.Warn, Effect: check.WarnOnly, Datacenter: "dc_c",
		Detail: "dc_c streams from dc_b",
	}

	isolation = check.Check{
		ID: check.CoordIsolation, Result: check.Fail, Effect: check.Confirm, Confirmation: check.ConfirmIsolation,
		Detail: "the old primary must be isolated",
		Facts:  map[string]any{"mode": "forced_only", "datacenters": []any{map[string]any{"datacenter": "dc_a"}}},
	}
	notMaxLSN = check.Check{
		ID: check.PatroniTargetMaxLSN, Result: check.Fail, Effect: check.Confirm, Confirmation: check.ConfirmNotMaxLSN,
		Datacenter: "dc_b", Detail: "dc_c received more WAL than dc_b",
	}
	divergedA = check.Check{
		ID: check.PatroniDivergence, Result: check.Fail, Effect: check.Confirm,
		Confirmation: check.ConfirmMaxDiscardBytes, Datacenter: "dc_a",
		Detail: "dc_a diverged by 4096 B", Facts: map[string]any{"bytes": 4096}, DiscardBytes: new(int64(4096)),
	}
	divergedC = check.Check{
		ID: check.PatroniDivergence, Result: check.Fail, Effect: check.Confirm,
		Confirmation: check.ConfirmMaxDiscardBytes, Datacenter: "dc_c",
		Detail: "dc_c diverged by 8192 B", Facts: map[string]any{"bytes": 8192}, DiscardBytes: new(int64(8192)),
	}
	divergedUnknown = check.Check{
		ID: check.PatroniDivergence, Result: check.Fail, Effect: check.Confirm,
		Confirmation: check.ConfirmMaxDiscardBytes, Datacenter: "dc_a",
		Detail: "the divergence of dc_a is not known",
	}
)

var plan = []*dcctlv1.Operation_Step{
	{Type: dcctlv1.Operation_Step_APPS_PREFLIGHT},
	{Type: dcctlv1.Operation_Step_GENERATION, PointOfNoReturn: true},
	{Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, Datacenter: "datacenters/dc_b"},
}

// refusal is a decoded refusal status.
type refusal struct {
	code       codes.Code
	message    string
	info       *errdetails.ErrorInfo
	violations []*errdetails.PreconditionFailure_Violation
	preflight  *dcctlv1.Preflight
}

func decode(t *testing.T, err error) refusal {
	t.Helper()

	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok, "%v is a gRPC status", err)

	out := refusal{code: st.Code(), message: st.Message()}

	for _, d := range st.Details() {
		switch d := d.(type) {
		case *errdetails.ErrorInfo:
			out.info = d
		case *errdetails.PreconditionFailure:
			out.violations = d.GetViolations()
		case *dcctlv1.Preflight:
			out.preflight = d
		default:
			t.Fatalf("unexpected detail %T", d)
		}
	}

	require.NotNil(t, out.info, "ErrorInfo")
	assert.Equal(t, apierr.Domain, out.info.GetDomain())

	return out
}

func violation(typ, subject, description string) *errdetails.PreconditionFailure_Violation {
	return &errdetails.PreconditionFailure_Violation{Type: typ, Subject: subject, Description: description}
}

func requireViolations(t *testing.T, want []*errdetails.PreconditionFailure_Violation, got refusal) {
	t.Helper()

	require.Len(t, got.violations, len(want), "violations: %v", got.violations)

	for i := range want {
		assert.True(t, proto.Equal(want[i], got.violations[i]), "violation %d = %v; want %v", i, got.violations[i], want[i])
	}
}

// requirePreflight asserts that the Preflight detail has every check, the plan in
// PENDING and mode.
func requirePreflight(t *testing.T, checks []check.Check, mode dcctlv1.Operation_Mode, got refusal) {
	t.Helper()

	require.NotNil(t, got.preflight, "Preflight detail")

	wantChecks, err := answer.Checks(checks)
	require.NoError(t, err)

	want := &dcctlv1.Preflight{Checks: wantChecks, Steps: pending(plan), Mode: mode}
	assert.True(t, proto.Equal(want, got.preflight), "Preflight = %v; want %v", got.preflight, want)
}

func pending(steps []*dcctlv1.Operation_Step) []*dcctlv1.Operation_Step {
	out := make([]*dcctlv1.Operation_Step, len(steps))

	for i, s := range steps {
		c := proto.CloneOf(s)
		c.Index = int32(i)
		c.State = dcctlv1.Operation_Step_PENDING
		out[i] = c
	}

	return out
}

func refuse(checks []check.Check, given check.Confirmations, mode dcctlv1.Operation_Mode) error {
	return answer.Refusal(check.Decide(checks, given), plan, mode)
}

func TestRefusal_allowed(t *testing.T) {
	require.NoError(t, refuse([]check.Check{lockFree, othersLag}, check.Confirmations{}, dcctlv1.Operation_QUORUM))
}

func TestRefusal_specialReasons(t *testing.T) {
	tests := []struct {
		name     string
		failed   check.Check
		code     codes.Code
		reason   apierr.Reason
		message  string
		metadata map[string]string
	}{
		{
			name: "operation_running", failed: operationRunning,
			code: codes.FailedPrecondition, reason: apierr.ReasonOperationRunning,
			message: "operation operations/op-2 is running", metadata: map[string]string{"operation": "operations/op-2"},
		},
		{
			name: "operation_not_interrupted", failed: opNotInterrupted,
			code: codes.FailedPrecondition, reason: apierr.ReasonOperationNotInterrupted,
			message:  "operations/op-2 is running",
			metadata: map[string]string{"operation": "operations/op-2", "state": "RUNNING"},
		},
		{
			name: "operation_not_found", failed: opNotFound,
			code: codes.NotFound, reason: apierr.ReasonOperationNotFound,
			message: "operations/op-2 is not found", metadata: map[string]string{"operation": "operations/op-2"},
		},
		{
			name: "lock_busy", failed: lockHeld,
			code: codes.Aborted, reason: apierr.ReasonLockBusy,
			message:  "dcctl/lock in dc_b is held by instance dcctl-dc_a of dc_a for operation op-9",
			metadata: map[string]string{"holder": "dcctl-dc_a", "datacenter": "datacenters/dc_a", "operation": "operations/op-9"},
		},
		{
			name: "lock_unreachable", failed: lockUnknown,
			code: codes.Unavailable, reason: apierr.ReasonLockUnavailable,
			message:  "dcctl/lock not taken: no kv answered (dc_a, dc_b)",
			metadata: map[string]string{"datacenters": "dc_a,dc_b"},
		},
		{
			name: "operation_unreachable", failed: operationUnknown,
			code: codes.Unavailable, reason: apierr.ReasonLockUnavailable,
			message:  "dcctl/lock not taken: no kv answered (dc_a, dc_b)",
			metadata: map[string]string{"datacenters": "dc_a,dc_b"},
		},
		{
			name: "leader_unavailable", failed: leaderDown,
			code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable,
			message: "the leading instance is critical in Consul", metadata: map[string]string{"instance": "dcctl-dc_b"},
		},
		{
			name: "leader_in_another_datacenter", failed: leaderElsewhere,
			code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable,
			message:  "this instance does not lead the action, its datacenter is dc_b",
			metadata: map[string]string{"datacenters": "dc_a,dc_c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := []check.Check{tt.failed, lagTooBig, isolation}

			got := decode(t, refuse(checks, check.Confirmations{}, dcctlv1.Operation_FORCED))

			assert.Equal(t, tt.code, got.code)
			assert.Equal(t, string(tt.reason), got.info.GetReason())
			assert.Equal(t, tt.message, got.message)
			assert.Equal(t, tt.metadata, got.info.GetMetadata())
			requireViolations(t, []*errdetails.PreconditionFailure_Violation{
				violation("check", string(tt.failed.ID), tt.failed.Detail),
				violation("check", "patroni.target_lag", "lag 52428800 B exceeds 1048576 B"),
				violation("confirmation", "isolation", "the old primary must be isolated"),
			}, got)
			requirePreflight(t, checks, dcctlv1.Operation_FORCED, got)
		})
	}
}

func TestRefusal_precedence(t *testing.T) {
	tests := []struct {
		name   string
		checks []check.Check
		want   apierr.Reason
	}{
		{name: "not_interrupted_over_lock", checks: []check.Check{lockHeld, opNotInterrupted}, want: apierr.ReasonOperationNotInterrupted},
		{name: "not_found_over_leader", checks: []check.Check{leaderDown, opNotFound}, want: apierr.ReasonOperationNotFound},
		{name: "operation_over_lock", checks: []check.Check{lockHeld, operationRunning}, want: apierr.ReasonOperationRunning},
		{name: "operation_over_leader", checks: []check.Check{leaderDown, operationRunning}, want: apierr.ReasonOperationRunning},
		{name: "lock_over_leader", checks: []check.Check{leaderDown, lockHeld}, want: apierr.ReasonLockBusy},
		{name: "busy_over_unreachable", checks: []check.Check{operationUnknown, lockHeld}, want: apierr.ReasonLockBusy},
		{name: "operation_over_unreachable", checks: []check.Check{lockUnknown, operationRunning}, want: apierr.ReasonOperationRunning},
		{name: "unreachable_kv_over_leader", checks: []check.Check{leaderDown, operationUnknown, lockUnknown}, want: apierr.ReasonLockUnavailable},
		{name: "unreadable_copy_lets_lock", checks: []check.Check{operationUnreadable, lockHeld}, want: apierr.ReasonLockBusy},
		{name: "unreadable_copy_lets_leader", checks: []check.Check{operationUnreadable, leaderDown}, want: apierr.ReasonLeaderUnavailable},
		{name: "special_over_blocked", checks: []check.Check{lagTooBig, leaderDown}, want: apierr.ReasonLeaderUnavailable},
		{name: "all", checks: []check.Check{leaderDown, lockHeld, lagTooBig, operationRunning}, want: apierr.ReasonOperationRunning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decode(t, refuse(tt.checks, check.Confirmations{}, dcctlv1.Operation_QUORUM))

			assert.Equal(t, string(tt.want), got.info.GetReason())
			assert.Equal(t, tt.want.Code(), got.code)
		})
	}
}

func TestRefusal_preflightFailed(t *testing.T) {
	checks := []check.Check{lockFree, lagTooBig, notPaused, othersLag, isolation, divergedA, divergedC}

	got := decode(t, refuse(checks, check.Confirmations{MaxDiscardBytes: 4096}, dcctlv1.Operation_QUORUM))

	assert.Equal(t, codes.FailedPrecondition, got.code)
	assert.Equal(t, string(apierr.ReasonPreflightFailed), got.info.GetReason())
	assert.Equal(t, "preflight failed: patroni.target_lag, patroni.not_paused", got.message)
	assert.Empty(t, got.info.GetMetadata())
	requireViolations(t, []*errdetails.PreconditionFailure_Violation{
		violation("check", "patroni.target_lag", "lag 52428800 B exceeds 1048576 B"),
		violation("check", "patroni.not_paused", "pause is set"),
		violation("confirmation", "isolation", "the old primary must be isolated"),
		violation("confirmation", "max_discard_bytes", "dc_c diverged by 8192 B; max_discard_bytes must be at least 8192"),
	}, got)
	requirePreflight(t, checks, dcctlv1.Operation_QUORUM, got)
}

func TestRefusal_preflightFailedOneIDPerDatacenter(t *testing.T) {
	inA, inB := notPaused, notPaused
	inA.Datacenter, inA.Detail = "dc_a", "pause is set in dc_a"

	got := decode(t, refuse([]check.Check{inA, inB}, check.Confirmations{}, dcctlv1.Operation_QUORUM))

	assert.Equal(t, "preflight failed: patroni.not_paused", got.message)
	requireViolations(t, []*errdetails.PreconditionFailure_Violation{
		violation("check", "patroni.not_paused", "pause is set in dc_a"),
		violation("check", "patroni.not_paused", "pause is set"),
	}, got)
}

func TestRefusal_unknownDiscardAmount(t *testing.T) {
	checks := []check.Check{lockFree, divergedUnknown}

	got := decode(t, refuse(checks, check.Confirmations{MaxDiscardBytes: 1 << 40}, dcctlv1.Operation_QUORUM))

	assert.Equal(t, codes.FailedPrecondition, got.code)
	assert.Equal(t, string(apierr.ReasonPreflightFailed), got.info.GetReason())
	requireViolations(t, []*errdetails.PreconditionFailure_Violation{
		violation("check", "patroni.divergence",
			"the divergence of dc_a is not known (the amount to discard is not known, no max_discard_bytes covers it)"),
	}, got)
	requirePreflight(t, checks, dcctlv1.Operation_QUORUM, got)
}

func TestRefusal_specialIDWithUnknownResult(t *testing.T) {
	odd := lockHeld
	odd.Result = "maybe"

	got := decode(t, refuse([]check.Check{odd}, check.Confirmations{}, dcctlv1.Operation_QUORUM))

	assert.Equal(t, string(apierr.ReasonPreflightFailed), got.info.GetReason())
}

func TestRefusal_confirmationRequired(t *testing.T) {
	checks := []check.Check{lockFree, othersLag, isolation, notMaxLSN, divergedA, divergedC}

	got := decode(t, refuse(checks, check.Confirmations{NotMaxLSN: true, MaxDiscardBytes: 4096}, dcctlv1.Operation_FORCED))

	assert.Equal(t, codes.FailedPrecondition, got.code)
	assert.Equal(t, string(apierr.ReasonConfirmationRequired), got.info.GetReason())
	assert.Equal(t, "dcctl.confirmation.required", apierr.Reason(got.info.GetReason()).ID())
	assert.Equal(t, "confirmation required: isolation, max_discard_bytes", got.message)
	requireViolations(t, []*errdetails.PreconditionFailure_Violation{
		violation("confirmation", "isolation", "the old primary must be isolated"),
		violation("confirmation", "max_discard_bytes", "dc_c diverged by 8192 B; max_discard_bytes must be at least 8192"),
	}, got)
	requirePreflight(t, checks, dcctlv1.Operation_FORCED, got)
	assert.Equal(t, "forced_only", got.preflight.GetChecks()[2].GetFacts().GetFields()["mode"].GetStringValue(),
		"facts of the confirm checks are in Preflight")
}

func TestRefusal_confirmationJoinsChecks(t *testing.T) {
	got := decode(t, refuse([]check.Check{divergedA, divergedC}, check.Confirmations{}, dcctlv1.Operation_QUORUM))

	requireViolations(t, []*errdetails.PreconditionFailure_Violation{
		violation("confirmation", "max_discard_bytes",
			"dc_a diverged by 4096 B; dc_c diverged by 8192 B; max_discard_bytes must be at least 8192"),
	}, got)
}

func TestRefusal_operationCopyUnreadable(t *testing.T) {
	checks := []check.Check{operationUnreadable, lockFree}

	got := decode(t, refuse(checks, check.Confirmations{}, dcctlv1.Operation_QUORUM))

	assert.Equal(t, codes.FailedPrecondition, got.code)
	assert.Equal(t, string(apierr.ReasonPreflightFailed), got.info.GetReason())
	assert.Equal(t, "preflight failed: coord.no_operation", got.message)
	requireViolations(t, []*errdetails.PreconditionFailure_Violation{
		violation("check", "coord.no_operation", "dcctl/op in dc_a cannot be read: invalid"),
	}, got)
}

// op.interrupted gives its own reason only when its facts tell the operation and why.
func TestRefusal_operationFactsMissing(t *testing.T) {
	tests := []struct {
		name  string
		facts map[string]any
	}{
		{name: "no_kv_answers", facts: map[string]any{"unreachable": []any{"dc_a", "dc_b"}}},
		{name: "not_found_where_kv_answers", facts: map[string]any{"operation": "operations/op-2", "unreachable": []any{"dc_b"}}},
		{name: "no_state", facts: map[string]any{"operation": "operations/op-2"}},
		{name: "found_true", facts: map[string]any{"operation": "operations/op-2", "found": true}},
		{name: "no_operation", facts: map[string]any{"state": "RUNNING"}},
		{name: "no_facts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := opNotInterrupted
			c.Facts = tt.facts

			got := decode(t, refuse([]check.Check{c}, check.Confirmations{}, dcctlv1.Operation_QUORUM))

			assert.Equal(t, string(apierr.ReasonPreflightFailed), got.info.GetReason())
		})
	}
}

// An error Refusal cannot answer with is not a status: the API logs it and answers
// with a fixed message.
func TestRefusal_unknownOutcome(t *testing.T) {
	err := answer.Refusal(check.Verdict{Outcome: "later"}, plan, dcctlv1.Operation_QUORUM)

	require.Error(t, err)
	_, ok := status.FromError(err)
	assert.False(t, ok, "%v is not a gRPC status", err)
}

func TestRefusal_factsStructCannotHold(t *testing.T) {
	bad := lagTooBig
	bad.Facts = map[string]any{"ch": make(chan int)}

	err := refuse([]check.Check{bad}, check.Confirmations{}, dcctlv1.Operation_QUORUM)

	require.Error(t, err)
	_, ok := status.FromError(err)
	assert.False(t, ok, "%v is not a gRPC status", err)
}

func TestRefusal_planUnchanged(t *testing.T) {
	steps := []*dcctlv1.Operation_Step{{Index: 7, Type: dcctlv1.Operation_Step_VERIFY, State: dcctlv1.Operation_Step_RUNNING}}

	err := answer.Refusal(check.Decide([]check.Check{lagTooBig}, check.Confirmations{}), steps, dcctlv1.Operation_QUORUM)
	got := decode(t, err)

	assert.Equal(t, dcctlv1.Operation_Step_RUNNING, steps[0].GetState())
	assert.EqualValues(t, 7, steps[0].GetIndex())
	assert.Equal(t, dcctlv1.Operation_Step_PENDING, got.preflight.GetSteps()[0].GetState())
	assert.EqualValues(t, 0, got.preflight.GetSteps()[0].GetIndex())
}
