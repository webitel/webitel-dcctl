package api

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

func TestActionForwardsToTargetDatacenter(t *testing.T) {
	env := newActionEnv()
	env.catalog.set("dc_b", "node-b", "10.1.0.1:10023")
	dcs, _ := env.servers(t)

	req := &dcctlv1.FailoverDatacenterRequest{
		Name: "datacenters/dc_b", ValidateOnly: true, Confirmation: &dcctlv1.Confirmation{Isolation: true},
	}

	op, err := dcs.FailoverDatacenter(aliceCtx(t), req)
	require.NoError(t, err)
	assert.Equal(t, "operations/remote", op.GetName(), "the answer of the leading instance")

	calls := env.forwarder.forwarded()
	require.Len(t, calls, 1)
	assert.Equal(t, "10.1.0.1:10023", calls[0].address)
	assert.Equal(t, dcctlv1.DatacenterService_FailoverDatacenter_FullMethodName, calls[0].method)
	assertProto(t, req, calls[0].req)

	keys := make([]string, 0, len(calls[0].md))
	for k := range calls[0].md {
		keys = append(keys, k)
	}

	assert.Equal(t, []string{auth.InitiatorKey}, keys, "only the initiator goes to the leading instance")
	assert.Empty(t, env.preflight.requests())
	assert.Empty(t, env.engine.started())
}

func TestActionLeaders(t *testing.T) {
	tests := []struct {
		name string
		// active is the effective active datacenter of the status.
		active  string
		call    func(ctx context.Context, s *DatacenterServer) error
		forward bool
	}{
		{
			name: "resolve_winner_elsewhere", active: "dc_a", forward: true,
			call: func(ctx context.Context, s *DatacenterServer) error {
				_, err := s.ResolveConflict(ctx, &dcctlv1.ResolveConflictRequest{Winner: "datacenters/dc_b"})

				return err
			},
		},
		{
			name: "sync_apps_active_elsewhere", active: "dc_b", forward: true,
			call: func(ctx context.Context, s *DatacenterServer) error {
				_, err := s.SyncApps(ctx, &dcctlv1.SyncAppsRequest{})

				return err
			},
		},
		{
			name: "sync_apps_active_here", active: "dc_a",
			call: func(ctx context.Context, s *DatacenterServer) error {
				_, err := s.SyncApps(ctx, &dcctlv1.SyncAppsRequest{})

				return err
			},
		},
		{
			name: "sync_apps_active_unknown", active: "dc_x",
			call: func(ctx context.Context, s *DatacenterServer) error {
				_, err := s.SyncApps(ctx, &dcctlv1.SyncAppsRequest{})

				return err
			},
		},
		{
			name: "initialize_here", active: "dc_b",
			call: func(ctx context.Context, s *DatacenterServer) error {
				_, err := s.InitializeDatacenters(ctx, &dcctlv1.InitializeDatacentersRequest{ActiveDatacenter: "datacenters/dc_b"})

				return err
			},
		},
		{
			name: "initialize_arbiter_here", active: "dc_b",
			call: func(ctx context.Context, s *DatacenterServer) error {
				_, err := s.InitializeArbiter(ctx, &dcctlv1.InitializeArbiterRequest{})

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newActionEnv()
			env.status.st.Effective.ActiveDatacenter = tt.active
			env.catalog.set("dc_b", "node-b", "10.1.0.1:10023")
			dcs, _ := env.servers(t)

			require.NoError(t, tt.call(aliceCtx(t), dcs))

			if tt.forward {
				assert.Len(t, env.forwarder.forwarded(), 1)
				assert.Empty(t, env.engine.started())
			} else {
				assert.Empty(t, env.forwarder.forwarded())
				assert.Len(t, env.engine.started(), 1)
			}
		})
	}
}

