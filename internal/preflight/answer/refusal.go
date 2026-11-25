package answer

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// PreconditionFailure violation types.
const (
	violationCheck        = "check"
	violationConfirmation = "confirmation"
)

// Facts of the coordination checks that pick the reason of a refusal.
const (
	// factReachable is false when no Consul KV answered coord.no_operation or
	// coord.lock_free.
	factReachable = "reachable"
	// factUnreachable lists the datacenters whose KV did not answer.
	factUnreachable = "unreachable"
	// factOperation is the resource name of the operation coord.no_operation found.
	factOperation = "op_id"
	// The holder of the lock coord.lock_free found: the KV it is held in, the instance,
	// its datacenter and the ID of the operation.
	factLockKV         = "kv"
	factLockHolder     = "holder"
	factLockDatacenter = "datacenter"
	factLockOperation  = "operation"
	// The operation op.interrupted judged, its state as the enum name, and found=false
	// when every KV answered without it.
	factOpName  = "operation"
	factOpState = "state"
	factOpFound = "found"
	// factLeaderInstance is the instance coord.leader_reachable judged.
	factLeaderInstance = "instance"
	// factLeaderDatacenters is set when this instance does not lead the action: the
	// datacenters whose instance does.
	factLeaderDatacenters = "leader_datacenters"
)

// Refusal returns the error the action is refused with, the same for the request and
// its validate_only, and nil when v allows it. plan is the steps the operation would
// run, in order; the Preflight detail shows them PENDING and leaves plan unchanged. mode
// is how the operation would write the generation; the mode fact of coord.quorum is the
// mode of the voters, not this one.
//
// The PreconditionFailure lists the failed checks, then the missing confirmations. An
// outcome Refusal does not know and a check whose facts google.protobuf.Struct cannot
// hold give a plain error, not a status: its text may carry facts the client must not
// see.
func Refusal(v check.Verdict, plan []*dcctlv1.Operation_Step, mode dcctlv1.Operation_Mode) error {
	switch v.Outcome {
	case check.Allowed:
		return nil
	case check.Blocked, check.NeedsConfirmation:
	default:
		return fmt.Errorf("preflight answer: outcome %q is not known", v.Outcome)
	}

	checks, err := Checks(v.Checks)
	if err != nil {
		return fmt.Errorf("preflight answer: %w", err)
	}

	violations := append(checkViolations(v.Failed), confirmationViolations(v.Unconfirmed)...)

	st := reasonOf(v)

	withDetails, err := st.WithDetails(
		&errdetails.PreconditionFailure{Violations: violations},
		protoadapt.MessageV1Of(&dcctlv1.Preflight{Checks: checks, Steps: pending(plan), Mode: mode}),
	)
	if err != nil {
		// Only a detail that cannot be marshaled gets here; the reason still reaches
		// the client.
		return st.Err()
	}

	return withDetails.Err()
}

// reasonOf returns the status of the refusal before the preflight details.
func reasonOf(v check.Verdict) *status.Status {
	if v.Outcome == check.NeedsConfirmation {
		return apierr.Status(apierr.ReasonConfirmationRequired,
			"confirmation required: "+strings.Join(confirmationNames(v.Unconfirmed), ", "), nil)
	}

	if st := special(v.Failed); st != nil {
		return st
	}

	ids := make([]string, 0, len(v.Failed))
	for _, c := range v.Failed {
		if !slices.Contains(ids, string(c.ID)) {
			ids = append(ids, string(c.ID))
		}
	}

	return apierr.Status(apierr.ReasonPreflightFailed, "preflight failed: "+strings.Join(ids, ", "), nil)
}

// special returns the status of the coordination check that gives the refusal its own
// reason, in the order of the package doc, or nil when none failed.
func special(failed []check.Check) *status.Status {
	op, hasOp := failedCheck(failed, check.OpInterrupted)
	noOp, hasNoOp := failedCheck(failed, check.CoordNoOperation)
	lock, hasLock := failedCheck(failed, check.CoordLockFree)
	leader, hasLeader := failedCheck(failed, check.CoordLeaderReachable)

	switch {
	case hasOp && notFound(op):
		return apierr.Status(apierr.ReasonOperationNotFound, op.Detail,
			map[string]string{"operation": fact(op, factOpName)})
	case hasOp && fact(op, factOpName) != "" && fact(op, factOpState) != "":
		return apierr.Status(apierr.ReasonOperationNotInterrupted, op.Detail,
			map[string]string{"operation": fact(op, factOpName), "state": fact(op, factOpState)})
	case hasNoOp && fact(noOp, factOperation) != "":
		return operationRunning(noOp)
	case hasLock && !unreachable(lock):
		return lockBusy(lock)
	case hasLock && unreachable(lock):
		return lockUnavailable(lock)
	case hasNoOp && unreachable(noOp):
		return lockUnavailable(noOp)
	case hasLeader:
		return leaderUnavailable(leader)
	}

	return nil
}

