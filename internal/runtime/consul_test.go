package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAgent is the part of the Consul agent HTTP API the runtime uses. It
// records registrations, TTL updates and deregistrations. With hangSelfFrom
// set, /v1/agent/self hangs from that call on until the test ends.
type fakeAgent struct {
	meta         map[string]string
	hangSelfFrom int
	release      chan struct{}

	mu           sync.Mutex
	selfCalls    int
	registered   []api.AgentServiceRegistration
	ttlUpdates   []string
	deregistered []string
}

func newFakeAgent(t *testing.T, meta map[string]string) (*fakeAgent, *httptest.Server) {
	t.Helper()

	return newHangingAgent(t, meta, 0)
}

func newHangingAgent(t *testing.T, meta map[string]string, hangSelfFrom int) (*fakeAgent, *httptest.Server) {
	t.Helper()

	a := &fakeAgent{meta: meta, hangSelfFrom: hangSelfFrom, release: make(chan struct{})}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(a.release) })

	return a, srv
}

func (a *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.hangs(r) {
		select {
		case <-a.release:
		case <-r.Context().Done():
		}

		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case r.URL.Path == "/v1/agent/self":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Config": map[string]any{"NodeName": "node-a1", "Datacenter": "dc1"},
			"Meta":   a.meta,
		})
	case r.URL.Path == "/v1/agent/service/register":
		var reg api.AgentServiceRegistration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		a.registered = append(a.registered, reg)
	case strings.HasPrefix(r.URL.Path, "/v1/agent/check/update/"):
		body, _ := io.ReadAll(r.Body)
		a.ttlUpdates = append(a.ttlUpdates, string(body))
	case strings.HasPrefix(r.URL.Path, "/v1/agent/service/deregister/"):
		a.deregistered = append(a.deregistered, strings.TrimPrefix(r.URL.Path, "/v1/agent/service/deregister/"))
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeAgent) hangs(r *http.Request) bool {
	if r.URL.Path != "/v1/agent/self" {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.selfCalls++

	return a.hangSelfFrom > 0 && a.selfCalls >= a.hangSelfFrom
}

func (a *fakeAgent) snapshot() (registered []api.AgentServiceRegistration, ttlUpdates, deregistered []string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append(registered, a.registered...), append(ttlUpdates, a.ttlUpdates...), append(deregistered, a.deregistered...)
}

func testConsul(srv *httptest.Server) Consul {
	return Consul{Address: srv.Listener.Addr().String(), Datacenter: "dc_a", NodeMetaKey: "webitel_datacenter"}
}

func TestCheckDatacenter(t *testing.T) {
	_, srv := newFakeAgent(t, map[string]string{"webitel_datacenter": "dc_a"})

	require.NoError(t, testConsul(srv).checkDatacenter(t.Context()))
}

func TestCheckDatacenterMismatch(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"other_datacenter", map[string]string{"webitel_datacenter": "dc_b"}, `node_meta webitel_datacenter="dc_b" of the local Consul agent, but datacenter is "dc_a"`},
		{"no_key", map[string]string{"other": "dc_a"}, `no node_meta webitel_datacenter in the local Consul agent; datacenter is "dc_a"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, srv := newFakeAgent(t, tt.meta)

			require.ErrorContains(t, testConsul(srv).checkDatacenter(t.Context()), tt.want)
		})
	}
}

func TestNodeName(t *testing.T) {
	_, srv := newFakeAgent(t, map[string]string{"webitel_datacenter": "dc_a"})

	name, err := testConsul(srv).NodeName(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "node-a1", name)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	_, err = Consul{Address: closed.Listener.Addr().String()}.NodeName(t.Context())
	require.ErrorContains(t, err, "read the local Consul agent at "+closed.Listener.Addr().String())
}

func TestCheckDatacenterAgentErrors(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	tests := []struct {
		name string
		addr string
	}{
		{"agent_error", failing.Listener.Addr().String()},
		{"agent_unreachable", closed.Listener.Addr().String()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Consul{Address: tt.addr, Datacenter: "dc_a", NodeMetaKey: "webitel_datacenter"}

			err := c.checkDatacenter(t.Context())
			require.ErrorContains(t, err, "read node_meta of the local Consul agent at "+tt.addr)
		})
	}
}

func TestRunRegistersInConsul(t *testing.T) {
	agent, srv := newFakeAgent(t, map[string]string{"webitel_datacenter": "dc_a"})

	cfg := testConfig(newTestPKI(t))
	c := testConsul(srv)
	cfg.Consul = &c

	rt := newTestRuntime(t, cfg, &fakeStatus{})
	stop := start(t, rt)

	_, peerPort, err := net.SplitHostPort(rt.peerLn.Addr().String())
	require.NoError(t, err)

	registered, _, _ := agent.snapshot()
	require.Len(t, registered, 1)

	reg := registered[0]
	assert.Equal(t, "webitel-dcctl", reg.Name)
	assert.Equal(t, "127.0.0.1", reg.Address)
	assert.Equal(t, peerPort, strconv.Itoa(reg.Port))
	require.Len(t, reg.Checks, 1)
	assert.NotEmpty(t, reg.Checks[0].TTL)
	assert.Empty(t, reg.Checks[0].TCP)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ttlUpdates, _ := agent.snapshot()
		assert.NotEmpty(c, ttlUpdates)
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, stop())

	_, _, deregistered := agent.snapshot()
	assert.Contains(t, deregistered, reg.ID)
}

func TestRunRefusesForeignDatacenter(t *testing.T) {
	agent, srv := newFakeAgent(t, map[string]string{"webitel_datacenter": "dc_b"})

	cfg := testConfig(newTestPKI(t))
	c := testConsul(srv)
	cfg.Consul = &c

	rt := newTestRuntime(t, cfg, &fakeStatus{})

	err := rt.Run(t.Context())
	require.ErrorContains(t, err, `node_meta webitel_datacenter="dc_b"`)
	assert.Nil(t, rt.peerLn)

	registered, _, _ := agent.snapshot()
	assert.Empty(t, registered)
}

// The first agent/self call is the datacenter check, the second one comes from
// the go-kit registry constructor.
func TestRunStopsWhileAgentHangs(t *testing.T) {
	tests := []struct {
		name         string
		hangSelfFrom int
	}{
		{"datacenter_check", 1},
		{"registry", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, srv := newHangingAgent(t, map[string]string{"webitel_datacenter": "dc_a"}, tt.hangSelfFrom)

			cfg := testConfig(newTestPKI(t))
			c := testConsul(srv)
			cfg.Consul = &c

			rt := newTestRuntime(t, cfg, &fakeStatus{})

			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()

			began := time.Now()

			require.ErrorIs(t, rt.Run(ctx), context.DeadlineExceeded)
			assert.Less(t, time.Since(began), 2*time.Second)
		})
	}
}

func TestConsulAddressFromEnv(t *testing.T) {
	t.Setenv("CONSUL_HTTP_ADDR", "10.0.0.1:8500")
	assert.Equal(t, "10.0.0.1:8500", Consul{}.apiConfig().Address)

	t.Setenv("CONSUL_HTTP_ADDR", "")
	assert.Equal(t, "127.0.0.1:8500", Consul{}.apiConfig().Address)

	assert.Equal(t, "127.0.0.1:1", Consul{Address: "127.0.0.1:1"}.apiConfig().Address)
}
