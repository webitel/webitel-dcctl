package collect

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

func serveConfig(t *testing.T, profile config.Profile) *config.Serve {
	t.Helper()

	return &config.Serve{
		Profile:    profile,
		Datacenter: "dc_a",
		TLS:        testpki.New(t, "test CA").Files(t, "127.0.0.1"),
		Consul: config.Consul{
			NodeMetaKey: "webitel_datacenter",
			Servers: []config.ConsulServer{
				{Address: "10.1.0.1:8500", Datacenter: "dc_a"},
				{Address: "10.2.0.1:8500", Datacenter: "dc_b"},
			},
		},
		Patroni:      config.Patroni{Scope: "pg"},
		Coordination: config.Coordination{SessionTTL: 15 * time.Second},
		Status:       config.Status{Interval: 5 * time.Second, RequestTimeout: 3 * time.Second},
	}
}

func testLogger() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

func TestFromConfig(t *testing.T) {
	instance := Instance{Datacenter: "dc_a", Node: "dcctl-a1"}

	t.Run("warm_standby", func(t *testing.T) {
		cfg := serveConfig(t, config.ProfileWarmStandby)

		c, err := FromConfig(cfg, instance, nil, ReadsStale, testLogger())
		require.NoError(t, err)

		assert.Equal(t, instance, c.cfg.Instance)
		assert.Equal(t, "pg", c.cfg.Scope)
		assert.Equal(t, "webitel_datacenter", c.cfg.NodeMetaKey)
		assert.Nil(t, c.src.Arbiter, "no arbiter is configured")
		require.Len(t, c.src.Voters, 2)
		require.Len(t, c.src.Datacenters, 2)
		require.Len(t, c.src.Clusters, 2, "a Patroni cluster per datacenter")
		assert.Equal(t, []string{"dc_a"}, c.src.Clusters[0].Datacenters)
		assert.Equal(t, []string{"dc_b"}, c.src.Clusters[1].Datacenters)
	})

	t.Run("stretch", func(t *testing.T) {
		cfg := serveConfig(t, config.ProfileStretch)

		c, err := FromConfig(cfg, instance, nil, ReadsStale, testLogger())
		require.NoError(t, err)

		require.Len(t, c.src.Voters, 1)
		require.Len(t, c.src.Datacenters, 2)
		require.Len(t, c.src.Clusters, 1, "one Patroni cluster over all datacenters")
		assert.Equal(t, []string{"dc_a", "dc_b"}, c.src.Clusters[0].Datacenters)
	})

	t.Run("arbiter", func(t *testing.T) {
		cfg := serveConfig(t, config.ProfileWarmStandby)

		arb, err := arbiter.NewClient(arbiter.ClientOptions{Address: "127.0.0.1:1", TLS: cfg.TLS, Timeout: time.Second})
		require.NoError(t, err)
		t.Cleanup(func() { _ = arb.Close() })

		c, err := FromConfig(cfg, instance, arb, ReadsStale, testLogger())
		require.NoError(t, err)
		assert.Same(t, arb, c.src.Arbiter)
	})

	t.Run("bad_ca", func(t *testing.T) {
		cfg := serveConfig(t, config.ProfileWarmStandby)
		cfg.TLS.CA = t.TempDir() + "/missing.pem"

		_, err := FromConfig(cfg, instance, nil, ReadsStale, testLogger())
		require.Error(t, err)
	})
}

