package cli

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

const (
	methodWatch = "/webitel.dcctl.v1.OperationService/WatchOperation"
	methodGetOp = "/webitel.dcctl.v1.OperationService/GetOperation"
)

// stream sends ops and then ends the stream, or keeps it open when hold is
// set, as the server does after INTERRUPTED.
func stream(hold bool, ops ...*dcctlv1.Operation) func(*dcctlv1.WatchOperationRequest, grpc.ServerStreamingServer[dcctlv1.Operation]) error {
	return func(_ *dcctlv1.WatchOperationRequest, s grpc.ServerStreamingServer[dcctlv1.Operation]) error {
		for _, op := range ops {
			if err := s.Send(op); err != nil {
				return err
			}
		}

		if hold {
			<-s.Context().Done()
		}

		return nil
	}
}

// opAt returns the operation as it is after step steps of the switch.
func opAt(state dcctlv1.Operation_State, steps ...dcctlv1.Operation_Step_State) *dcctlv1.Operation {
	op := testOperation("op7", state)
	op.Steps = nil

	for i, st := range steps {
		op.Steps = append(op.Steps, &dcctlv1.Operation_Step{
			Index: int32(i + 1), Type: dcctlv1.Operation_Step_APPS_STOP, State: st, Datacenter: "datacenters/dc_a",
		})
	}

	return op
}

func TestWatchUntilSucceeded(t *testing.T) {
	e := newEnv(t)
	running := opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING)
	e.api.watch = stream(false,
		opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_PENDING),
		running,
		running, // the keepalive repeats the state
		opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED),
	)

	res := e.run("watch", "operations/op7")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "watch_table", res.stdout)

	assert.Equal(t, []string{methodWatch}, e.api.methods(), "no call besides the stream")
}

func TestWatchExitCodes(t *testing.T) {
	withWarning := opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED)
	withWarning.Warnings = []string{"dc_c was not repointed"}

	stepWarning := opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED)
	stepWarning.Steps[0].Warnings = []string{"slow"}

	failed := opAt(dcctlv1.Operation_FAILED, dcctlv1.Operation_Step_FAILED)
	failed.Error = &dcctlv1.OperationError{Reason: "STEP_FAILED", Detail: "patroni did not answer"}

	tests := []struct {
		name   string
		last   *dcctlv1.Operation
		hold   bool
		code   int
		stdout string
		stderr string
	}{
		{"succeeded", opAt(dcctlv1.Operation_SUCCEEDED), false, ExitOK, "SUCCEEDED", ""},
		{"warnings", withWarning, false, ExitWarnings, "warning: dc_c was not repointed", ""},
		{"step warnings", stepWarning, false, ExitWarnings, "warning: slow", ""},
		{"failed", failed, false, ExitFailed, "error: STEP_FAILED: patroni did not answer", "operation op7 failed: patroni did not answer [STEP_FAILED]"},
		{"abandoned", opAt(dcctlv1.Operation_ABANDONED), false, ExitFailed, "ABANDONED", "was abandoned"},
		{"interrupted", opAt(dcctlv1.Operation_INTERRUPTED), true, ExitInterrupted, "INTERRUPTED", `webitel-dcctl resume op7" or give it up with "webitel-dcctl abandon op7`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.api.watch = stream(tt.hold, opAt(dcctlv1.Operation_RUNNING), tt.last)

			res := e.run("watch", "op7")

			assert.Equal(t, tt.code, res.code, res.stderr)
			assert.Contains(t, res.stdout, tt.stdout)
			assert.Contains(t, res.stderr, tt.stderr)
		})
	}
}

func TestWatchReconnects(t *testing.T) {
	e := newEnv(t)

	var streams atomic.Int32

	e.api.watch = func(r *dcctlv1.WatchOperationRequest, s grpc.ServerStreamingServer[dcctlv1.Operation]) error {
		if streams.Add(1) == 1 {
			_ = s.Send(opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING))

			return apierr.New(apierr.ReasonLeaderUnavailable, "the leading instance went away")
		}

		return stream(false, opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED))(r, s)
	}
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
		return opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING), nil
	}

	res := e.run("watch", "op7")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.EqualValues(t, 2, streams.Load())
	assert.Equal(t, 1, countOf(e.api.methods(), methodGetOp), "the operation is read again before the second stream")
}

func TestWatchEndedWhileDisconnected(t *testing.T) {
	e := newEnv(t)

	var streams atomic.Int32

	e.api.watch = func(_ *dcctlv1.WatchOperationRequest, s grpc.ServerStreamingServer[dcctlv1.Operation]) error {
		streams.Add(1)

		_ = s.Send(opAt(dcctlv1.Operation_RUNNING))

		return status.Error(codes.Unavailable, "connection reset")
	}
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
		return opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED), nil
	}

	res := e.run("watch", "op7")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.EqualValues(t, 1, streams.Load())
	assert.Contains(t, res.stdout, "SUCCEEDED")
}

