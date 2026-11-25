package apierr_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func TestReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason apierr.Reason
		value  string
		id     string
		code   codes.Code
	}{
		{apierr.ReasonRequestInvalid, "REQUEST_INVALID", "dcctl.request.invalid", codes.InvalidArgument},
		{apierr.ReasonAuthenticationRequired, "AUTHENTICATION_REQUIRED", "dcctl.auth.required", codes.Unauthenticated},
		{apierr.ReasonDatacenterNotFound, "DATACENTER_NOT_FOUND", "dcctl.datacenter.not_found", codes.NotFound},
		{apierr.ReasonOperationNotFound, "OPERATION_NOT_FOUND", "dcctl.operation.not_found", codes.NotFound},
		{apierr.ReasonPreflightFailed, "PREFLIGHT_FAILED", "dcctl.preflight.failed", codes.FailedPrecondition},
		{apierr.ReasonConfirmationRequired, "CONFIRMATION_REQUIRED", "dcctl.confirmation.required", codes.FailedPrecondition},
		{apierr.ReasonOperationRunning, "OPERATION_RUNNING", "dcctl.operation.running", codes.FailedPrecondition},
		{apierr.ReasonOperationNotInterrupted, "OPERATION_NOT_INTERRUPTED", "dcctl.operation.not_interrupted", codes.FailedPrecondition},
		{apierr.ReasonLockBusy, "LOCK_BUSY", "dcctl.lock.busy", codes.Aborted},
		{apierr.ReasonRequestIDMismatch, "REQUEST_ID_MISMATCH", "dcctl.request_id.mismatch", codes.AlreadyExists},
		{apierr.ReasonLeaderUnavailable, "LEADER_UNAVAILABLE", "dcctl.leader.unavailable", codes.Unavailable},
		{apierr.ReasonEtagMismatch, "ETAG_MISMATCH", "", codes.Aborted},
		{apierr.ReasonArbiterUninitialized, "ARBITER_UNINITIALIZED", "", codes.FailedPrecondition},
		{apierr.ReasonRecordNotFound, "RECORD_NOT_FOUND", "", codes.NotFound},
	}

	assert.Len(t, apierr.Reasons(), len(tests), "every reason of the contract is listed")

	for i, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.value, string(tt.reason))
			assert.Equal(t, tt.id, tt.reason.ID())
			assert.Equal(t, tt.code, tt.reason.Code())
			assert.Equal(t, tt.reason, apierr.Reasons()[i])
		})
	}
}

func TestReasonUnknown(t *testing.T) {
	t.Parallel()

	r := apierr.Reason("SOMETHING_ELSE")

	assert.Empty(t, r.ID())
	assert.Equal(t, codes.Unknown, r.Code())
}

func TestReasonsUnique(t *testing.T) {
	t.Parallel()

	reasons := map[apierr.Reason]bool{}
	ids := map[string]bool{}

	for _, r := range apierr.Reasons() {
		assert.False(t, reasons[r], "duplicate reason %s", r)
		reasons[r] = true

		if r.ID() == "" {
			continue
		}

		assert.False(t, ids[r.ID()], "duplicate id %s", r.ID())
		ids[r.ID()] = true
	}
}

func TestDomain(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "dcctl.webitel.com", apierr.Domain)
}

func TestNew(t *testing.T) {
	t.Parallel()

	bad := &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{
		{Field: "name", Description: "must match"},
	}}

	err := apierr.New(apierr.ReasonRequestInvalid, "invalid request", bad)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
	assert.Equal(t, "invalid request", st.Message())

	details := st.Details()
	require.Len(t, details, 2)

	info, ok := details[0].(*errdetails.ErrorInfo)
	require.True(t, ok, "first detail is %T", details[0])
	assert.Equal(t, apierr.Domain, info.GetDomain())
	assert.Equal(t, "REQUEST_INVALID", info.GetReason())

	got, ok := details[1].(*errdetails.BadRequest)
	require.True(t, ok, "second detail is %T", details[1])
	assert.Equal(t, "name", got.GetFieldViolations()[0].GetField())
}

func TestStatusMetadata(t *testing.T) {
	t.Parallel()

	st := apierr.Status(apierr.ReasonLockBusy, "lock is busy", map[string]string{"holder": "node-b"})

	assert.Equal(t, codes.Aborted, st.Code())
	assert.Equal(t, "lock is busy", st.Message())

	details := st.Details()
	require.Len(t, details, 1)

	info, ok := details[0].(*errdetails.ErrorInfo)
	require.True(t, ok, "detail is %T", details[0])
	assert.Equal(t, "LOCK_BUSY", info.GetReason())
	assert.Equal(t, map[string]string{"holder": "node-b"}, info.GetMetadata())
}
