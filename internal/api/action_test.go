package api

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// actionCase is one action RPC run by node-a of dc_a, which leads it.
type actionCase struct {
	name   string
	typ    dcctlv1.Operation_Type
	action preflight.Action
	// target is the datacenter the preflight is run for.
	target string
	// req returns a request; isolation changes its body.
	req  func(validateOnly bool, id string, isolation bool) actionRequest
	call func(ctx context.Context, s *DatacenterServer, req actionRequest) (*dcctlv1.Operation, error)
}

func conf(isolation bool) *dcctlv1.Confirmation {
	if !isolation {
		return nil
	}

	return &dcctlv1.Confirmation{Isolation: true}
}

func actionCases() []actionCase {
	return []actionCase{
		{
			name: "switch", typ: dcctlv1.Operation_SWITCH, action: preflight.Switch, target: "dc_a",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.SwitchDatacenter(ctx, r.(*dcctlv1.SwitchDatacenterRequest))
			},
		},
		{
			name: "failover", typ: dcctlv1.Operation_FAILOVER, action: preflight.Failover, target: "dc_a",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.FailoverDatacenterRequest{Name: "datacenters/dc_a", ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.FailoverDatacenter(ctx, r.(*dcctlv1.FailoverDatacenterRequest))
			},
		},
		{
			name: "demote", typ: dcctlv1.Operation_DEMOTE, action: preflight.Demote, target: "dc_a",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.DemoteDatacenterRequest{Name: "datacenters/dc_a", ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.DemoteDatacenter(ctx, r.(*dcctlv1.DemoteDatacenterRequest))
			},
		},
		{
			name: "repoint", typ: dcctlv1.Operation_REPOINT, action: preflight.Repoint, target: "dc_a",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.RepointDatacenterRequest{Name: "datacenters/dc_a", ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.RepointDatacenter(ctx, r.(*dcctlv1.RepointDatacenterRequest))
			},
		},
		{
			name: "mark_recovered", typ: dcctlv1.Operation_MARK_RECOVERED, action: preflight.MarkRecovered, target: "dc_a",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.MarkDatacenterRecoveredRequest{Name: "datacenters/dc_a", ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.MarkDatacenterRecovered(ctx, r.(*dcctlv1.MarkDatacenterRecoveredRequest))
			},
		},
		{
			name: "resolve_conflict", typ: dcctlv1.Operation_RESOLVE_CONFLICT, action: preflight.Resolve, target: "dc_a",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.ResolveConflictRequest{Winner: "datacenters/dc_a", ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.ResolveConflict(ctx, r.(*dcctlv1.ResolveConflictRequest))
			},
		},
		{
			name: "initialize", typ: dcctlv1.Operation_INITIALIZE, action: preflight.Init, target: "dc_b",
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.InitializeDatacentersRequest{
					ActiveDatacenter: "datacenters/dc_b", ValidateOnly: v, RequestId: id, Confirmation: conf(iso),
				}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.InitializeDatacenters(ctx, r.(*dcctlv1.InitializeDatacentersRequest))
			},
		},
		{
			name: "initialize_arbiter", typ: dcctlv1.Operation_INITIALIZE_ARBITER, action: preflight.ArbiterInit,
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.InitializeArbiterRequest{ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.InitializeArbiter(ctx, r.(*dcctlv1.InitializeArbiterRequest))
			},
		},
		{
			name: "sync_apps", typ: dcctlv1.Operation_SYNC_APPS, action: preflight.AppsSync,
			req: func(v bool, id string, iso bool) actionRequest {
				return &dcctlv1.SyncAppsRequest{ValidateOnly: v, RequestId: id, Confirmation: conf(iso)}
			},
			call: func(ctx context.Context, s *DatacenterServer, r actionRequest) (*dcctlv1.Operation, error) {
				return s.SyncApps(ctx, r.(*dcctlv1.SyncAppsRequest))
			},
		},
	}
}

