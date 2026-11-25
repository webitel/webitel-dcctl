package arbiter_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/infra/health"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/arbiter/store"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

// allowedIP is the SAN IP of a client in the clients of a test arbiter.
const allowedIP = "10.1.0.81"

var testHealth = health.Config{
	Interval:   50 * time.Millisecond,
	Timeout:    40 * time.Millisecond,
	MinUnready: 50 * time.Millisecond,
	StaleAfter: time.Second,
	DrainHold:  10 * time.Millisecond,
}

// testArbiter is a running arbiter daemon.
type testArbiter struct {
	cfg *config.Arbiter
	ca  *testpki.CA
	// stop cancels the daemon and returns the error of Run.
	stop func() error
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

// startArbiter runs an arbiter that admits allowedIP until the test ends.
func startArbiter(t *testing.T) *testArbiter {
	t.Helper()

	ca := testpki.New(t, "test ca")
	cfg := &config.Arbiter{
		Listen:  config.ArbiterListen{API: freeAddr(t), Health: freeAddr(t)},
		DataDir: t.TempDir(),
		TLS:     ca.Files(t, "127.0.0.1"),
		Clients: []string{allowedIP},
	}

	return &testArbiter{cfg: cfg, ca: ca, stop: runArbiter(t, cfg)}
}

// restart stops a and runs it again with the same config and store.
func (a *testArbiter) restart(t *testing.T) {
	t.Helper()

	require.NoError(t, a.stop())

	a.stop = runArbiter(t, a.cfg)
}

// runArbiter runs the arbiter of cfg until it is ready, and returns the
// func that stops it and returns the error of Run. The end of the test
// stops it too.
func runArbiter(t *testing.T, cfg *config.Arbiter) (stop func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})

	var runErr error

	go func() {
		defer close(finished)

		runErr = arbiter.RunWithHealth(ctx, cfg, testHealth, testLogger())
	}()

	stop = sync.OnceValue(func() error {
		cancel()
		<-finished

		return runErr
	})

	t.Cleanup(func() { _ = stop() })

	deadline := time.After(5 * time.Second)

	for {
		if code, _, err := healthGet(t, cfg.Listen.Health+"/readyz"); err == nil && code == http.StatusOK {
			return stop
		}

		select {
		case <-finished:
			require.FailNow(t, "Run returned before start", "err: %v", runErr)
		case <-deadline:
			require.FailNow(t, "arbiter is not ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// healthGet returns the status code and body of GET http://addrPath.
func healthGet(t *testing.T, addrPath string) (int, []byte, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addrPath, nil)
	require.NoError(t, err)

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	return resp.StatusCode, body, err
}

// clientTLS trusts the arbiter's CA and presents certs.
func (a *testArbiter) clientTLS(certs ...tls.Certificate) *tls.Config {
	return &tls.Config{RootCAs: a.ca.Pool, Certificates: certs, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}
}

func (a *testArbiter) dial(t *testing.T, cfg *tls.Config) dcctlv1.ArbiterServiceClient {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///"+a.cfg.Listen.API, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return dcctlv1.NewArbiterServiceClient(conn)
}

func callCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func TestRunAdmitsListedClient(t *testing.T) {
	a := startArbiter(t)
	client := a.dial(t, a.clientTLS(a.ca.Leaf(t, allowedIP)))

	_, err := client.GetRecord(callCtx(t), &dcctlv1.GetRecordRequest{Name: "records/state"})
	requireReason(t, err, codes.FailedPrecondition, "ARBITER_UNINITIALIZED")

	state, err := client.UpdateRecord(callCtx(t), &dcctlv1.UpdateRecordRequest{
		Record:       &dcctlv1.Record{Name: "records/state", Value: stateJSON},
		AllowMissing: true,
	})
	require.NoError(t, err)

	got, err := client.GetRecord(callCtx(t), &dcctlv1.GetRecordRequest{Name: "records/state"})
	require.NoError(t, err)
	assert.Equal(t, state.GetEtag(), got.GetEtag())
}

func TestRunValidatesRequests(t *testing.T) {
	a := startArbiter(t)
	client := a.dial(t, a.clientTLS(a.ca.Leaf(t, allowedIP)))

	_, err := client.GetRecord(callCtx(t), &dcctlv1.GetRecordRequest{Name: "records/journal"})
	requireReason(t, err, codes.InvalidArgument, "REQUEST_INVALID")

	for _, tt := range []struct {
		name         string
		etag         string
		allowMissing bool
	}{
		{"leading_zero", "07", false},
		{"etag_with_allow_missing", "7", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.UpdateRecord(callCtx(t), &dcctlv1.UpdateRecordRequest{
				Record:       &dcctlv1.Record{Name: "records/state", Value: stateJSON, Etag: tt.etag},
				AllowMissing: tt.allowMissing,
			})
			requireReason(t, err, codes.InvalidArgument, "REQUEST_INVALID")
		})
	}
}

func TestRunRejectsClients(t *testing.T) {
	a := startArbiter(t)
	foreign := testpki.New(t, "foreign ca")

	tests := []struct {
		name  string
		certs []tls.Certificate
		want  codes.Code
	}{
		{"signed_not_listed", []tls.Certificate{a.ca.Leaf(t, "10.1.0.99")}, codes.PermissionDenied},
		{"signed_without_ip", []tls.Certificate{a.ca.Leaf(t)}, codes.PermissionDenied},
		{"no_certificate", nil, codes.Unavailable},
		{"foreign_ca", []tls.Certificate{foreign.Leaf(t, allowedIP)}, codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := a.dial(t, a.clientTLS(tt.certs...))

			_, err := client.GetRecord(callCtx(t), &dcctlv1.GetRecordRequest{Name: "records/state"})
			assert.Equal(t, tt.want, status.Code(err), "GetRecord() error = %v", err)
		})
	}
}

func TestRunHealth(t *testing.T) {
	a := startArbiter(t)

	_, body, err := healthGet(t, a.cfg.Listen.Health+"/healthz")
	require.NoError(t, err)

	var got struct {
		Checks []struct {
			System string `json:"system"`
			Name   string `json:"name"`
			Group  string `json:"group"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(body, &got))

	checks := make(map[string]string, len(got.Checks))
	for _, c := range got.Checks {
		checks[c.System+"/"+c.Name] = c.Group + ":" + c.Status
	}

	assert.Equal(t, map[string]string{"http/arbiter": "critical:ok", "dcctl/store": "critical:ok"}, checks)
}

func TestRunStopsAndClosesStore(t *testing.T) {
	a := startArbiter(t)

	require.NoError(t, a.stop())

	st, err := store.Open(a.cfg.DataDir)
	require.NoError(t, err, "the store must be released on stop")
	require.NoError(t, st.Close())
}

func TestRunStoreLocked(t *testing.T) {
	ca := testpki.New(t, "test ca")
	cfg := &config.Arbiter{
		Listen:  config.ArbiterListen{API: freeAddr(t), Health: freeAddr(t)},
		DataDir: t.TempDir(),
		TLS:     ca.Files(t, "127.0.0.1"),
		Clients: []string{allowedIP},
	}

	openStore(t, cfg.DataDir)

	err := arbiter.RunWithHealth(t.Context(), cfg, testHealth, testLogger())
	assert.ErrorIs(t, err, store.ErrLocked)
}
