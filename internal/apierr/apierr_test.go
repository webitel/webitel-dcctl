package apierr_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"

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
