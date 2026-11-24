package consul

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/webitel/webitel-go-kit/infra/discovery"
	kit "github.com/webitel/webitel-go-kit/infra/discovery/consul"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// startConsul runs `consul agent -dev` in a container and returns its HTTP address.
// The test is skipped when Docker is not available.
func startConsul(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("integration test needs Docker")
	}

	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "hashicorp/consul:1.15",
			ExposedPorts: []string{"8500/tcp"},
			WaitingFor:   wait.ForHTTP("/v1/status/leader").WithPort("8500/tcp"),
			Cmd:          []string{"agent", "-dev", "-client", "0.0.0.0"},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "8500")
	require.NoError(t, err)

	return host + ":" + port.Port()
}

func integrationClients(t *testing.T, profile config.Profile, addr string) *Clients {
	t.Helper()

	c, err := New(Options{
		Profile:     profile,
		NodeMetaKey: "webitel_datacenter",
		Servers: []config.ConsulServer{
			{Address: refusedAddr(t), Datacenter: "dc_a"}, // the first server of the datacenter is down
			{Address: addr, Datacenter: "dc_a"},
			{Address: addr, Datacenter: "dc_b"},
		},
		SessionTTL: 10 * time.Second,
		LockDelay:  time.Second,
		Timeout:    5 * time.Second,
	})
	require.NoError(t, err)

	return c
}

func TestIntegrationKVCAS(t *testing.T) {
	ctx := t.Context()
	c := integrationClients(t, config.ProfileWarmStandby, startConsul(t)).mustClient(t, "dc_a")

	_, err := c.KVGet(ctx, "dcctl/state")
	require.ErrorIs(t, err, ErrKeyNotFound)

	ok, err := c.KVCAS(ctx, "dcctl/state", []byte("v1"), 0)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = c.KVCAS(ctx, "dcctl/state", []byte("again"), 0)
	require.NoError(t, err)
	assert.False(t, ok, "index 0 must not overwrite an existing key")

	e, err := c.KVGet(ctx, "dcctl/state")
	require.NoError(t, err)
	assert.Equal(t, []byte("v1"), e.Value)

	ok, err = c.KVCAS(ctx, "dcctl/state", []byte("v2"), e.ModifyIndex)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = c.KVCAS(ctx, "dcctl/state", []byte("v3"), e.ModifyIndex)
	require.NoError(t, err)
	assert.False(t, ok, "a stale ModifyIndex must lose")
}

func TestIntegrationSessionLock(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)
	c := integrationClients(t, config.ProfileWarmStandby, addr).mustClient(t, "dc_a")

	s1, err := c.CreateSession(ctx, "op-1")
	require.NoError(t, err)

	s2, err := c.CreateSession(ctx, "op-2")
	require.NoError(t, err)

	require.NoError(t, c.RenewSession(ctx, s1))

	ok, err := c.Acquire(ctx, "dcctl/lock", []byte("one"), s1)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = c.Acquire(ctx, "dcctl/lock", []byte("two"), s2)
	require.NoError(t, err)
	assert.False(t, ok, "a held lock must not be taken")

	ok, err = c.Release(ctx, "dcctl/lock", []byte("one"), s1)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Acquire(ctx, "dcctl/lock", []byte("two"), s2)
	require.NoError(t, err)
	assert.True(t, ok, "a released lock is free at once")

	// Invalidating the holder applies the lock delay to the key.
	require.NoError(t, c.DestroySession(ctx, s2))

	s3, err := c.CreateSession(ctx, "op-3")
	require.NoError(t, err)

	ok, err = c.Acquire(ctx, "dcctl/lock", []byte("three"), s3)
	require.NoError(t, err)
	assert.False(t, ok, "the lock delay holds the key after its session is invalidated")

	assert.Eventually(t, func() bool {
		got, err := c.Acquire(ctx, "dcctl/lock", []byte("three"), s3)

		return err == nil && got
	}, 5*time.Second, 200*time.Millisecond, "the key is free after the lock delay")

	require.NoError(t, c.DestroySession(ctx, s1))
	require.ErrorIs(t, c.RenewSession(ctx, s1), ErrSessionNotFound)

	raw, err := api.NewClient(&api.Config{Address: addr, Scheme: "http"})
	require.NoError(t, err)

	info, _, err := raw.Session().Info(s3, nil)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Empty(t, info.NodeChecks, "the session must not depend on serfHealth")
	assert.Equal(t, api.SessionBehaviorRelease, info.Behavior)
	assert.Equal(t, 10*time.Second, parseTTL(t, info.TTL))
}

func parseTTL(t *testing.T, s string) time.Duration {
	t.Helper()

	d, err := time.ParseDuration(s)
	require.NoError(t, err)

	return d
}