// A server that accepts connections and never answers must not take the whole wait for
// the source: the next server is still asked within the request timeout. Every read of
// the collection may be answered by a server without a leader.
func TestFromConfig_deadFirstConsulServer(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = dead.Close() })

	var (
		mu         sync.Mutex
		consistent []string
	)

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if !r.URL.Query().Has("stale") {
			consistent = append(consistent, r.URL.Path)
		}
		mu.Unlock()

		w.Header().Set("Connection", "close")
		w.Header().Set("X-Consul-Knownleader", "false")
		w.Header().Set("X-Consul-Lastcontact", "0")

		switch {
		case r.URL.Path == "/v1/kv/"+kv.StateKey:
			_, _ = io.WriteString(w, `[{"Key":"dcctl/state","Value":"e30=","ModifyIndex":7}]`)
		case strings.HasPrefix(r.URL.Path, "/v1/kv/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	t.Cleanup(live.Close)

	liveAddr := strings.TrimPrefix(live.URL, "http://")

	cfg := serveConfig(t, config.ProfileWarmStandby)
	cfg.Consul.Servers = []config.ConsulServer{
		{Address: dead.Addr().String(), Datacenter: "dc_a"},
		{Address: liveAddr, Datacenter: "dc_a"},
		{Address: liveAddr, Datacenter: "dc_b"},
	}
	cfg.Status.RequestTimeout = 900 * time.Millisecond

	c, err := FromConfig(cfg, Instance{}, nil, ReadsStale, testLogger())
	require.NoError(t, err)

	s, ok := c.cycle(t.Context(), nil)
	require.True(t, ok)

	v := s.Voters[0].KV
	require.True(t, v.Reachable, "%v", v.Err)
	require.NotNil(t, v.Data.State)
	assert.Equal(t, uint64(7), v.Data.State.ModifyIndex)
	assert.False(t, v.Data.Meta.KnownLeader)
	assert.True(t, s.Datacenters[0].Services.Reachable, "%v", s.Datacenters[0].Services.Err)

	mu.Lock()
	defer mu.Unlock()

	assert.Empty(t, consistent, "collection reads are stale")
}

// A collector an action is decided on reads from the Consul leaders: a server cut off
// from its leader may show a lock free or an operation missing.
func TestFromConfig_consistentReads(t *testing.T) {
	var (
		mu    sync.Mutex
		stale []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Query().Has("stale") {
			stale = append(stale, r.URL.Path)
		}
		mu.Unlock()

		w.Header().Set("X-Consul-Knownleader", "true")
		w.Header().Set("X-Consul-Lastcontact", "0")

		if strings.HasPrefix(r.URL.Path, "/v1/kv/") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")

	cfg := serveConfig(t, config.ProfileWarmStandby)
	cfg.Consul.Servers = []config.ConsulServer{{Address: addr, Datacenter: "dc_a"}, {Address: addr, Datacenter: "dc_b"}}
	cfg.Status.RequestTimeout = 900 * time.Millisecond

	c, err := FromConfig(cfg, Instance{}, nil, ReadsConsistent, testLogger())
	require.NoError(t, err)

	s, err := c.Collect(t.Context())
	require.NoError(t, err)
	require.True(t, s.Voters[0].KV.Reachable, "%v", s.Voters[0].KV.Err)

	mu.Lock()
	defer mu.Unlock()

	assert.Empty(t, stale)
}

type passingCatalog struct {
	ClusterCatalog

	nodes   []consul.ServiceNode
	service string
	passing bool
}

func (c *passingCatalog) ClusterInstances(_ context.Context, service, _ string, passingOnly bool) ([]consul.ServiceNode, consul.QueryMeta, error) {
	c.service, c.passing = service, passingOnly

	return c.nodes, consul.QueryMeta{}, nil
}

func TestClusterResolver(t *testing.T) {
	catalog := &passingCatalog{nodes: []consul.ServiceNode{
		{Address: "10.1.0.5", Port: 5432},
		{NodeAddress: "10.2.0.5", Port: 5432},
	}}

	addrs, err := clusterResolver{catalog}.HealthyAddrs(t.Context(), "dc_a", "pg", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.1.0.5:5432", "10.2.0.5:5432"}, addrs, "members of every datacenter seed the cluster")
	assert.Equal(t, "pg", catalog.service)
	assert.True(t, catalog.passing)
}
