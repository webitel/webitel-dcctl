// Package apierr holds the error contract of the dcctl API: the ErrorInfo
// domain, the reasons gRPC clients match on, the ids HTTP clients match on
// and the gRPC code of each reason.
package apierr

import "google.golang.org/grpc/codes"

// Domain is the google.rpc.ErrorInfo domain of every dcctl error.
const Domain = "dcctl.webitel.com"

// Reason is the google.rpc.ErrorInfo reason of a dcctl error.
type Reason string

// Reasons of dcctl errors. The arbiter reasons have no HTTP id because the
// arbiter API is gRPC only.
const (
	ReasonRequestInvalid          Reason = "REQUEST_INVALID"
	ReasonAuthenticationRequired  Reason = "AUTHENTICATION_REQUIRED"
	ReasonDatacenterNotFound      Reason = "DATACENTER_NOT_FOUND"
	ReasonOperationNotFound       Reason = "OPERATION_NOT_FOUND"
	ReasonPreflightFailed         Reason = "PREFLIGHT_FAILED"
	ReasonConfirmationRequired    Reason = "CONFIRMATION_REQUIRED"
	ReasonOperationRunning        Reason = "OPERATION_RUNNING"
	ReasonOperationNotInterrupted Reason = "OPERATION_NOT_INTERRUPTED"
	ReasonLockBusy                Reason = "LOCK_BUSY"
	ReasonRequestIDMismatch       Reason = "REQUEST_ID_MISMATCH"
	ReasonLeaderUnavailable       Reason = "LEADER_UNAVAILABLE"
	ReasonEtagMismatch            Reason = "ETAG_MISMATCH"
	ReasonArbiterUninitialized    Reason = "ARBITER_UNINITIALIZED"
	ReasonRecordNotFound          Reason = "RECORD_NOT_FOUND"
)

type entry struct {
	reason Reason
	id     string
	code   codes.Code
}

// contract is the error table in its documented order.
var contract = []entry{
	{ReasonRequestInvalid, "dcctl.request.invalid", codes.InvalidArgument},
	{ReasonAuthenticationRequired, "dcctl.auth.required", codes.Unauthenticated},
	{ReasonDatacenterNotFound, "dcctl.datacenter.not_found", codes.NotFound},
	{ReasonOperationNotFound, "dcctl.operation.not_found", codes.NotFound},
	{ReasonPreflightFailed, "dcctl.preflight.failed", codes.FailedPrecondition},
	{ReasonConfirmationRequired, "dcctl.confirmation.required", codes.FailedPrecondition},
	{ReasonOperationRunning, "dcctl.operation.running", codes.FailedPrecondition},
	{ReasonOperationNotInterrupted, "dcctl.operation.not_interrupted", codes.FailedPrecondition},
	{ReasonLockBusy, "dcctl.lock.busy", codes.Aborted},
	{ReasonRequestIDMismatch, "dcctl.request_id.mismatch", codes.AlreadyExists},
	{ReasonLeaderUnavailable, "dcctl.leader.unavailable", codes.Unavailable},
	{ReasonEtagMismatch, "", codes.Aborted},
	{ReasonArbiterUninitialized, "", codes.FailedPrecondition},
	{ReasonRecordNotFound, "", codes.NotFound},
}

// Reasons returns every reason in the order of the contract table.
func Reasons() []Reason {
	out := make([]Reason, len(contract))
	for i, e := range contract {
		out[i] = e.reason
	}

	return out
}

func lookup(r Reason) (entry, bool) {
	for _, e := range contract {
		if e.reason == r {
			return e, true
		}
	}

	return entry{}, false
}

// ID returns the id HTTP clients get in the error body, or "" for a reason
// without one.
func (r Reason) ID() string {
	e, _ := lookup(r)

	return e.id
}

// Code returns the gRPC code of the reason, or codes.Unknown for a reason
// outside the contract.
func (r Reason) Code() codes.Code {
	e, ok := lookup(r)
	if !ok {
		return codes.Unknown
	}

	return e.code
}