// TestActionForwardedIsNotForwardedAgain checks the hop limit: an instance
// that does not lead a forwarded action runs its preflight, which refuses it,
// instead of forwarding it again.
func TestActionForwardedIsNotForwardedAgain(t *testing.T) {
	env := newActionEnv()
	env.catalog.set("dc_b", "node-b", "10.1.0.1:10023")
	env.preflight.refuse("switch", check.Check{
		ID: check.CoordLeaderReachable, Result: check.Fail, Effect: check.Block, Detail: "this instance does not lead the action",
		Facts: map[string]any{"instance": "node-a", "leader_datacenters": []any{"dc_b"}},
	})
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(forwardedCtx(t, aliceFrom("node-c", "dc_c")), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	st := assertCode(t, codes.Unavailable, err)
	assert.Equal(t, apierr.ReasonLeaderUnavailable, reasonOf(st))
	assert.Equal(t, map[string]string{"datacenters": "dc_b"}, metadataOf(st))
	assert.Empty(t, env.forwarder.forwarded())
	assert.Len(t, env.preflight.requests(), 1)
}

func TestActionForwardedKeepsInitiator(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	ini := aliceFrom("node-b", "dc_b")

	_, err := dcs.SwitchDatacenter(forwardedCtx(t, ini), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a"})
	require.NoError(t, err)

	plans := env.engine.started()
	require.Len(t, plans, 1)
	assertProto(t, ini, plans[0].Initiator)
}

// TestActionSameNodeNameElsewhere checks that an instance of another
// datacenter with the node name of this one leads there.
func TestActionSameNodeNameElsewhere(t *testing.T) {
	env := newActionEnv()
	env.catalog.set("dc_b", "node-a", "10.1.0.1:10023")
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	require.NoError(t, err)

	calls := env.forwarder.forwarded()
	require.Len(t, calls, 1)
	assert.Equal(t, "10.1.0.1:10023", calls[0].address)
}

func TestActionLeaderUnavailable(t *testing.T) {
	env := newActionEnv()
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	st := assertCode(t, codes.Unavailable, err)
	assert.Equal(t, apierr.ReasonLeaderUnavailable, reasonOf(st))
	assert.Equal(t, map[string]string{"datacenters": "dc_b"}, metadataOf(st))
	assert.Empty(t, env.engine.started())
}

func TestLeaderAnswer(t *testing.T) {
	refusal := answer.Refusal(check.Decide([]check.Check{blockedCheck()}, check.Confirmations{}), nil, dcctlv1.Operation_QUORUM)

	tests := []struct {
		name   string
		err    error
		code   codes.Code
		reason apierr.Reason
		// same is set when the error must reach the client as it is.
		same bool
		// text is in the message when set.
		text string
	}{
		{name: "refusal", err: refusal, code: codes.FailedPrecondition, reason: apierr.ReasonPreflightFailed, same: true},
		{
			name: "connection_refused", code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable,
			err: status.Error(codes.Unavailable, "connection error: desc = transport: dial tcp 10.1.0.1:10023: connect: connection refused"),
		},
		{
			name: "deadline", err: status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable, text: "request_id",
		},
		{
			name: "certificate_denied", code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable,
			err: status.Error(codes.PermissionDenied, "client certificate is not of a registered dcctl instance"),
		},
		{name: "not_a_status", err: errors.New("dial 10.1.0.1:10023"), code: codes.Unavailable, reason: apierr.ReasonLeaderUnavailable},
		{name: "internal", err: status.Error(codes.Internal, "grpc: failed to decode from 10.1.0.1:10023"), code: codes.Internal},
		{name: "unimplemented", err: status.Error(codes.Unimplemented, "switch is not implemented"), code: codes.Unimplemented, same: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newActionEnv()
			env.catalog.set("dc_b", "node-b", "10.1.0.1:10023")
			env.forwarder.err = tt.err
			dcs, _ := env.servers(t)

			_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
			st := assertCode(t, tt.code, err)
			assert.Equal(t, tt.reason, reasonOf(st))
			assert.NotContains(t, st.Message(), "10.1.0.1")

			if tt.same {
				assertProto(t, status.Convert(tt.err).Proto(), st.Proto())
			}

			if tt.text != "" {
				assert.Contains(t, st.Message(), tt.text)
			}
		})
	}
}