func TestWatchGivesUp(t *testing.T) {
	e := newEnv(t)

	var streams atomic.Int32

	e.api.watch = func(*dcctlv1.WatchOperationRequest, grpc.ServerStreamingServer[dcctlv1.Operation]) error {
		streams.Add(1)

		return status.Error(codes.Unavailable, "no leader")
	}
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
		return opAt(dcctlv1.Operation_RUNNING), nil
	}

	res := e.run("watch", "op7")

	assert.Equal(t, ExitFailed, res.code)
	assert.EqualValues(t, maxWatchFailures+1, streams.Load())
	assert.Contains(t, res.stderr, "webitel-dcctl watch op7")
}

func TestWatchRefusal(t *testing.T) {
	e := newEnv(t)

	var streams atomic.Int32

	e.api.watch = func(*dcctlv1.WatchOperationRequest, grpc.ServerStreamingServer[dcctlv1.Operation]) error {
		streams.Add(1)

		return apierr.Status(apierr.ReasonOperationNotFound, "operation op7 not found",
			map[string]string{"operation": "operations/op7"}).Err()
	}

	res := e.run("watch", "op7")

	assert.Equal(t, ExitFailed, res.code)
	assert.EqualValues(t, 1, streams.Load(), "an answer is not retried")
	assert.Contains(t, res.stderr, "operation op7 not found [OPERATION_NOT_FOUND]")
}

func TestWatchCanceled(t *testing.T) {
	e := newEnv(t)
	e.api.watch = stream(true, opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING))

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan result)

	go func() { done <- e.runContext(ctx, "watch", "op7") }()

	require.Eventually(t, func() bool { return len(e.api.methods()) == 1 }, 5*time.Second, time.Millisecond)

	// The first message is printed once the stream is open.
	time.Sleep(50 * time.Millisecond)
	cancel()

	res := <-done

	assert.Equal(t, ExitCanceled, res.code)
	assert.Contains(t, res.stderr, "the operation keeps running")
	assert.Contains(t, res.stderr, "webitel-dcctl watch op7")
}

func TestWatchJSON(t *testing.T) {
	e := newEnv(t)
	e.api.watch = stream(false,
		opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING),
		opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING),
		opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED),
	)

	res := e.run("ops", "watch", "op7", "-o", "json")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "watch_json", res.stdout)

	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 2, "the keepalive is not a line of output")

	var last dcctlv1.Operation

	require.NoError(t, protoJSONUnmarshal([]byte(lines[1]), &last))
	assert.Equal(t, dcctlv1.Operation_SUCCEEDED, last.GetState())
	assert.True(t, proto.Equal(opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED), &last))
}

func TestWatchUsage(t *testing.T) {
	e := newEnv(t)

	for _, args := range [][]string{
		{"watch"},
		{"watch", "a", "b"},
		{"watch", "a b"},
		{"ops", "watch"},
	} {
		assert.Equal(t, ExitUsage, e.run(args...).code, args)
	}

	assert.Empty(t, e.api.methods())
}

func countOf(xs []string, x string) int {
	n := 0

	for _, v := range xs {
		if v == x {
			n++
		}
	}

	return n
}

func TestWatchUnknownStateIsNotSuccess(t *testing.T) {
	unknown := opAt(dcctlv1.Operation_State(42))
	running := opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING)

	for name, last := range map[string]*dcctlv1.Operation{
		"a state of a newer server": unknown,
		"unspecified":               opAt(dcctlv1.Operation_STATE_UNSPECIFIED),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.api.watch = stream(false, running, last)

			res := e.run("watch", "op7")

			assert.Equal(t, ExitFailed, res.code)
			assert.Contains(t, res.stderr, "does not know")
		})
	}

	// After a reconnect the operation is read in that state.
	e := newEnv(t)
	e.api.watch = stream(false, running)
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return unknown, nil }

	assert.Equal(t, ExitFailed, e.run("watch", "op7").code)
}

func TestWatchIgnoresAnOlderRevision(t *testing.T) {
	e := newEnv(t)

	newer := opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING)
	newer.Revision = 6
	stale := opAt(dcctlv1.Operation_INTERRUPTED, dcctlv1.Operation_Step_FAILED)
	stale.Revision = 5
	done := opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED)
	done.Revision = 7

	var streams atomic.Int32

	e.api.watch = func(_ *dcctlv1.WatchOperationRequest, s grpc.ServerStreamingServer[dcctlv1.Operation]) error {
		if streams.Add(1) == 1 {
			_ = s.Send(newer)

			return status.Error(codes.Unavailable, "gone")
		}

		_ = s.Send(stale) // a copy of an older write

		return s.Send(done)
	}
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return stale, nil }

	res := e.run("watch", "op7")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.EqualValues(t, 2, streams.Load())
	assert.NotContains(t, res.stdout, "INTERRUPTED")
}
