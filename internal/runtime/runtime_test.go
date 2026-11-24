package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/infra/health"
	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func testLogger() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

func testConfig(p testPKI) Config {
	return Config{
		HTTP:   "127.0.0.1:0",
		Peer:   "127.0.0.1:0",
		Health: "127.0.0.1:0",
		TLS:    p.files,
		HealthConfig: health.Config{
			Interval:   50 * time.Millisecond,
			Timeout:    40 * time.Millisecond,
			MinUnready: 50 * time.Millisecond,
			StaleAfter: time.Second,
			DrainHold:  10 * time.Millisecond,
		},
		StopTimeout: 2 * time.Second,
	}
}

// newTestRuntime builds a runtime with the fake StatusService and
// OperationService on gRPC and the gateway.
func newTestRuntime(t *testing.T, cfg Config, st *fakeStatus) *Runtime {
	t.Helper()

	rt, err := New(cfg, testLogger())
	require.NoError(t, err)

	rt.RegisterService(&dcctlv1.StatusService_ServiceDesc, st)
	rt.RegisterService(&dcctlv1.OperationService_ServiceDesc, fakeOperations{})
	require.NoError(t, rt.RegisterGateway(t.Context(), dcctlv1.RegisterStatusServiceHandler))
	require.NoError(t, rt.RegisterGateway(t.Context(), dcctlv1.RegisterOperationServiceHandler))

	return rt
}

// start runs rt until the returned stop is called or the test ends; stop
// returns the error of Run.
func start(t *testing.T, rt *Runtime) (stop func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- rt.Run(ctx) }()

	select {
	case <-rt.started:
	case err := <-done:
		cancel()
		require.FailNow(t, "Run returned before start", "err: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		require.FailNow(t, "runtime did not start")
	}

	stop = sync.OnceValue(func() error {
		cancel()

		return <-done
	})

	t.Cleanup(func() { _ = stop() })

	return stop
}

func grpcClient(t *testing.T, addr string, creds credentials.TransportCredentials) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(creds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// httpResult is a response read to the end.
type httpResult struct {
	code   int
	header http.Header
	body   []byte
}

func httpGet(t *testing.T, url string) httpResult {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return httpResult{code: resp.StatusCode, header: resp.Header, body: body}
}

func callCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func TestListenHTTPServesGRPCAndGateway(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	start(t, rt)

	conn := grpcClient(t, rt.httpLn.Addr().String(), insecure.NewCredentials())
	got, err := dcctlv1.NewStatusServiceClient(conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, "datacenters/dc_a", got.GetDetectedActiveDatacenter())

	resp := httpGet(t, "http://"+rt.httpLn.Addr().String()+"/api/v1/status")
	assert.Equal(t, http.StatusOK, resp.code)
	assert.JSONEq(t, `{"name":"status","detected_active_datacenter":"datacenters/dc_a"}`, string(resp.body))
}

func TestGatewayError(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	start(t, rt)

	resp := httpGet(t, "http://"+rt.httpLn.Addr().String()+"/api/v1/status:verify")

	assert.Equal(t, http.StatusBadRequest, resp.code)
	assert.Empty(t, resp.header.Values("Www-Authenticate"))
	assert.JSONEq(t, `{
		"id": "dcctl.preflight.failed",
		"code": 400,
		"status": "Bad Request",
		"detail": "preflight failed",
		"violations": [{"type": "check", "subject": "coord.lock_free", "description": "dcctl/lock is held"}],
		"preflight": {"checks": [{"id": "coord.lock_free", "result": "FAIL"}]}
	}`, string(resp.body))
}

func TestGatewayStreamError(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	start(t, rt)

	resp := httpGet(t, "http://"+rt.httpLn.Addr().String()+"/api/v1/operations/op1:watch")
	assert.Equal(t, http.StatusOK, resp.code)

	var lines []string
	for sc := bufio.NewScanner(strings.NewReader(string(resp.body))); sc.Scan(); {
		lines = append(lines, sc.Text())
	}

	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"result":{"name":"operations/op1"}}`, lines[0])
	assert.JSONEq(t, `{"error":{
		"id": "dcctl.operation.not_found",
		"code": 404,
		"status": "Not Found",
		"detail": "operation not found",
		"metadata": {"operation": "operations/op1"}
	}}`, lines[1])
}

func TestGatewayStreamErrorBeforeFirstMessage(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	start(t, rt)

	resp := httpGet(t, "http://"+rt.httpLn.Addr().String()+"/api/v1/"+missingOperation+":watch")

	assert.Equal(t, http.StatusNotFound, resp.code)
	assert.JSONEq(t, `{"error":{
		"id": "dcctl.operation.not_found",
		"code": 404,
		"status": "Not Found",
		"detail": "operation not found",
		"metadata": {"operation": "operations/missing"}
	}}`, string(resp.body))
}

func TestHTTPRouting(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	rt.HandleUI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ui")
	}))
	start(t, rt)

	base := "http://" + rt.httpLn.Addr().String()

	resp := httpGet(t, base+"/api/v1/nope")
	assert.Equal(t, http.StatusNotFound, resp.code)
	assert.JSONEq(t, `{"code":404,"status":"Not Found","detail":"Not Found"}`, string(resp.body))

	resp = httpGet(t, base+"/datacenters/dc_a")
	assert.Equal(t, http.StatusOK, resp.code)
	assert.Equal(t, "ui", string(resp.body))
}

func TestHTTPWithoutUI(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	start(t, rt)

	resp := httpGet(t, "http://"+rt.httpLn.Addr().String()+"/")
	assert.Equal(t, http.StatusNotFound, resp.code)
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return addr
}

func TestRunHTTPPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })

	cfg := testConfig(newTestPKI(t))
	cfg.HTTP = busy.Addr().String()
	cfg.Peer = freeAddr(t)

	rt := newTestRuntime(t, cfg, &fakeStatus{})

	require.ErrorContains(t, rt.Run(t.Context()), "listen.http: ")

	peer, err := net.Listen("tcp", cfg.Peer)
	require.NoError(t, err, "listen.peer must be released")
	require.NoError(t, peer.Close())
}

func TestPeerRequiresClientCert(t *testing.T) {
	p := newTestPKI(t)
	rt := newTestRuntime(t, testConfig(p), &fakeStatus{})
	start(t, rt)

	tests := []struct {
		name string
		cert clientCert
		want codes.Code
	}{
		{"no_certificate", certNone, codes.Unavailable},
		{"foreign_ca", certForeign, codes.Unavailable},
		{"signed_by_ca", certSigned, codes.OK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds := credentials.NewTLS(p.clientTLS(t, tt.cert))
			conn := grpcClient(t, rt.peerLn.Addr().String(), creds)

			_, err := dcctlv1.NewStatusServiceClient(conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
			assert.Equal(t, tt.want, status.Code(err), "GetStatus() error = %v", err)
		})
	}
}

func TestHealthEndpoints(t *testing.T) {
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	start(t, rt)

	base := "http://" + rt.healthSrv.Addr()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		resp := httpGet(t, base+"/readyz")
		assert.Equal(c, http.StatusOK, resp.code)
	}, 5*time.Second, 20*time.Millisecond)

	resp := httpGet(t, base+"/livez")
	assert.Equal(t, http.StatusOK, resp.code)

	resp = httpGet(t, base+"/healthz")
	assert.Equal(t, http.StatusOK, resp.code)

	var got struct {
		Status string `json:"status"`
		Checks []struct {
			System string `json:"system"`
			Name   string `json:"name"`
			Group  string `json:"group"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(resp.body, &got))

	assert.Equal(t, "ready", got.Status)

	keys := make(map[string]string, len(got.Checks))
	for _, c := range got.Checks {
		keys[c.System+"/"+c.Name] = c.Group + ":" + c.Status
	}

	assert.Equal(t, map[string]string{"http/api": "critical:ok", "http/peer": "critical:ok"}, keys)
}