// registerNodes puts the same service on a node of each datacenter, as Ansible does
// with node_meta in stretch.
func registerNodes(t *testing.T, addr string) {
	t.Helper()

	raw, err := api.NewClient(&api.Config{Address: addr, Scheme: "http"})
	require.NoError(t, err)

	nodes := map[string]string{"dc_a": "10.1.0.11", "dc_b": "10.2.0.11"}
	for dc, ip := range nodes {
		_, err := raw.Catalog().Register(&api.CatalogRegistration{
			Node:     "node-" + dc,
			Address:  ip,
			NodeMeta: map[string]string{"webitel_datacenter": dc},
			Service:  &api.AgentService{ID: "api-" + dc, Service: "api", Port: 8080},
		}, nil)
		require.NoError(t, err)
	}
}

func TestIntegrationStretch(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)
	registerNodes(t, addr)

	t.Run("a_query_for_dc_b_returns_only_its_instances", func(t *testing.T) {
		c := integrationClients(t, config.ProfileStretch, addr)

		a, err := c.HealthyAddrs(ctx, "dc_a", "api", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.1.0.11:8080"}, a)

		b, err := c.HealthyAddrs(ctx, "dc_b", "api", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.2.0.11:8080"}, b)
	})

	t.Run("warm_standby_does_not_filter", func(t *testing.T) {
		c := integrationClients(t, config.ProfileWarmStandby, addr)

		all, err := c.HealthyAddrs(ctx, "dc_b", "api", "")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"10.1.0.11:8080", "10.2.0.11:8080"}, all)
	})

	t.Run("prepared_query_returns_node_meta_of_every_datacenter", func(t *testing.T) {
		raw, err := api.NewClient(&api.Config{Address: addr, Scheme: "http"})
		require.NoError(t, err)

		_, _, err = raw.PreparedQuery().Create(&api.PreparedQueryDefinition{
			Name:    "webitel-api",
			Service: api.ServiceQuery{Service: "api", OnlyPassing: true},
		}, nil)
		require.NoError(t, err)

		cl := integrationClients(t, config.ProfileStretch, addr).mustClient(t, "dc_b")

		nodes, err := cl.ExecuteQuery(ctx, "webitel-api")
		require.NoError(t, err)

		dcs := make([]string, 0, len(nodes))
		for _, n := range nodes {
			dcs = append(dcs, n.NodeMeta["webitel_datacenter"])
		}

		assert.ElementsMatch(t, []string{"dc_a", "dc_b"}, dcs)
	})
}

type testLogger struct{ t *testing.T }

func (l testLogger) Info(msg string, args ...any)  { l.t.Log("info:", msg, args) }
func (l testLogger) Warn(msg string, args ...any)  { l.t.Log("warn:", msg, args) }
func (l testLogger) Error(msg string, args ...any) { l.t.Log("error:", msg, args) }

// The runtime registers webitel-dcctl through the go-kit registry with a TTL check
// that follows its readiness.
func TestIntegrationRegistrationWithTTLCheck(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)

	var ready atomic.Bool

	ready.Store(true)

	reg, err := kit.NewConsulRegistry(addr, testLogger{t})
	require.NoError(t, err)

	for _, opt := range []discovery.Option[discovery.DiscoveryProvider]{
		discovery.WithHealthCheck[discovery.DiscoveryProvider](false),
		discovery.WithHeartbeat[discovery.DiscoveryProvider](true),
		discovery.WithHealthCheckInterval[discovery.DiscoveryProvider](1),
		kit.WithReadiness(func() (bool, error) { return ready.Load(), nil }),
	} {
		opt(reg)
	}

	svc := &discovery.ServiceInstance{
		Id:        "webitel-dcctl-1",
		Name:      "webitel-dcctl",
		Endpoints: []string{"https://10.1.0.5:10023"},
	}
	require.NoError(t, reg.Register(ctx, svc))
	t.Cleanup(func() { _ = reg.Deregister(context.Background(), svc) })

	c := integrationClients(t, config.ProfileWarmStandby, addr)

	assert.Eventually(t, func() bool {
		addrs, err := c.HealthyAddrs(ctx, "dc_a", "webitel-dcctl", "")

		return err == nil && len(addrs) == 1 && addrs[0] == "10.1.0.5:10023"
	}, 15*time.Second, 200*time.Millisecond, "passing while ready")

	ready.Store(false)

	assert.Eventually(t, func() bool {
		addrs, err := c.HealthyAddrs(ctx, "dc_a", "webitel-dcctl", "")

		return err == nil && len(addrs) == 0
	}, 15*time.Second, 200*time.Millisecond, "critical when not ready")
}
