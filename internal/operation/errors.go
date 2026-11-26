package operation

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
)

var (
	// ErrNotRunning means the engine is not running: [Engine.Run] has not
	// started or has returned. Its gRPC status is Unavailable.
	ErrNotRunning error = &statusError{code: codes.Unavailable, msg: "operation engine is not running"}
	// ErrNoResumer means no scenario rebuilds the steps of the operation type.
	// Its gRPC status is Unimplemented.
	ErrNoResumer error = &statusError{code: codes.Unimplemented, msg: "operation type cannot be resumed"}
	// ErrOutcomeUnknown marks a step error after which it is not known what
	// the step changed, as a request that may have been applied: the
	// operation is interrupted instead of rolled back.
	ErrOutcomeUnknown = errors.New("outcome of the step is unknown")
	// ErrUnknownStepType means the operation has a step of a type this dcctl
	// does not know, written by a newer version; it cannot be resumed here. It
	// comes with the refusal of op.steps_known.
	ErrUnknownStepType = errors.New("operation has a step of an unknown type")
	// ErrRolledBack means the operation was interrupted during or after its
	// rollback; it cannot be resumed. It comes with the refusal of
	// op.not_rolled_back.
	ErrRolledBack = errors.New("operation was rolled back")
	// ErrStateChanged means the voters no longer hold the record the plan
	// starts from: the state changed after the preflight. It comes with the
	// refusal of coord.base_unchanged; the action is planned again.
	ErrStateChanged = errors.New("state changed since the preflight")
	// ErrStopping means the instance stops: its operation is interrupted.
	ErrStopping = errors.New("instance is stopping")
	// errOrphaned interrupts an operation no live session of dcctl/lock holds.
	errOrphaned = errors.New("no live session holds dcctl/lock for the operation: its leader stopped or lost the lock")
	// errSuperseded means a copy holds a newer change of the operation than
	// this instance made, after it lost the lock.
	errSuperseded = errors.New("a copy holds a newer change of the operation")
	// errRestarted interrupts the operation of this instance found running after
	// a restart.
	errRestarted = errors.New("the leading instance restarted while the operation was running")
)

// statusError is an error of the engine with the gRPC status the API answers
// with.
type statusError struct {
	code codes.Code
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// GRPCStatus returns the status the API answers with.
func (e *statusError) GRPCStatus() *status.Status { return status.New(e.code, e.msg) }

// NotRecordedError is returned by [Engine.Start] when the operation reached no
// copy of dcctl/op, so it did not start. Its gRPC status is Unavailable
// KV_UNAVAILABLE with metadata datacenters, the KVs that did not take it, and
// operation.
type NotRecordedError struct {
	Name string
	// Datacenters are the KVs that did not take the operation.
	Datacenters []string
	Err         error
}

func (e *NotRecordedError) Error() string {
	return fmt.Sprintf("%s is not started: no copy of dcctl/op took it: %v", e.Name, e.Err)
}

func (e *NotRecordedError) Unwrap() error { return e.Err }

// GRPCStatus returns the status the API answers with.
func (e *NotRecordedError) GRPCStatus() *status.Status {
	md := map[string]string{"operation": e.Name}
	if len(e.Datacenters) > 0 {
		md["datacenters"] = strings.Join(e.Datacenters, ",")
	}

	return apierr.Status(apierr.ReasonKVUnavailable, e.Error(), md)
}

// NotInterruptedError is returned by [Engine.Resume] and [Engine.Abandon] for
// an operation that is not interrupted. Its gRPC status is FailedPrecondition
// OPERATION_NOT_INTERRUPTED with metadata operation and state.
type NotInterruptedError struct {
	Name  string
	State dcctlv1.Operation_State
}

func (e *NotInterruptedError) Error() string {
	return fmt.Sprintf("%s is %s, not interrupted", e.Name, e.State)
}

// GRPCStatus returns the status the API answers with.
func (e *NotInterruptedError) GRPCStatus() *status.Status {
	return apierr.Status(apierr.ReasonOperationNotInterrupted, e.Error(),
		map[string]string{"operation": e.Name, "state": e.State.String()})
}

// NotFoundError is returned by [Engine.Resume] and [Engine.Abandon] when no
// copy of dcctl/op holds the operation. Its gRPC status is NotFound
// OPERATION_NOT_FOUND when every copy was read, and Unavailable KV_UNAVAILABLE
// otherwise, as the operation may be in a copy that did not answer.
type NotFoundError struct {
	Name string
	// Unreachable are the KVs whose copy was not read.
	Unreachable []string
}

func (e *NotFoundError) Error() string {
	if len(e.Unreachable) > 0 {
		return fmt.Sprintf("%s is not found in the copies of dcctl/op that answered", e.Name)
	}

	return e.Name + " is not found"
}

// GRPCStatus returns the status the API answers with.
func (e *NotFoundError) GRPCStatus() *status.Status {
	if len(e.Unreachable) > 0 {
		st, _ := status.FromError(apierr.KVUnavailable(e.Error(), e.Unreachable))

		return st
	}

	return apierr.Status(apierr.ReasonOperationNotFound, e.Error(), map[string]string{"operation": e.Name})
}

// interrupting reports whether err leaves what the step changed unknown, so
// the operation is interrupted instead of rolled back.
func interrupting(err error) bool {
	return errors.Is(err, ErrOutcomeUnknown) ||
		errors.Is(err, coord.ErrOutcomeUnknown) ||
		errors.Is(err, pgctl.ErrOutcomeUnknown) ||
		errors.Is(err, coord.ErrUndetermined) ||
		errors.Is(err, coord.ErrUncommitted) ||
		errors.Is(err, coord.ErrLockLost) ||
		errors.Is(err, errNotRecorded)
}

// operationError returns err as the error of an operation: its detail, and the
// reason, id and metadata of the API error it carries.
func operationError(err error) *dcctlv1.OperationError {
	oe := &dcctlv1.OperationError{Detail: truncate(err.Error())}

	var gs interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &gs) {
		return oe
	}

	for _, d := range gs.GRPCStatus().Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			oe.Reason, oe.Id, oe.Metadata = info.GetReason(), apierr.Reason(info.GetReason()).ID(), info.GetMetadata()

			break
		}
	}

	return oe
}