func TestActionValidateOnly(t *testing.T) {
	for _, tc := range actionCases() {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionEnv()
			dcs, _ := env.servers(t)

			op, err := tc.call(aliceCtx(t), dcs, tc.req(true, requestA, false))
			require.NoError(t, err)

			assert.Empty(t, op.GetName())
			assert.Empty(t, op.GetRequestId())
			assert.Equal(t, tc.typ, op.GetType())
			assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode())
			assert.Equal(t, int64(5), op.GetNewGeneration())
			require.Len(t, op.GetSteps(), 1)
			assert.Equal(t, dcctlv1.Operation_Step_PENDING, op.GetSteps()[0].GetState())
			assert.Equal(t, aliceUser, op.GetInitiator().GetUser())

			stored := requestOf(op)
			require.NotNil(t, stored, "the request is in the answer")
			assert.False(t, stored.(actionRequest).GetValidateOnly())

			assert.Empty(t, env.engine.started(), "validate_only starts nothing")

			runs := env.preflight.requests()
			require.Len(t, runs, 1)
			assert.Equal(t, tc.action, runs[0].Action)
			assert.Equal(t, tc.target, runs[0].Params.Datacenter)
		})
	}
}

// TestActionRefusalIsTheSame checks that validate_only is refused with the
// error of the request itself, with the plan in the Preflight detail.
func TestActionRefusalIsTheSame(t *testing.T) {
	for _, tc := range actionCases() {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionEnv()
			env.preflight.refuse(tc.action, blockedCheck())
			dcs, _ := env.servers(t)

			_, dryErr := tc.call(aliceCtx(t), dcs, tc.req(true, "", false))
			_, realErr := tc.call(aliceCtx(t), dcs, tc.req(false, requestA, false))

			st := assertCode(t, codes.FailedPrecondition, dryErr)
			assert.Equal(t, apierr.ReasonPreflightFailed, reasonOf(st))
			assert.True(t, proto.Equal(st.Proto(), status.Convert(realErr).Proto()), "dry run: %v; request: %v", dryErr, realErr)

			pre := preflightOf(st)
			require.NotNil(t, pre)
			require.Len(t, pre.GetSteps(), 1)
			assert.Equal(t, dcctlv1.Operation_QUORUM, pre.GetMode())

			assert.Empty(t, env.engine.started())
		})
	}
}

func TestActionStarts(t *testing.T) {
	for _, tc := range actionCases() {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionEnv()
			dcs, _ := env.servers(t)

			op, err := tc.call(aliceCtx(t), dcs, tc.req(false, requestA, false))
			require.NoError(t, err)
			assert.Equal(t, "operations/op1", op.GetName())

			plans := env.engine.started()
			require.Len(t, plans, 1)

			p := plans[0]
			assert.Equal(t, tc.typ, p.Type)
			assert.Equal(t, requestA, p.RequestID)
			assert.Equal(t, tc.action, p.Preflight.Action)
			assert.Equal(t, aliceUser, p.Initiator.GetUser())
			assert.Equal(t, clientIP, p.Initiator.GetAddress())
			assert.Equal(t, "node-a", p.Initiator.GetInstance().GetNode())
			assert.Equal(t, "datacenters/dc_a", p.Initiator.GetInstance().GetDatacenter())
			assert.True(t, proto.Equal(storedRequest(tc.req(false, requestA, false)), p.Request), "stored request %v", p.Request)
			assert.Empty(t, env.forwarder.forwarded())
		})
	}
}

func TestActionReplaysRequestID(t *testing.T) {
	for _, tc := range actionCases() {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionEnv()
			dcs, _ := env.servers(t)

			first, err := tc.call(aliceCtx(t), dcs, tc.req(false, requestA, false))
			require.NoError(t, err)

			again, err := tc.call(aliceCtx(t), dcs, tc.req(false, requestA, false))
			require.NoError(t, err)
			assert.Equal(t, first.GetName(), again.GetName())
			assert.Len(t, env.engine.started(), 1, "one operation for one request_id")
		})
	}
}

