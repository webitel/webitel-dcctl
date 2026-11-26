package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/infra/health"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/runtime"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

// peerSet is the webitel-dcctl instances registered in Consul, as the peer
// listener admits them.
type peerSet struct {
	mu    sync.Mutex
	addrs []string
}

func (p *peerSet) Datacenters() []string { return []string{"dc_a"} }

func (p *peerSet) RegisteredAddrs(context.Context, string, string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.addrs...), nil
}

func (p *peerSet) add(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.addrs = append(p.addrs, addr)
}

// countingForwarder counts the calls it forwards.
type countingForwarder struct {
	Forwarder

	mu    sync.Mutex
	calls int
}

func (f *countingForwarder) Forward(ctx context.Context, address, method string, req, resp proto.Message) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	return f.Forwarder.Forward(ctx, address, method, req, resp)
}

func (f *countingForwarder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// instance is a dcctl instance with the interceptors of serve: basic auth on
// listen.http, the instance check on listen.peer.
type instance struct {
	env      *actionEnv
	conn     *grpc.ClientConn
	base     string
	peerAddr string
	forwards *countingForwarder
}

// startInstance runs self over env. peers are the instances its peer
// listener admits; it adds itself.
func startInstance(t *testing.T, ca *testpki.CA, self operation.Instance, env *actionEnv, peers *peerSet) *instance {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(alicePassword), bcrypt.MinCost)
	require.NoError(t, err)

	users := []config.User{{Name: aliceUser, PasswordHash: string(hash), Role: config.RoleOperator}}

	httpOpts, peerOpts, err := auth.ServerOptions(t.Context(), users, peers, testLogger())
	require.NoError(t, err)

	files := ca.Files(t, "127.0.0.1")

	rt, err := runtime.New(runtime.Config{
		HTTP: "127.0.0.1:0", Peer: "127.0.0.1:0", Health: "127.0.0.1:0", TLS: files,
		HTTPOptions: httpOpts, PeerOptions: peerOpts, APIGuard: auth.CheckAPIRequest,
		HealthConfig: health.Config{
			Interval: 50 * time.Millisecond, Timeout: 40 * time.Millisecond, MinUnready: 50 * time.Millisecond,
			StaleAfter: time.Second, DrainHold: 10 * time.Millisecond,
		},
		StopTimeout: 2 * time.Second,
	}, testLogger())
	require.NoError(t, err)

	client, err := NewPeerClient(files, 5*time.Second, time.Second)
	require.NoError(t, err)

	forwards := &countingForwarder{Forwarder: client}

	acts := env.actions()
	acts.Self, acts.Forwarder = self, forwards
	acts.Datacenters = []string{"dc_a", "dc_b", "dc_c"}

	opts := testOptions()
	opts.Status, opts.Operations, opts.Actions, opts.Log = env.status, env.ops, acts, testLogger()
	require.NoError(t, Register(t.Context(), rt, opts))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- rt.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
		assert.NoError(t, client.Close())
	})

	select {
	case <-rt.Started():
	case err := <-done:
		require.FailNow(t, "runtime stopped", "err: %v", err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "runtime did not start")
	}

	peers.add(rt.PeerAddr().String())

	conn, err := grpc.NewClient("passthrough:///"+rt.HTTPAddr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &instance{
		env: env, conn: conn, base: "http://" + rt.HTTPAddr().String(), peerAddr: rt.PeerAddr().String(), forwards: forwards,
	}
}

func (i *instance) datacenters() dcctlv1.DatacenterServiceClient {
	return dcctlv1.NewDatacenterServiceClient(i.conn)
}

// aliceCall returns the context of a gRPC call of alice.
func aliceCall(t *testing.T) context.Context {
	t.Helper()

	return metadata.AppendToOutgoingContext(callCtx(t), "authorization", basicHeader())
}

// post sends body to path of the gateway as alice, with the header pairs.
func (i *instance) post(t *testing.T, path, body string, header ...string) httpResult {
	t.Helper()

	req, err := http.NewRequestWithContext(callCtx(t), http.MethodPost, i.base+path, strings.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("Authorization", basicHeader())
	req.Header.Set("Content-Type", "application/json")

	for j := 0; j+1 < len(header); j += 2 {
		req.Header.Set(header[j], header[j+1])
	}

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return httpResult{code: resp.StatusCode, header: resp.Header, body: string(raw)}
}

// closedAddr returns a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return addr
}
