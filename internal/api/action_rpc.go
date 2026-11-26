package api

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// SwitchDatacenter switches to the datacenter of the request.
func (s *DatacenterServer) SwitchDatacenter(ctx context.Context, req *dcctlv1.SwitchDatacenterRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.SwitchDatacenterRequest]{
		typ: dcctlv1.Operation_SWITCH, method: dcctlv1.DatacenterService_SwitchDatacenter_FullMethodName,
		req: req, planner: s.planners().Switch, datacenter: datacenterID(req.GetName()),
	})
}

// FailoverDatacenter fails over to the datacenter of the request.
func (s *DatacenterServer) FailoverDatacenter(ctx context.Context, req *dcctlv1.FailoverDatacenterRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.FailoverDatacenterRequest]{
		typ: dcctlv1.Operation_FAILOVER, method: dcctlv1.DatacenterService_FailoverDatacenter_FullMethodName,
		req: req, planner: s.planners().Failover, datacenter: datacenterID(req.GetName()),
	})
}

// DemoteDatacenter demotes the old primary of the datacenter of the request.
func (s *DatacenterServer) DemoteDatacenter(ctx context.Context, req *dcctlv1.DemoteDatacenterRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.DemoteDatacenterRequest]{
		typ: dcctlv1.Operation_DEMOTE, method: dcctlv1.DatacenterService_DemoteDatacenter_FullMethodName,
		req: req, planner: s.planners().Demote, datacenter: datacenterID(req.GetName()),
	})
}

// RepointDatacenter points the datacenter of the request at the active one.
func (s *DatacenterServer) RepointDatacenter(ctx context.Context, req *dcctlv1.RepointDatacenterRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.RepointDatacenterRequest]{
		typ: dcctlv1.Operation_REPOINT, method: dcctlv1.DatacenterService_RepointDatacenter_FullMethodName,
		req: req, planner: s.planners().Repoint, datacenter: datacenterID(req.GetName()),
	})
}

// MarkDatacenterRecovered clears needs_recovery of the datacenter of the
// request.
func (s *DatacenterServer) MarkDatacenterRecovered(ctx context.Context, req *dcctlv1.MarkDatacenterRecoveredRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.MarkDatacenterRecoveredRequest]{
		typ: dcctlv1.Operation_MARK_RECOVERED, method: dcctlv1.DatacenterService_MarkDatacenterRecovered_FullMethodName,
		req: req, planner: s.planners().MarkRecovered, datacenter: datacenterID(req.GetName()),
	})
}

// ResolveConflict resolves a generation conflict in favor of the winner.
func (s *DatacenterServer) ResolveConflict(ctx context.Context, req *dcctlv1.ResolveConflictRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.ResolveConflictRequest]{
		typ: dcctlv1.Operation_RESOLVE_CONFLICT, method: dcctlv1.DatacenterService_ResolveConflict_FullMethodName,
		req: req, planner: s.planners().ResolveConflict, datacenter: datacenterID(req.GetWinner()),
	})
}

// InitializeDatacenters creates the initial state with the active datacenter
// of the request.
func (s *DatacenterServer) InitializeDatacenters(ctx context.Context, req *dcctlv1.InitializeDatacentersRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.InitializeDatacentersRequest]{
		typ: dcctlv1.Operation_INITIALIZE, method: dcctlv1.DatacenterService_InitializeDatacenters_FullMethodName,
		req: req, planner: s.planners().Initialize, datacenter: datacenterID(req.GetActiveDatacenter()),
	})
}

// SyncApps brings the apps of the active datacenter to the desired state.
func (s *DatacenterServer) SyncApps(ctx context.Context, req *dcctlv1.SyncAppsRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.SyncAppsRequest]{
		typ: dcctlv1.Operation_SYNC_APPS, method: dcctlv1.DatacenterService_SyncApps_FullMethodName,
		req: req, planner: s.planners().SyncApps,
	})
}

