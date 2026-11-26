package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// syncBuffer is a buffer a command writes to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// The operator who gives the confirmation with a flag sees the facts it rests
// on all the same, before the name is typed.
func TestFlagAnswerShowsTheFacts(t *testing.T) {
	iso := isolationCheck(map[string]any{"mode": "forced_only", "last_seen": "A-was-primary-at-10:14"})
	asked := failWith(refusal(apierr.ReasonConfirmationRequired, "x", iso))

	t.Run("before the typed name", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "dc_b\n"
		succeedingWatch(e)
		e.script(asked, answer(planOf(iso)), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

		res := e.run("failover", "dc_b", "--confirm-isolation")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.NotContains(t, res.stderr, "[y/N]", "the flag answered")
		assert.Contains(t, res.stdout, "facts of coord.isolation")
		assert.Contains(t, res.stdout, "A-was-primary-at-10:14")
	})

	t.Run("with -o json on stderr", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "dc_b\n"
		succeedingWatch(e)
		e.script(asked, answer(planOf(iso)), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

		res := e.run("failover", "dc_b", "--confirm-isolation", "-o", "json")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.Contains(t, res.stderr, "A-was-primary-at-10:14")
		assert.NotContains(t, res.stdout, "facts of", "stdout carries JSON only")
	})

	t.Run("in the table of --dry-run", func(t *testing.T) {
		e := newEnv(t)
		e.script(asked, answer(planOf(iso)))

		res := e.run("failover", "dc_b", "--dry-run", "--confirm-isolation")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.Contains(t, res.stdout, "facts of coord.isolation")
		assert.Contains(t, res.stdout, "A-was-primary-at-10:14")
	})

	t.Run("in the refusal of --dry-run without the flag", func(t *testing.T) {
		e := newEnv(t)
		e.script(asked)

		res := e.run("failover", "dc_b", "--dry-run")

		assert.Equal(t, ExitFailed, res.code)
		assert.Contains(t, res.stderr, "A-was-primary-at-10:14")
	})
}

// A state the client does not know is not a success, whichever way it gets
// the operation.
func TestUnspecifiedStartIsNotSuccess(t *testing.T) {
	e := newEnv(t)
	e.script(answer(&dcctlv1.Operation{Name: "operations/op7"}))

	res := e.run("switch", "dc_b", "--yes")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "does not know")

	e = newEnv(t)
	e.script(answer(&dcctlv1.Operation{Name: "operations/op7", State: dcctlv1.Operation_State(42)}))

	assert.Equal(t, ExitFailed, e.run("switch", "dc_b", "--yes", "--detach").code)
}

func TestCtrlCAtThePrompt(t *testing.T) {
	for name, args := range map[string][]string{
		"typed name": {"switch", "dc_b"},
		"question":   {"failover", "dc_b"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)

			if name == "typed name" {
				e.script(answer(planOf()))
			} else {
				e.script(failWith(refusal(apierr.ReasonConfirmationRequired, "x", isolationCheck(nil))))
			}

			pr, pw := io.Pipe()

			t.Cleanup(func() { _ = pw.Close() })

			errOut := &syncBuffer{}

			var out bytes.Buffer

			app := e.app(&out, &bytes.Buffer{})
			app.Reader = pr
			app.ErrWriter = errOut

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan int, 1)

			go func() {
				err := app.RunContext(ctx, append([]string{"webitel-dcctl"}, args...))
				done <- Report(errOut, err)
			}()

			require.Eventually(t, func() bool {
				return strings.Contains(errOut.String(), "to continue") || strings.Contains(errOut.String(), "[y/N]")
			}, 5*time.Second, time.Millisecond)

			cancel()

			select {
			case code := <-done:
				assert.Equal(t, ExitCanceled, code)
			case <-time.After(5 * time.Second):
				t.Fatal("Ctrl-C did not end the command at the prompt")
			}

			assertNoStart(t, e, rpcSwitch)
			assertNoStart(t, e, rpcFailover)
		})
	}
}

// Ctrl-C while the call runs: the action may have started, and the operator
// is told how to find out.
func TestCtrlCOnTheCall(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"switch", []string{"switch", "dc_b", "--yes"}, "repeat the command with --request-id"},
		{"resume", []string{"resume", "op7", "--yes"}, `"webitel-dcctl ops"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }

			started := make(chan struct{})
			release := make(chan struct{})

			t.Cleanup(func() { close(release) })

			e.api.act = func(string, proto.Message) (*dcctlv1.Operation, error) {
				close(started)
				<-release

				return nil, apierr.New(apierr.ReasonLockBusy, "late")
			}

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan result, 1)

			go func() { done <- e.runContext(ctx, tt.args...) }()

			<-started
			cancel()

			res := <-done

			assert.Equal(t, ExitCanceled, res.code, res.stderr)
			assert.Contains(t, res.stderr, tt.want)
		})
	}
}

func TestRequestIDIsNotForResumeAndAbandon(t *testing.T) {
	e := newEnv(t)

	for _, cmd := range []string{"resume", "abandon"} {
		res := e.run(cmd, "op7", "--request-id", "0b9a3e1c-1111-4222-8333-444455556666")

		assert.Equal(t, ExitUsage, res.code, cmd)
		assert.Contains(t, res.stderr, cmd+" has no request_id")
	}

	assert.Empty(t, e.api.methods())
}

func TestResumeHelpWarnsAboutYes(t *testing.T) {
	e := newEnv(t)

	res := e.run("resume", "--help")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stdout, "FORCED")
}

// The first message of the stream after a resume may be an older copy than the
// operation the resume returned.
func TestStaleFirstMessageAfterResume(t *testing.T) {
	e := newEnv(t)

	stale := testOperation("op7", dcctlv1.Operation_INTERRUPTED)
	stale.Revision = 5
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return stale, nil }

	resumed := testOperation("op7", dcctlv1.Operation_RUNNING)
	resumed.Revision = 6

	done := testOperation("op7", dcctlv1.Operation_SUCCEEDED)
	done.Revision = 7

	e.script(answer(resumed))
	e.api.watch = stream(false, stale, done)

	res := e.run("resume", "op7", "--yes")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.NotContains(t, res.stdout, "INTERRUPTED")
}