// failedCheck returns the first check id among cs that failed; a result that is not
// known gives no special reason.
func failedCheck(cs []check.Check, id check.ID) (check.Check, bool) {
	i := slices.IndexFunc(cs, func(c check.Check) bool { return c.ID == id && c.Result == check.Fail })
	if i < 0 {
		return check.Check{}, false
	}

	return cs[i], true
}

// notFound reports whether op.interrupted failed because no KV holds the operation.
func notFound(c check.Check) bool {
	found, ok := c.Facts[factOpFound].(bool)

	return ok && !found && fact(c, factOpName) != ""
}

// unreachable reports whether c failed because no Consul KV answered.
func unreachable(c check.Check) bool {
	r, ok := c.Facts[factReachable].(bool)

	return ok && !r
}

func operationRunning(c check.Check) *status.Status {
	return apierr.Status(apierr.ReasonOperationRunning, c.Detail,
		map[string]string{"operation": fact(c, factOperation)})
}

func lockBusy(c check.Check) *status.Status {
	err := &coord.LockBusyError{
		Name: fact(c, factLockKV),
		Holder: kv.Lock{
			Instance:   fact(c, factLockHolder),
			Datacenter: fact(c, factLockDatacenter),
			Operation:  fact(c, factLockOperation),
		},
	}

	return err.GRPCStatus()
}

func lockUnavailable(c check.Check) *status.Status {
	err := &coord.LockUnavailableError{}
	for _, name := range strs(c, factUnreachable) {
		err.Unreachable = append(err.Unreachable, coord.Unreachable{Name: name})
	}

	return err.GRPCStatus()
}

func leaderUnavailable(c check.Check) *status.Status {
	md := make(map[string]string, 1)

	if dcs := strs(c, factLeaderDatacenters); len(dcs) > 0 {
		md["datacenters"] = strings.Join(dcs, ",")
	} else if instance := fact(c, factLeaderInstance); instance != "" {
		md["instance"] = instance
	}

	return apierr.Status(apierr.ReasonLeaderUnavailable, c.Detail, md)
}

// fact returns the string fact key of c, or "".
func fact(c check.Check, key string) string {
	if s, ok := c.Facts[key].(string); ok {
		return s
	}

	return ""
}

// strs returns the strings of the list fact key of c.
func strs(c check.Check, key string) []string {
	list, ok := c.Facts[key].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(list))

	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}

	return out
}

func checkViolations(failed []check.Check) []*errdetails.PreconditionFailure_Violation {
	out := make([]*errdetails.PreconditionFailure_Violation, 0, len(failed))

	for _, c := range failed {
		desc := c.Detail
		if c.Result == check.Fail && c.Effect == check.Confirm &&
			c.Confirmation == check.ConfirmMaxDiscardBytes && c.DiscardBytes == nil {
			desc += " (the amount to discard is not known, no max_discard_bytes covers it)"
		}

		out = append(out, &errdetails.PreconditionFailure_Violation{
			Type: violationCheck, Subject: string(c.ID), Description: desc,
		})
	}

	return out
}

// confirmationViolations returns one violation for each confirmation the checks need,
// in the order they first need it.
func confirmationViolations(unconfirmed []check.Check) []*errdetails.PreconditionFailure_Violation {
	names := confirmationNames(unconfirmed)
	out := make([]*errdetails.PreconditionFailure_Violation, 0, len(names))

	for _, name := range names {
		var (
			details []string
			bound   *int64
		)

		for _, c := range unconfirmed {
			if string(c.Confirmation) != name {
				continue
			}

			details = append(details, c.Detail)

			if c.DiscardBytes != nil && (bound == nil || *c.DiscardBytes > *bound) {
				bound = c.DiscardBytes
			}
		}

		if bound != nil {
			details = append(details, fmt.Sprintf("%s must be at least %d", check.ConfirmMaxDiscardBytes, *bound))
		}

		out = append(out, &errdetails.PreconditionFailure_Violation{
			Type: violationConfirmation, Subject: name, Description: strings.Join(details, "; "),
		})
	}

	return out
}

func confirmationNames(unconfirmed []check.Check) []string {
	var out []string

	for _, c := range unconfirmed {
		if !slices.Contains(out, string(c.Confirmation)) {
			out = append(out, string(c.Confirmation))
		}
	}

	return out
}

// pending returns copies of plan with their position and PENDING.
func pending(plan []*dcctlv1.Operation_Step) []*dcctlv1.Operation_Step {
	out := make([]*dcctlv1.Operation_Step, len(plan))

	for i, s := range plan {
		step := proto.CloneOf(s)
		step.Index = int32(i)
		step.State = dcctlv1.Operation_Step_PENDING
		out[i] = step
	}

	return out
}