// InitializeArbiter initializes an empty arbiter.
func (s *DatacenterServer) InitializeArbiter(ctx context.Context, req *dcctlv1.InitializeArbiterRequest) (*dcctlv1.Operation, error) {
	return runAction(ctx, s.actions, action[*dcctlv1.InitializeArbiterRequest]{
		typ: dcctlv1.Operation_INITIALIZE_ARBITER, method: dcctlv1.DatacenterService_InitializeArbiter_FullMethodName,
		req: req, planner: s.planners().InitializeArbiter,
	})
}

func (s *DatacenterServer) planners() Planners {
	if s.actions == nil {
		return Planners{}
	}

	return s.actions.cfg.Planners
}

// ResumeOperation resumes an interrupted operation. The instance that led it
// resumes it, or one of its target when that one does not pass in Consul. The
// isolation confirmation resumes it writing the generation forced.
func (s *OperationServer) ResumeOperation(ctx context.Context, req *dcctlv1.ResumeOperationRequest) (*dcctlv1.Operation, error) {
	a := s.actions
	if a == nil {
		return nil, status.Error(codes.Unimplemented, "resume is not implemented")
	}

	ini, forwarded, err := auth.Initiator(ctx, a.self)
	if err != nil {
		return nil, err
	}

	if !forwarded {
		l := a.resumeLeader(ctx, req.GetName())
		if op, done, err := a.forward(ctx, l, dcctlv1.OperationService_ResumeOperation_FullMethodName, req, ini); done {
			return op, err
		}
	}

	id := operationID(req.GetName())
	conf := confirmations(req.GetConfirmation())
	toForced := conf.Isolation

	if req.GetValidateOnly() {
		mode := dcctlv1.Operation_MODE_UNSPECIFIED
		if toForced {
			mode = dcctlv1.Operation_FORCED
		}

		return a.dryRunOf(ctx, preflight.Request{
			Action: preflight.Resume, Params: preflight.Params{Operation: id, ToForced: toForced}, Confirmations: conf,
		}, mode)
	}

	op, err := a.cfg.Engine.Resume(ctx, operation.ResumeRequest{
		Operation: id, Confirmations: conf, ToForced: toForced, Initiator: ini,
	})
	if err != nil {
		return nil, rpcError(ctx, a.log, "operation cannot be resumed", err)
	}

	return op, nil
}

// AbandonOperation abandons an interrupted operation; any instance does.
func (s *OperationServer) AbandonOperation(ctx context.Context, req *dcctlv1.AbandonOperationRequest) (*dcctlv1.Operation, error) {
	a := s.actions
	if a == nil {
		return nil, status.Error(codes.Unimplemented, "abandon is not implemented")
	}

	ini, _, err := auth.Initiator(ctx, a.self)
	if err != nil {
		return nil, err
	}

	id := operationID(req.GetName())
	conf := confirmations(req.GetConfirmation())

	if req.GetValidateOnly() {
		return a.dryRunOf(ctx, preflight.Request{
			Action: preflight.Abandon, Params: preflight.Params{Operation: id}, Confirmations: conf,
		}, dcctlv1.Operation_MODE_UNSPECIFIED)
	}

	op, err := a.cfg.Engine.Abandon(ctx, operation.AbandonRequest{Operation: id, Confirmations: conf, Initiator: ini})
	if err != nil {
		return nil, rpcError(ctx, a.log, "operation cannot be abandoned", err)
	}

	return op, nil
}

// dryRunOf answers validate_only of an action that has no plan of its own:
// the checks of pre and mode, or the refusal the engine would give.
func (s *actionServer) dryRunOf(ctx context.Context, pre preflight.Request, mode dcctlv1.Operation_Mode) (*dcctlv1.Operation, error) {
	v, err := s.cfg.Preflight.Run(ctx, pre)
	if err != nil {
		return nil, rpcError(ctx, s.log, "preflight cannot run", err)
	}

	op, err := answer.DryRun(v, nil, mode)
	if err != nil {
		return nil, rpcError(ctx, s.log, "preflight answer cannot be built", err)
	}

	return op, nil
}

func datacenterID(name string) string { return strings.TrimPrefix(name, "datacenters/") }

func operationID(name string) string { return strings.TrimPrefix(name, "operations/") }
