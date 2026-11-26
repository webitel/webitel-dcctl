package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// Engine runs operations; [*operation.Engine] is one.
type Engine interface {
	Start(ctx context.Context, p operation.Plan) (*dcctlv1.Operation, error)
	Resume(ctx context.Context, req operation.ResumeRequest) (*dcctlv1.Operation, error)
	Abandon(ctx context.Context, req operation.AbandonRequest) (*dcctlv1.Operation, error)
}

// Preflight runs the checks of an action; [*preflight.Runner] is one.
type Preflight interface {
	Run(ctx context.Context, req preflight.Request) (check.Verdict, error)
}

// Planner plans the operation of the action with requests of type R. It plans
// on the state v was decided on whatever v allows, so that a refusal shows
// the plan too, and fails when that state gives no plan. A plan sets
// PreviousGeneration and PreviousActive, the state it starts from, so that
// the engine does not start it on a state changed since the preflight.
type Planner[R proto.Message] interface {
	Plan(req R, initiator *dcctlv1.Initiator, v check.Verdict) (operation.Plan, error)
}

// Planners plan the actions; an action without one answers Unimplemented.
type Planners struct {
	Switch            Planner[*dcctlv1.SwitchDatacenterRequest]
	Failover          Planner[*dcctlv1.FailoverDatacenterRequest]
	Demote            Planner[*dcctlv1.DemoteDatacenterRequest]
	Repoint           Planner[*dcctlv1.RepointDatacenterRequest]
	MarkRecovered     Planner[*dcctlv1.MarkDatacenterRecoveredRequest]
	ResolveConflict   Planner[*dcctlv1.ResolveConflictRequest]
	Initialize        Planner[*dcctlv1.InitializeDatacentersRequest]
	InitializeArbiter Planner[*dcctlv1.InitializeArbiterRequest]
	SyncApps          Planner[*dcctlv1.SyncAppsRequest]
}

// Actions is what the actions run on.
type Actions struct {
	// Self is this instance.
	Self operation.Instance
	// Datacenters are the configured datacenters, by name.
	Datacenters []string
	Engine      Engine
	Preflight   Preflight
	Planners    Planners
	// Catalog finds the instance that leads an action, and Forwarder calls it.
	Catalog   Catalog
	Forwarder Forwarder
}

func (a *Actions) validate() error {
	switch {
	case a.Self.Node == "" || a.Self.Datacenter == "":
		return errors.New("api: actions need the node and the datacenter of this instance")
	case a.Engine == nil || a.Preflight == nil:
		return errors.New("api: actions need the operation engine and the preflight")
	case a.Catalog == nil || a.Forwarder == nil:
		return errors.New("api: actions need the catalog and the forwarder")
	}

	return nil
}

// actionRequest is a request of an action that starts an operation.
type actionRequest interface {
	proto.Message
	GetConfirmation() *dcctlv1.Confirmation
	GetValidateOnly() bool
	GetRequestId() string
}

// actionServer runs the actions of DatacenterService and OperationService. A
// nil one answers every action Unimplemented.
type actionServer struct {
	cfg    Actions
	self   *dcctlv1.Instance
	status StatusSource
	ops    Operations
	log    *logging.Logger
	now    func() time.Time
}