// notifySocket listens on a unixgram socket with a path short enough for the
// 104 bytes of sun_path on darwin, where t.TempDir() is already too long.
func notifySocket(t *testing.T) (string, *net.UnixConn) {
	t.Helper()

	base := os.TempDir()
	if len(base) > 40 {
		base = "/tmp"
	}

	dir, err := os.MkdirTemp(base, "sd") //nolint:usetesting // t.TempDir() is too long for sun_path on darwin
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	addr := filepath.Join(dir, "n.sock")

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return addr, conn
}

// readNotify collects datagrams until want are all seen or the deadline passes.
func readNotify(t *testing.T, conn *net.UnixConn, want ...string) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	missing := make(map[string]bool, len(want))
	for _, w := range want {
		missing[w] = true
	}

	buf := make([]byte, 4096)
	for len(missing) > 0 {
		n, err := conn.Read(buf)
		require.NoError(t, err, "still waiting for %v", missing)

		for line := range strings.SplitSeq(string(buf[:n]), "\n") {
			delete(missing, line)
		}
	}
}

func TestSDNotify(t *testing.T) {
	addr, conn := notifySocket(t)
	t.Setenv("NOTIFY_SOCKET", addr)
	t.Setenv("WATCHDOG_USEC", "200000")
	t.Setenv("WATCHDOG_PID", "")

	rt := newTestRuntime(t, testConfig(newTestPKI(t)), &fakeStatus{})
	stop := start(t, rt)

	readNotify(t, conn, "READY=1", "STATUS=ready", "WATCHDOG=1")

	require.NoError(t, stop())

	readNotify(t, conn, "STOPPING=1")
}

func TestGracefulShutdown(t *testing.T) {
	st := &fakeStatus{entered: make(chan struct{}), release: make(chan struct{})}
	rt := newTestRuntime(t, testConfig(newTestPKI(t)), st)
	stop := start(t, rt)

	httpAddr := rt.httpLn.Addr().String()
	conn := grpcClient(t, httpAddr, insecure.NewCredentials())

	callErr := make(chan error, 1)

	go func() {
		_, err := dcctlv1.NewStatusServiceClient(conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
		callErr <- err
	}()

	<-st.entered

	stopErr := make(chan error, 1)

	go func() { stopErr <- stop() }()

	// The server is shutting down once it refuses new connections; the call
	// is still running.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		dialed, err := net.DialTimeout("tcp", httpAddr, time.Second)
		if err == nil {
			_ = dialed.Close()
		}

		assert.Error(c, err)
	}, 5*time.Second, 10*time.Millisecond)

	close(st.release)

	require.NoError(t, <-callErr)
	require.NoError(t, <-stopErr)
}

func TestStopTimeoutCutsLongCalls(t *testing.T) {
	st := &fakeStatus{entered: make(chan struct{}), release: make(chan struct{})}
	cfg := testConfig(newTestPKI(t))
	cfg.StopTimeout = 100 * time.Millisecond

	rt := newTestRuntime(t, cfg, st)
	stop := start(t, rt)

	conn := grpcClient(t, rt.httpLn.Addr().String(), insecure.NewCredentials())

	callErr := make(chan error, 1)

	go func() {
		_, err := dcctlv1.NewStatusServiceClient(conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
		callErr <- err
	}()

	<-st.entered

	began := time.Now()

	require.NoError(t, stop())
	assert.Less(t, time.Since(began), 2*time.Second)

	close(st.release)
	assert.Error(t, <-callErr)
}
