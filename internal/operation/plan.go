package operation

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// StepFunc runs or rolls back a step. It reads what it acts on itself, since
// the step may run again after an interruption, when its effect may already
// have happened ([Exec.Resumed]), and passes [Exec.Guard] to the primitives
// it calls. An error that leaves the outcome unknown must match
// [ErrOutcomeUnknown].
type StepFunc func(ctx context.Context, x *Exec) error

// Step is one step of a plan.
type Step struct {
	Type dcctlv1.Operation_Step_Type
	// Stage is the Nomad stage of APPS_STOP and APPS_START.
	Stage int32
	// Datacenter is the datacenter the step acts on, by name.
	Datacenter string
	// PointOfNoReturn marks the step whose effect cannot be rolled back. A
	// failure of an earlier step rolls the operation back; a failure of this
	// step or a later one interrupts it.
	PointOfNoReturn bool
	// Recheck, when set, returns the checks of the confirmations the step acts
	// under, evaluated anew. They are decided with the confirmations of the
	// request right before the step; a step they do not allow fails without
	// running.
	Recheck func(ctx context.Context) ([]check.Check, error)
	// Run is required.
	Run StepFunc
	// Rollback undoes Run, also a Run that failed half way; nil when there is
	// nothing to undo.
	Rollback StepFunc
}

// Plan is an action as a scenario builds it for [Engine.Start].
type Plan struct {
	Type dcctlv1.Operation_Type
	// Target is the datacenter the action targets, by name; empty for
	// INITIALIZE_ARBITER.
	Target string
	// Request is the request message of Type, for example
	// *dcctlv1.SwitchDatacenterRequest.
	Request   proto.Message
	RequestID string
	Initiator *dcctlv1.Initiator
	// Preflight is the preflight of the action with the confirmations of the
	// request; the steps recheck against those confirmations too.
	Preflight preflight.Request
	// Mode is how the operation writes the new generation.
	Mode                              dcctlv1.Operation_Mode
	PreviousGeneration, NewGeneration int64
	// PreviousActive is the active datacenter the plan starts from, by name,
	// see [coord.Base]. When it or PreviousGeneration is set, the operation
	// starts only when the voters still hold that record, as the preflight saw
	// it.
	PreviousActive string
	Steps          []Step
}

func (p Plan) validate() error {
	switch {
	case p.Type == dcctlv1.Operation_TYPE_UNSPECIFIED:
		return errors.New("plan has no type")
	case p.Initiator == nil:
		return errors.New("plan has no initiator")
	case len(p.Steps) == 0:
		return errors.New("plan has no steps")
	}

	return validateSteps(p.Steps)
}

func validateSteps(steps []Step) error {
	for i, s := range steps {
		switch {
		case !preflight.KnownStepType(s.Type):
			return fmt.Errorf("step %d has unknown type %d", i, s.Type)
		case s.Run == nil:
			return fmt.Errorf("step %d (%s) has no run", i, s.Type)
		}
	}

	return nil
}

// pending returns the steps of p as the API shows them before they run.
func (p Plan) pending() []*dcctlv1.Operation_Step {
	out := make([]*dcctlv1.Operation_Step, 0, len(p.Steps))

	for i, s := range p.Steps {
		out = append(out, &dcctlv1.Operation_Step{
			Index:           int32(i), //nolint:gosec // a plan has a handful of steps
			Type:            s.Type,
			Stage:           s.Stage,
			State:           dcctlv1.Operation_Step_PENDING,
			PointOfNoReturn: s.PointOfNoReturn,
			Datacenter:      datacenterName(s.Datacenter),
		})
	}

	return out
}

// setRequest sets req as the request of op, which must be of the type of op.
func setRequest(op *dcctlv1.Operation, req proto.Message) error {
	var (
		want dcctlv1.Operation_Type
		set  func()
	)

	switch r := req.(type) {
	case nil:
		return nil
	case *dcctlv1.SwitchDatacenterRequest:
		want, set = dcctlv1.Operation_SWITCH, func() { op.Request = &dcctlv1.Operation_SwitchDatacenter{SwitchDatacenter: r} }
	case *dcctlv1.FailoverDatacenterRequest:
		want, set = dcctlv1.Operation_FAILOVER, func() { op.Request = &dcctlv1.Operation_FailoverDatacenter{FailoverDatacenter: r} }
	case *dcctlv1.DemoteDatacenterRequest:
		want, set = dcctlv1.Operation_DEMOTE, func() { op.Request = &dcctlv1.Operation_DemoteDatacenter{DemoteDatacenter: r} }
	case *dcctlv1.RepointDatacenterRequest:
		want, set = dcctlv1.Operation_REPOINT, func() { op.Request = &dcctlv1.Operation_RepointDatacenter{RepointDatacenter: r} }
	case *dcctlv1.MarkDatacenterRecoveredRequest:
		want, set = dcctlv1.Operation_MARK_RECOVERED, func() {
			op.Request = &dcctlv1.Operation_MarkDatacenterRecovered{MarkDatacenterRecovered: r}
		}
	case *dcctlv1.ResolveConflictRequest:
		want, set = dcctlv1.Operation_RESOLVE_CONFLICT, func() { op.Request = &dcctlv1.Operation_ResolveConflict{ResolveConflict: r} }
	case *dcctlv1.InitializeDatacentersRequest:
		want, set = dcctlv1.Operation_INITIALIZE, func() {
			op.Request = &dcctlv1.Operation_InitializeDatacenters{InitializeDatacenters: r}
		}
	case *dcctlv1.InitializeArbiterRequest:
		want, set = dcctlv1.Operation_INITIALIZE_ARBITER, func() { op.Request = &dcctlv1.Operation_InitializeArbiter{InitializeArbiter: r} }
	case *dcctlv1.SyncAppsRequest:
		want, set = dcctlv1.Operation_SYNC_APPS, func() { op.Request = &dcctlv1.Operation_SyncApps{SyncApps: r} }
	default:
		return fmt.Errorf("request %T is not an action request", req)
	}

	if op.GetType() != want {
		return fmt.Errorf("request %T is not of an operation of type %s", req, op.GetType())
	}

	set()

	return nil
}