func newActionServer(cfg *Actions, src StatusSource, ops Operations, log *logging.Logger) (*actionServer, error) {
	if cfg == nil {
		return nil, nil //nolint:nilnil // no actions are served
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	self := &dcctlv1.Instance{Node: cfg.Self.Node, Datacenter: datacenterName(cfg.Self.Datacenter), Address: cfg.Self.Address}

	return &actionServer{cfg: *cfg, self: self, status: src, ops: ops, log: log, now: time.Now}, nil
}

// action is one call of an action that starts an operation.
type action[R actionRequest] struct {
	typ     dcctlv1.Operation_Type
	method  string
	req     R
	planner Planner[R]
	// datacenter is the datacenter the request names, by name; empty for
	// INITIALIZE_ARBITER and SYNC_APPS.
	datacenter string
}

// preflight returns the preflight of the action, the same for the request
// and its validate_only.
func (a action[R]) preflight() preflight.Request {
	return preflight.Request{
		Action:        preflightAction[a.typ],
		Params:        preflight.Params{Datacenter: a.datacenter},
		Confirmations: confirmations(a.req.GetConfirmation()),
	}
}

var preflightAction = map[dcctlv1.Operation_Type]preflight.Action{
	dcctlv1.Operation_SWITCH:             preflight.Switch,
	dcctlv1.Operation_FAILOVER:           preflight.Failover,
	dcctlv1.Operation_DEMOTE:             preflight.Demote,
	dcctlv1.Operation_REPOINT:            preflight.Repoint,
	dcctlv1.Operation_MARK_RECOVERED:     preflight.MarkRecovered,
	dcctlv1.Operation_RESOLVE_CONFLICT:   preflight.Resolve,
	dcctlv1.Operation_INITIALIZE:         preflight.Init,
	dcctlv1.Operation_INITIALIZE_ARBITER: preflight.ArbiterInit,
	dcctlv1.Operation_SYNC_APPS:          preflight.AppsSync,
}

func confirmations(c *dcctlv1.Confirmation) check.Confirmations {
	return check.Confirmations{
		Isolation: c.GetIsolation(), NotMaxLSN: c.GetNotMaxLsn(), MaxDiscardBytes: c.GetMaxDiscardBytes(),
	}
}

// runAction answers a call of a.
//
// A call another instance leads, see [actionServer.leaderOf], is forwarded to
// it as it is, with its initiator. A forwarded call runs where it arrives and
// is never forwarded again: its preflight refuses it when this instance does
// not lead it. A request_id the copies of dcctl/op already hold answers the
// operation it started, also when the leading instance is unavailable.
func runAction[R actionRequest](ctx context.Context, s *actionServer, a action[R]) (*dcctlv1.Operation, error) {
	if s == nil || a.planner == nil {
		return nil, status.Errorf(codes.Unimplemented, "%s is not implemented", strings.ToLower(a.typ.String()))
	}

	ini, forwarded, err := auth.Initiator(ctx, s.self)
	if err != nil {
		return nil, err
	}

	if a.datacenter != "" && !slices.Contains(s.cfg.Datacenters, a.datacenter) {
		return nil, datacenterNotFound(a.datacenter)
	}

	id := a.req.GetRequestId()
	idempotent := id != "" && !a.req.GetValidateOnly()

	if idempotent {
		if op, err := s.replay(ctx, id, a.req); op != nil || err != nil {
			return op, err
		}
	}

	if !forwarded {
		if op, done, err := s.forward(ctx, s.leaderOf(a.typ, a.datacenter), a.method, a.req, ini); done {
			return op, err
		}
	}

	op, err := runLocal(ctx, s, a, ini)
	if err != nil && idempotent {
		// The same request may have started the operation meanwhile.
		if op, rerr := s.replay(ctx, id, a.req); op != nil || rerr != nil {
			return op, rerr
		}
	}

	return op, err
}

// runLocal runs the preflight of a, plans it and starts its operation, or
// answers its validate_only.
func runLocal[R actionRequest](ctx context.Context, s *actionServer, a action[R], ini *dcctlv1.Initiator) (*dcctlv1.Operation, error) {
	pre := a.preflight()

	v, err := s.cfg.Preflight.Run(ctx, pre)
	if err != nil {
		return nil, rpcError(ctx, s.log, "preflight cannot run", err)
	}

	plan, planErr := a.planner.Plan(a.req, ini, v)
	if planErr == nil {
		planErr = completePlan(&plan, a.typ, a.req, pre, ini)
	}

	var (
		steps []*dcctlv1.Operation_Step
		mode  dcctlv1.Operation_Mode
	)

	if planErr == nil {
		steps, mode = plan.Pending(), plan.Mode
	}

	if refused := answer.Refusal(v, steps, mode); refused != nil {
		return nil, rpcError(ctx, s.log, "preflight answer cannot be built", refused)
	}

	if planErr != nil {
		return nil, rpcError(ctx, s.log, "action cannot be planned", planErr)
	}

	if a.req.GetValidateOnly() {
		return s.dryRun(ctx, v, plan)
	}

	op, err := s.cfg.Engine.Start(ctx, plan)
	if err != nil {
		return nil, rpcError(ctx, s.log, "operation cannot be started", err)
	}

	return op, nil
}

// completePlan sets what the API owns in plan: the action type, the request
// as it is stored, its request_id and initiator, and the preflight when the
// planner left it out.
func completePlan(plan *operation.Plan, typ dcctlv1.Operation_Type, req actionRequest, pre preflight.Request, ini *dcctlv1.Initiator) error {
	if plan.Type != dcctlv1.Operation_TYPE_UNSPECIFIED && plan.Type != typ {
		return fmt.Errorf("planner of %s returned a plan of %s", typ, plan.Type)
	}

	plan.Type = typ
	plan.Request = storedRequest(req)
	plan.RequestID = req.GetRequestId()
	plan.Initiator = ini

	if plan.Preflight.Action == "" {
		plan.Preflight = pre
	}

	return nil
}

// dryRun answers validate_only with the operation plan would start, without a
// name.
func (s *actionServer) dryRun(ctx context.Context, v check.Verdict, plan operation.Plan) (*dcctlv1.Operation, error) {
	op, err := answer.DryRun(v, plan.Pending(), plan.Mode)
	if err != nil {
		return nil, rpcError(ctx, s.log, "preflight answer cannot be built", err)
	}

	op.Type = plan.Type
	op.Target = datacenterName(plan.Target)
	op.Initiator = plan.Initiator
	op.PreviousGeneration = plan.PreviousGeneration
	op.NewGeneration = plan.NewGeneration
	setRequest(op, plan.Request)

	return op, nil
}

// reasonOfError returns the ErrorInfo reason of the status in the chain of
// err, "" without one.
func reasonOfError(err error) apierr.Reason {
	var gs interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &gs) {
		return ""
	}

	for _, d := range gs.GRPCStatus().Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == apierr.Domain {
			return apierr.Reason(info.GetReason())
		}
	}

	return ""
}