func TestActionRequestIDMismatch(t *testing.T) {
	for _, tc := range actionCases() {
		t.Run(tc.name, func(t *testing.T) {
			env := newActionEnv()
			dcs, _ := env.servers(t)

			first, err := tc.call(aliceCtx(t), dcs, tc.req(false, requestA, false))
			require.NoError(t, err)

			_, err = tc.call(aliceCtx(t), dcs, tc.req(false, requestA, true))
			st := assertCode(t, codes.AlreadyExists, err)
			assert.Equal(t, apierr.ReasonRequestIDMismatch, reasonOf(st))
			assert.Equal(t, map[string]string{"operation": first.GetName()}, metadataOf(st))
			assert.Len(t, env.engine.started(), 1)
		})
	}
}

func TestActionDryRunKeepsRequestID(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{
		Name: "datacenters/dc_a", ValidateOnly: true, RequestId: requestA,
	})
	require.NoError(t, err)

	op, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{
		Name: "datacenters/dc_a", RequestId: requestA, Confirmation: &dcctlv1.Confirmation{Isolation: true},
	})
	require.NoError(t, err)
	assert.Equal(t, requestA, op.GetRequestId())
}

// TestActionUnknownFieldsAreTheSameBody checks a request with fields this
// dcctl does not know against its operation as dcctl/op keeps it.
func TestActionUnknownFieldsAreTheSameBody(t *testing.T) {
	req := &dcctlv1.SwitchDatacenterRequest{
		Name: "datacenters/dc_a", RequestId: requestA, Confirmation: &dcctlv1.Confirmation{Isolation: true},
	}
	req.ProtoReflect().SetUnknown([]byte{0xa8, 0x1f, 0x01})
	req.GetConfirmation().ProtoReflect().SetUnknown([]byte{0xa8, 0x1f, 0x01})

	op := &dcctlv1.Operation{Name: "operations/op1", RequestId: requestA}
	setRequest(op, storedRequest(req))

	raw, err := protojson.Marshal(op)
	require.NoError(t, err)

	kept := &dcctlv1.Operation{}
	require.NoError(t, protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, kept))

	assert.True(t, sameRequest(requestOf(kept), req))
}

func TestActionEmptyConfirmationIsTheSameBody(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	first, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", RequestId: requestA})
	require.NoError(t, err)

	again, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{
		Name: "datacenters/dc_a", RequestId: requestA, Confirmation: &dcctlv1.Confirmation{},
	})
	require.NoError(t, err)
	assert.Equal(t, first.GetName(), again.GetName())
}

// TestActionReplayAfterRace checks a retry that loses the race to the same
// request: the engine refuses it, and the operation the other started is
// answered.
func TestActionReplayAfterRace(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	req := &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", RequestId: requestA}

	winner := &dcctlv1.Operation{
		Name: "operations/winner", Type: dcctlv1.Operation_SWITCH, State: dcctlv1.Operation_RUNNING, RequestId: requestA,
	}
	setRequest(winner, storedRequest(req))

	env.engine.ops = nil
	env.engine.startErr = &coord.OperationRunningError{Name: "dc_a", Operation: winner}
	env.engine.onStart = func() { env.ops.put(winner) }

	op, err := dcs.SwitchDatacenter(aliceCtx(t), req)
	require.NoError(t, err)
	assert.Equal(t, "operations/winner", op.GetName())
}

// TestActionReplayAfterRefusal checks that any refusal of an idempotent call
// looks for the request_id again.
func TestActionReplayAfterRefusal(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	req := &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", RequestId: requestA}

	winner := &dcctlv1.Operation{
		Name: "operations/winner", Type: dcctlv1.Operation_SWITCH, State: dcctlv1.Operation_SUCCEEDED, RequestId: requestA,
	}
	setRequest(winner, storedRequest(req))

	env.engine.ops = nil
	env.engine.startErr = answer.Refusal(check.Decide([]check.Check{blockedCheck()}, check.Confirmations{}), nil, dcctlv1.Operation_QUORUM)
	env.engine.onStart = func() { env.ops.put(winner) }

	op, err := dcs.SwitchDatacenter(aliceCtx(t), req)
	require.NoError(t, err)
	assert.Equal(t, "operations/winner", op.GetName())
}

