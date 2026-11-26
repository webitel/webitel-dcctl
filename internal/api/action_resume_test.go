package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// interrupted returns an interrupted switch to target that node of dc led.
func interrupted(node, dc, target string) *dcctlv1.Operation {
	return &dcctlv1.Operation{
		Name: "operations/op7", Type: dcctlv1.Operation_SWITCH, State: dcctlv1.Operation_INTERRUPTED,
		Target: datacenterName(target), Leader: &dcctlv1.Instance{Node: node, Datacenter: datacenterName(dc)},
	}
}

func TestResumeValidateOnly(t *testing.T) {
	env := newActionEnv()
	env.ops.put(interrupted("node-a", "dc_a", "dc_b"))
	_, ops := env.servers(t)

	op, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{
		Name: "operations/op7", ValidateOnly: true, Confirmation: &dcctlv1.Confirmation{Isolation: true},
	})
	require.NoError(t, err)
	assert.Empty(t, op.GetName())
	assert.Equal(t, dcctlv1.Operation_FORCED, op.GetMode())

	runs := env.preflight.requests()
	require.Len(t, runs, 1)
	assert.Equal(t, preflight.Request{
		Action: preflight.Resume, Params: preflight.Params{Operation: "op7", ToForced: true},
		Confirmations: check.Confirmations{Isolation: true},
	}, runs[0])
	assert.Empty(t, env.engine.resumes)
}

func TestResumeValidateOnlyRefused(t *testing.T) {
	env := newActionEnv()
	env.preflight.refuse(preflight.Resume, check.Check{
		ID: check.OpInterrupted, Result: check.Fail, Effect: check.Block, Detail: "operation op7 is not found",
		Facts: map[string]any{"operation": "operations/op7", "found": false},
	})
	_, ops := env.servers(t)

	_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7", ValidateOnly: true})
	st := assertCode(t, codes.NotFound, err)
	assert.Equal(t, apierr.ReasonOperationNotFound, reasonOf(st))
	assert.Equal(t, map[string]string{"operation": "operations/op7"}, metadataOf(st))
}

func TestResumeStarts(t *testing.T) {
	env := newActionEnv()
	env.ops.put(interrupted("node-a", "dc_a", "dc_b"))
	_, ops := env.servers(t)

	op, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
	require.NoError(t, err)
	assert.Equal(t, "operations/op7", op.GetName())

	require.Len(t, env.engine.resumes, 1)

	got := env.engine.resumes[0]
	assert.Equal(t, "op7", got.Operation)
	assert.False(t, got.ToForced)
	assert.Equal(t, aliceUser, got.Initiator.GetUser())
	assert.Empty(t, env.preflight.requests(), "the engine runs the preflight")
	assert.Empty(t, env.forwarder.forwarded())
}

func TestAbandonValidateOnlyAndStart(t *testing.T) {
	env := newActionEnv()
	env.ops.put(interrupted("node-b", "dc_b", "dc_b"))
	_, ops := env.servers(t)

	op, err := ops.AbandonOperation(aliceCtx(t), &dcctlv1.AbandonOperationRequest{Name: "operations/op7", ValidateOnly: true})
	require.NoError(t, err)
	assert.Empty(t, op.GetName())
	require.Len(t, env.preflight.requests(), 1)
	assert.Equal(t, preflight.Abandon, env.preflight.requests()[0].Action)
	assert.Empty(t, env.engine.abandons)

	op, err = ops.AbandonOperation(aliceCtx(t), &dcctlv1.AbandonOperationRequest{
		Name: "operations/op7", Confirmation: &dcctlv1.Confirmation{Isolation: true},
	})
	require.NoError(t, err)
	assert.Equal(t, dcctlv1.Operation_ABANDONED, op.GetState())

	require.Len(t, env.engine.abandons, 1)
	assert.Equal(t, operation.AbandonRequest{
		Operation: "op7", Confirmations: check.Confirmations{Isolation: true}, Initiator: env.engine.abandons[0].Initiator,
	}, env.engine.abandons[0])
	assert.Equal(t, aliceUser, env.engine.abandons[0].Initiator.GetUser())
	assert.Empty(t, env.forwarder.forwarded(), "any instance abandons")
}

func TestResumeAbandonEngineErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   codes.Code
		reason apierr.Reason
	}{
		{
			"not_interrupted", &operation.NotInterruptedError{Name: "operations/op7", State: dcctlv1.Operation_SUCCEEDED},
			codes.FailedPrecondition, apierr.ReasonOperationNotInterrupted,
		},
		{"not_found", &operation.NotFoundError{Name: "operations/op7"}, codes.NotFound, apierr.ReasonOperationNotFound},
		{"not_found_unread", &operation.NotFoundError{Name: "operations/op7", Unreachable: []string{"dc_b"}}, codes.Unavailable, apierr.ReasonKVUnavailable},
		{"no_resumer", operation.ErrNoResumer, codes.Unimplemented, ""},
		{"lock_busy", &coord.LockBusyError{Name: "dc_a", Holder: kv.Lock{Instance: "node-b"}}, codes.Aborted, apierr.ReasonLockBusy},
		{"not_running", operation.ErrNotRunning, codes.Unavailable, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newActionEnv()
			env.engine.err = tt.err
			_, ops := env.servers(t)

			_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
			st := assertCode(t, tt.code, err)
			assert.Equal(t, tt.reason, reasonOf(st))

			_, err = ops.AbandonOperation(aliceCtx(t), &dcctlv1.AbandonOperationRequest{Name: "operations/op7"})
			st = assertCode(t, tt.code, err)
			assert.Equal(t, tt.reason, reasonOf(st))
		})
	}
}

