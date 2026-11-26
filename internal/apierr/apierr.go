// Package apierr holds the error contract of the dcctl API: the ErrorInfo
// domain, the reasons gRPC clients match on, the ids HTTP clients match on
// and the gRPC code of each reason.
package apierr

import (
	"errors"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

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
	ReasonLockUnavailable         Reason = "LOCK_UNAVAILABLE"
	ReasonStatusNotReady          Reason = "STATUS_NOT_READY"
	ReasonKVUnavailable           Reason = "KV_UNAVAILABLE"
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
	{ReasonLockUnavailable, "dcctl.lock.unavailable", codes.Unavailable},
	{ReasonStatusNotReady, "dcctl.status.not_ready", codes.Unavailable},
	{ReasonKVUnavailable, "dcctl.kv.unavailable", codes.Unavailable},
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

// New returns a gRPC status error with the code of r, msg and the details,
// after an ErrorInfo with r.
func New(r Reason, msg string, details ...protoadapt.MessageV1) error {
	return Status(r, msg, nil, details...).Err()
}

// KVUnavailable returns the KV_UNAVAILABLE error with msg. datacenters are
// the datacenters whose Consul KV could not be read; the metadata lists them
// in `datacenters`, comma separated, and leaves the key out when there are
// none.
func KVUnavailable(msg string, datacenters []string) error {
	var md map[string]string
	if len(datacenters) > 0 {
		md = map[string]string{"datacenters": strings.Join(datacenters, ",")}
	}

	return Status(ReasonKVUnavailable, msg, md).Err()
}

// InfoOf returns the dcctl ErrorInfo of the gRPC status in the chain of err,
// nil when there is none.
func InfoOf(err error) *errdetails.ErrorInfo {
	var gs interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &gs) {
		return nil
	}

	for _, d := range gs.GRPCStatus().Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == Domain {
			return info
		}
	}

	return nil
}

// ReasonOf returns the reason of [InfoOf], "" without one.
func ReasonOf(err error) Reason {
	return Reason(InfoOf(err).GetReason())
}

// Status returns the gRPC status of [New] with metadata in its ErrorInfo,
// for an error type that implements GRPCStatus.
func Status(r Reason, msg string, metadata map[string]string, details ...protoadapt.MessageV1) *status.Status {
	st := status.New(r.Code(), msg)

	withDetails, err := st.WithDetails(append(
		[]protoadapt.MessageV1{&errdetails.ErrorInfo{Domain: Domain, Reason: string(r), Metadata: metadata}},
		details...,
	)...)
	if err != nil {
		// Only a detail that cannot be marshaled gets here; the code and
		// message still reach the client.
		return st
	}

	return withDetails
}