func TestActionUnknownDatacenter(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_x"})
	st := assertCode(t, codes.NotFound, err)
	assert.Equal(t, apierr.ReasonDatacenterNotFound, reasonOf(st))
	assert.Equal(t, map[string]string{"datacenter": "datacenters/dc_x"}, metadataOf(st))
	assert.Empty(t, env.forwarder.forwarded())
}

func TestActionUnimplemented(t *testing.T) {
	t.Run("no_planner", func(t *testing.T) {
		env := newActionEnv()
		env.planners = Planners{}
		dcs, _ := env.servers(t)

		_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
		st := assertCode(t, codes.Unimplemented, err)
		assert.Empty(t, reasonOf(st))
		assert.Empty(t, env.forwarder.forwarded(), "an action without a planner is not forwarded")
		assert.Empty(t, env.preflight.requests())
	})

	t.Run("no_actions", func(t *testing.T) {
		env := newActionEnv()
		dcs := NewDatacenterServer(env.status, observerAddress, testLogger())
		ops := NewOperationServer(env.ops, nil, nil, testLogger())

		_, err := dcs.FailoverDatacenter(aliceCtx(t), &dcctlv1.FailoverDatacenterRequest{Name: "datacenters/dc_a"})
		assertCode(t, codes.Unimplemented, err)

		_, err = ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/x"})
		assertCode(t, codes.Unimplemented, err)

		_, err = ops.AbandonOperation(aliceCtx(t), &dcctlv1.AbandonOperationRequest{Name: "operations/x"})
		assertCode(t, codes.Unimplemented, err)
	})
}

func TestActionNeedsInitiator(t *testing.T) {
	env := newActionEnv()
	dcs, ops := env.servers(t)

	_, err := dcs.SwitchDatacenter(t.Context(), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a"})
	st := assertCode(t, codes.Unauthenticated, err)
	assert.Equal(t, apierr.ReasonAuthenticationRequired, reasonOf(st))

	_, err = ops.AbandonOperation(t.Context(), &dcctlv1.AbandonOperationRequest{Name: "operations/x"})
	assertCode(t, codes.Unauthenticated, err)
	assert.Empty(t, env.preflight.requests())
}

func TestActionPlannerFails(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		env := newActionEnv()
		env.planners.Switch = fakePlanner[*dcctlv1.SwitchDatacenterRequest]{err: errors.New("cannot plan")}
		env.preflight.refuse(preflight.Switch, blockedCheck())
		dcs, _ := env.servers(t)

		_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", ValidateOnly: true})
		st := assertCode(t, codes.FailedPrecondition, err)
		assert.Equal(t, apierr.ReasonPreflightFailed, reasonOf(st))
		assert.Empty(t, preflightOf(st).GetSteps())
	})

	t.Run("allowed", func(t *testing.T) {
		env := newActionEnv()
		env.planners.Switch = fakePlanner[*dcctlv1.SwitchDatacenterRequest]{err: errors.New("cannot plan 10.0.0.1")}
		dcs, _ := env.servers(t)

		_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a"})
		st := assertCode(t, codes.Internal, err)
		assert.NotContains(t, st.Message(), "10.0.0.1")
		assert.Empty(t, env.engine.started())
	})
}

func TestActionEngineRefusal(t *testing.T) {
	env := newActionEnv()
	env.engine.startErr = &coord.LockBusyError{Name: "dc_a", Holder: kv.Lock{Instance: "node-b", Datacenter: "dc_b"}}
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a"})
	st := assertCode(t, codes.Aborted, err)
	assert.Equal(t, apierr.ReasonLockBusy, reasonOf(st))
}

func preflightOf(st *status.Status) *dcctlv1.Preflight {
	for _, d := range st.Details() {
		if p, ok := d.(*dcctlv1.Preflight); ok {
			return p
		}
	}

	return nil
}

func metadataOf(st *status.Status) map[string]string {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetMetadata()
		}
	}

	return nil
}