func TestResumeForwardsToItsLeader(t *testing.T) {
	env := newActionEnv()
	env.ops.put(interrupted("node-b", "dc_b", "dc_a"))
	env.catalog.set("dc_b", "node-b2", "10.1.0.2:10023")
	env.catalog.set("dc_b", "node-b", "10.1.0.1:10023")
	_, ops := env.servers(t)

	req := &dcctlv1.ResumeOperationRequest{Name: "operations/op7", Confirmation: &dcctlv1.Confirmation{Isolation: true}}

	op, err := ops.ResumeOperation(aliceCtx(t), req)
	require.NoError(t, err)
	assert.Equal(t, "operations/remote", op.GetName())

	calls := env.forwarder.forwarded()
	require.Len(t, calls, 1)
	assert.Equal(t, "10.1.0.1:10023", calls[0].address, "the instance that led it")
	assert.Equal(t, dcctlv1.OperationService_ResumeOperation_FullMethodName, calls[0].method)
	assert.Empty(t, env.engine.resumes)
}

func TestResumeLeaderGone(t *testing.T) {
	t.Run("other_instance_of_its_datacenter", func(t *testing.T) {
		env := newActionEnv()
		env.ops.put(interrupted("node-b", "dc_b", "dc_b"))
		env.catalog.set("dc_b", "node-b2", "10.1.0.2:10023")
		_, ops := env.servers(t)

		_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
		require.NoError(t, err)

		calls := env.forwarder.forwarded()
		require.Len(t, calls, 1)
		assert.Equal(t, "10.1.0.2:10023", calls[0].address, "an instance of the target")
	})

	t.Run("this_instance_of_its_datacenter", func(t *testing.T) {
		env := newActionEnv()
		env.ops.put(interrupted("node-a2", "dc_a", "dc_a"))
		_, ops := env.servers(t)

		_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
		require.NoError(t, err)
		assert.Len(t, env.engine.resumes, 1)
		assert.Empty(t, env.forwarder.forwarded())
	})

	t.Run("nobody_of_its_datacenter", func(t *testing.T) {
		env := newActionEnv()
		env.ops.put(interrupted("node-b", "dc_b", "dc_b"))
		_, ops := env.servers(t)

		_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
		st := assertCode(t, codes.Unavailable, err)
		assert.Equal(t, map[string]string{"datacenters": "dc_b"}, metadataOf(st))
	})

	t.Run("target_is_here", func(t *testing.T) {
		env := newActionEnv()
		env.ops.put(interrupted("node-b", "dc_b", "dc_a"))
		_, ops := env.servers(t)

		_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
		require.NoError(t, err)
		assert.Len(t, env.engine.resumes, 1)
		assert.Empty(t, env.forwarder.forwarded())
	})

	t.Run("target_elsewhere", func(t *testing.T) {
		env := newActionEnv()
		env.ops.put(interrupted("node-b", "dc_b", "dc_c"))
		env.catalog.set("dc_c", "node-c", "10.2.0.1:10023")
		_, ops := env.servers(t)

		_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
		require.NoError(t, err)

		calls := env.forwarder.forwarded()
		require.Len(t, calls, 1)
		assert.Equal(t, "10.2.0.1:10023", calls[0].address)
	})

	t.Run("nobody", func(t *testing.T) {
		env := newActionEnv()
		env.ops.put(interrupted("node-b", "dc_b", "dc_c"))
		_, ops := env.servers(t)

		_, err := ops.ResumeOperation(aliceCtx(t), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
		st := assertCode(t, codes.Unavailable, err)
		assert.Equal(t, apierr.ReasonLeaderUnavailable, reasonOf(st))
		assert.Equal(t, map[string]string{"datacenters": "dc_b,dc_c"}, metadataOf(st))
		assert.Empty(t, env.engine.resumes)
	})
}

func TestResumeForwardedRunsHere(t *testing.T) {
	env := newActionEnv()
	env.ops.put(interrupted("node-b", "dc_b", "dc_b"))
	env.catalog.set("dc_b", "node-b", "10.1.0.1:10023")
	_, ops := env.servers(t)

	ini := aliceFrom("node-c", "dc_c")

	_, err := ops.ResumeOperation(forwardedCtx(t, ini), &dcctlv1.ResumeOperationRequest{Name: "operations/op7"})
	require.NoError(t, err)
	assert.Empty(t, env.forwarder.forwarded(), "a forwarded call is not forwarded again")
	require.Len(t, env.engine.resumes, 1)
	assertProto(t, ini, env.engine.resumes[0].Initiator)
}
