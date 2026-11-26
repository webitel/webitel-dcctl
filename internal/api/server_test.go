package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/infra/health"
	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/runtime"
	dcstatus "github.com/webitel/webitel-dcctl/internal/status"
	"github.com/webitel/webitel-dcctl/internal/testpki"
	"github.com/webitel/webitel-dcctl/internal/validation"
)

// harness is the API served by a real runtime on loopback: gRPC over h2c and
// the gateway under /api/ on listen.http.
type harness struct {
	conn *grpc.ClientConn
	base string
	logs *logtest.Recorder
}

// fakeStatusSource gives st, or nothing before the first cycle when !ok.
type fakeStatusSource struct {
	st dcstatus.Status
	ok bool
}

func (f fakeStatusSource) Status(time.Time) (dcstatus.Status, bool) { return f.st, f.ok }

type fakeAlarms []*dcctlv1.Status_Alarm

func (f fakeAlarms) Alarms(context.Context) []*dcctlv1.Status_Alarm { return f }

type fakeVerifier struct {
	checks []*dcctlv1.Check
	err    error
}

func (f fakeVerifier) Verify(context.Context) ([]*dcctlv1.Check, error) { return f.checks, f.err }

type fakeOperations struct {
	view coord.OpView
	err  error
}

func (f fakeOperations) Read(context.Context) (coord.OpView, error) { return f.view, f.err }

type fakeJournal struct {
	resp *dcctlv1.ListJournalEntriesResponse
	err  error
}

func (f fakeJournal) List(context.Context, *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error) {
	return f.resp, f.err
}

// testOptions are Options with the example status and nothing else.
func testOptions() Options {
	return Options{
		Address:    observerAddress,
		Status:     fakeStatusSource{st: exampleStatus(), ok: true},
		Operations: fakeOperations{},
		Journal:    fakeJournal{resp: &dcctlv1.ListJournalEntriesResponse{}},
	}
}

func startAPI(t *testing.T, opts Options, extra ...grpc.StreamServerInterceptor) harness {
	t.Helper()

	logs := logtest.NewRecorder()
	log := logging.New(logging.WithProvider(logs))
	opts.Log = log

	v, err := protovalidate.New()
	require.NoError(t, err)

	rt, err := runtime.New(runtime.Config{
		HTTP:   "127.0.0.1:0",
		Peer:   "127.0.0.1:0",
		Health: "127.0.0.1:0",
		TLS:    testpki.New(t, "test ca").Files(t, "127.0.0.1"),
		HTTPOptions: []grpc.ServerOption{
			grpc.ChainUnaryInterceptor(validation.UnaryServerInterceptor(v)),
			grpc.ChainStreamInterceptor(append(extra, validation.StreamServerInterceptor(v))...),
		},
		HealthConfig: health.Config{
			Interval: 50 * time.Millisecond, Timeout: 40 * time.Millisecond, MinUnready: 50 * time.Millisecond,
			StaleAfter: time.Second, DrainHold: 10 * time.Millisecond,
		},
		StopTimeout: 2 * time.Second,
	}, log)
	require.NoError(t, err)
	require.NoError(t, Register(t.Context(), rt, opts))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- rt.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	select {
	case <-rt.Started():
	case err := <-done:
		require.FailNow(t, "runtime stopped", "err: %v", err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "runtime did not start")
	}

	addr := rt.HTTPAddr().String()

	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return harness{conn: conn, base: "http://" + addr, logs: logs}
}

func callCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

type httpResult struct {
	code   int
	header http.Header
	body   string
}

func (h harness) get(t *testing.T, path string, header ...string) httpResult {
	t.Helper()

	req, err := http.NewRequestWithContext(callCtx(t), http.MethodGet, h.base+path, nil)
	require.NoError(t, err)

	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return httpResult{code: resp.StatusCode, header: resp.Header, body: string(body)}
}

// logged reports whether some record of the harness holds text in its body
// or attributes.
func (h harness) logged(text string) bool {
	for _, r := range h.logs.Records() {
		if strings.Contains(r.Body().AsString(), text) {
			return true
		}

		for _, kv := range r.Attributes() {
			if strings.Contains(kv.Value.String(), text) {
				return true
			}
		}
	}

	return false
}

func assertCode(t *testing.T, want codes.Code, err error) *status.Status {
	t.Helper()

	st, ok := status.FromError(err)
	require.True(t, ok, "not a status error: %v", err)
	assert.Equal(t, want, st.Code(), st.Message())

	return st
}

// reasonOf returns the ErrorInfo reason of st, "" without one.
func reasonOf(st *status.Status) apierr.Reason {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == apierr.Domain {
			return apierr.Reason(info.GetReason())
		}
	}

	return ""
}

func testLogger() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}
