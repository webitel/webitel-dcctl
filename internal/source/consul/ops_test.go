package consul

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// What the clients of Nomad and Patroni ask of the Consul client.
type healthyAddrser interface {
	HealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error)
}

var (
	_ healthyAddrser   = (*Clients)(nil)
	_ nomad.Resolver   = (*Clients)(nil)
	_ patroni.Resolver = (*Clients)(nil)
)

func TestHealthyAddrs(t *testing.T) {
	ctx := t.Context()

	serve := func(t *testing.T) (*fake, *http.Request) {
		t.Helper()

		var got http.Request

		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			got = *r.Clone(context.Background())
			_, _ = w.Write([]byte(healthBody))
		})

		return f, &got
	}

	t.Run("warm_standby_has_no_node_meta_filter", func(t *testing.T) {
		f, req := serve(t)
		c := newTestClients(t, config.ProfileWarmStandby, f.addr())

		addrs, err := c.HealthyAddrs(ctx, "dc_a", "web", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.1.1:8080"}, addrs)
		assert.Equal(t, "/v1/health/service/web", req.URL.Path)
		assert.Empty(t, req.URL.Query()["node-meta"])
		assert.NotEmpty(t, req.URL.Query()["passing"])
	})

	t.Run("stretch_filters_by_node_meta_of_the_datacenter", func(t *testing.T) {
		f, req := serve(t)
		c := newTestClients(t, config.ProfileStretch, f.addr())

		_, err := c.HealthyAddrs(ctx, "dc_a", "web", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"webitel_datacenter:dc_a"}, req.URL.Query()["node-meta"])
	})

	t.Run("unknown_datacenter", func(t *testing.T) {
		f, _ := serve(t)
		c := newTestClients(t, config.ProfileStretch, f.addr())

		_, err := c.HealthyAddrs(ctx, "dc_x", "web", "")
		require.ErrorIs(t, err, ErrUnknownDatacenter)
	})

	t.Run("failure_is_classified", func(t *testing.T) {
		f := newFake(t, respond(http.StatusForbidden, "denied"))
		c := newTestClients(t, config.ProfileStretch, f.addr())

		_, err := c.HealthyAddrs(ctx, "dc_a", "web", "")

		reason, ok := source.ReasonOf(err)
		require.True(t, ok)
		assert.Equal(t, source.ReasonAuth, reason)
	})
}

func TestRegisteredAddrs(t *testing.T) {
	ctx := t.Context()

	serve := func(t *testing.T) (*fake, *http.Request) {
		t.Helper()

		var got http.Request

		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			got = *r.Clone(context.Background())
			_, _ = w.Write([]byte(healthBody))
		})

		return f, &got
	}

	t.Run("not_only_passing", func(t *testing.T) {
		f, req := serve(t)
		c := newTestClients(t, config.ProfileWarmStandby, f.addr())

		addrs, err := c.RegisteredAddrs(ctx, "dc_a", "web")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.1.1:8080"}, addrs)
		assert.Equal(t, "/v1/health/service/web", req.URL.Path)
		assert.Empty(t, req.URL.Query()["passing"])
		assert.Empty(t, req.URL.Query()["tag"])
		assert.Empty(t, req.URL.Query()["node-meta"])
	})

	t.Run("stretch_filters_by_node_meta_of_the_datacenter", func(t *testing.T) {
		f, req := serve(t)
		c := newTestClients(t, config.ProfileStretch, f.addr())

		_, err := c.RegisteredAddrs(ctx, "dc_a", "web")
		require.NoError(t, err)
		assert.Equal(t, []string{"webitel_datacenter:dc_a"}, req.URL.Query()["node-meta"])
	})

	t.Run("unknown_datacenter", func(t *testing.T) {
		f, _ := serve(t)
		c := newTestClients(t, config.ProfileStretch, f.addr())

		_, err := c.RegisteredAddrs(ctx, "dc_x", "web")
		require.ErrorIs(t, err, ErrUnknownDatacenter)
	})
}

func TestClusterInstances(t *testing.T) {
	var got http.Request

	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		_, _ = w.Write([]byte(healthBody))
	})
	c := newTestClients(t, config.ProfileStretch, f.addr())

	cl, err := c.Client("dc_a")
	require.NoError(t, err)

	nodes, _, err := cl.ClusterInstances(t.Context(), "pg", "primary", false)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, "/v1/health/service/pg", got.URL.Path)
	assert.Empty(t, got.URL.Query()["node-meta"], "a cluster-wide query is not filtered by datacenter")
	assert.Equal(t, []string{"primary"}, got.URL.Query()["tag"])
	assert.Empty(t, got.URL.Query()["passing"])
}

func TestReadConsistency(t *testing.T) {
	ctx := t.Context()

	serve := func(t *testing.T) (*fake, *[]string) {
		t.Helper()

		var (
			mu    sync.Mutex
			stale []string
		)

		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if r.URL.Query().Has("stale") {
				stale = append(stale, r.Method+" "+r.URL.Path)
			}
			mu.Unlock()

			w.Header().Set("X-Consul-Knownleader", "false")
			w.Header().Set("X-Consul-Lastcontact", "1500")

			switch {
			case strings.HasPrefix(r.URL.Path, "/v1/kv/") && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`[{"Key":"dcctl/state","Value":"dg==","ModifyIndex":7}]`))
			case strings.HasPrefix(r.URL.Path, "/v1/kv/"):
				_, _ = w.Write([]byte("true"))
			case strings.HasPrefix(r.URL.Path, "/v1/query/"):
				_, _ = w.Write([]byte(`{"Service":"webitel-api","Nodes":` + healthBody + `}`))
			default:
				_, _ = w.Write([]byte(healthBody))
			}
		})

		return f, &stale
	}

	t.Run("stale_reads_report_the_leader_state", func(t *testing.T) {
		f, stale := serve(t)
		c := newStaleClient(t, f.addr())
		want := QueryMeta{KnownLeader: false, LastContact: 1500 * time.Millisecond}

		e, meta, err := c.KVGet(ctx, "dcctl/state")
		require.NoError(t, err)
		assert.Equal(t, uint64(7), e.ModifyIndex)
		assert.Equal(t, want, meta)

		_, meta, err = c.Instances(ctx, "web", "", false)
		require.NoError(t, err)
		assert.Equal(t, want, meta)

		_, meta, err = c.ClusterInstances(ctx, "web", "", false)
		require.NoError(t, err)
		assert.Equal(t, want, meta)

		_, meta, err = c.ExecuteQuery(ctx, "webitel-api")
		require.NoError(t, err)
		assert.Equal(t, want, meta)

		_, err = c.KVList(ctx, "dcctl/journal/")
		require.NoError(t, err)

		_, err = c.KVCAS(ctx, "dcctl/state", []byte("v"), 7)
		require.NoError(t, err)

		assert.Equal(t, []string{
			"GET /v1/kv/dcctl/state",
			"GET /v1/health/service/web",
			"GET /v1/health/service/web",
			"GET /v1/query/webitel-api/execute",
			"GET /v1/kv/dcctl/journal/",
		}, *stale, "every read may be stale, a write may not")
	})

	t.Run("consistent_by_default", func(t *testing.T) {
		f, stale := serve(t)
		c := newTestClient(t, f.addr())

		_, _, err := c.KVGet(ctx, "dcctl/state")
		require.NoError(t, err)
		_, _, err = c.Instances(ctx, "web", "", false)
		require.NoError(t, err)
		_, _, err = c.ExecuteQuery(ctx, "webitel-api")
		require.NoError(t, err)

		assert.Empty(t, *stale)
	})

	t.Run("missing_key_reports_the_leader_state", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Consul-Knownleader", "true")
			w.Header().Set("X-Consul-Lastcontact", "0")
			w.WriteHeader(http.StatusNotFound)
		})
		c := newStaleClient(t, f.addr())

		_, meta, err := c.KVGet(ctx, "dcctl/state")
		require.ErrorIs(t, err, ErrKeyNotFound)
		assert.True(t, meta.KnownLeader)
	})
}

func TestKV(t *testing.T) {
	ctx := t.Context()

	t.Run("get_returns_ModifyIndex", func(t *testing.T) {
		f := newFake(t, respond(http.StatusOK, `[{"Key":"dcctl/state","Value":"dg==","ModifyIndex":7}]`))
		c := newTestClient(t, f.addr())

		e, _, err := c.KVGet(ctx, "dcctl/state")
		require.NoError(t, err)
		assert.Equal(t, uint64(7), e.ModifyIndex)
		assert.Equal(t, []byte("v"), e.Value)
	})

	t.Run("missing_key_is_not_an_unreachability", func(t *testing.T) {
		f := newFake(t, respond(http.StatusNotFound, ""))
		c := newTestClient(t, f.addr())

		_, _, err := c.KVGet(ctx, "dcctl/state")
		require.ErrorIs(t, err, ErrKeyNotFound)

		_, classified := source.ReasonOf(err)
		assert.False(t, classified)
	})

	t.Run("cas_sends_the_index", func(t *testing.T) {
		var cas string

		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			cas = r.URL.Query().Get("cas")
			_, _ = w.Write([]byte("true"))
		})
		c := newTestClient(t, f.addr())

		ok, err := c.KVCAS(ctx, "dcctl/state", []byte("v"), 7)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "7", cas)
	})
}

func TestSessions(t *testing.T) {
	ctx := t.Context()

	t.Run("create_uses_the_configured_ttl_lock_delay_release_and_no_node_checks", func(t *testing.T) {
		var body map[string]any

		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			_, _ = w.Write([]byte(`{"ID":"sid"}`))
		})
		c := newTestClient(t, f.addr())
		c.sessionTTL, c.lockDelay = 15*time.Second, 15*time.Second

		id, err := c.CreateSession(ctx, "dcctl/op")
		require.NoError(t, err)
		assert.Equal(t, "sid", id)
		assert.Equal(t, "15s", body["TTL"])
		assert.Equal(t, "15000ms", body["LockDelay"])
		assert.Equal(t, "release", body["Behavior"])
		assert.Equal(t, []any{}, body["NodeChecks"])
	})

	t.Run("renew_of_an_expired_session", func(t *testing.T) {
		f := newFake(t, respond(http.StatusNotFound, ""))
		c := newTestClient(t, f.addr())

		require.ErrorIs(t, c.RenewSession(ctx, "sid"), ErrSessionNotFound)
	})

	t.Run("acquire_fails_over_and_is_classified_when_no_server_answers", func(t *testing.T) {
		c := newTestClient(t, refusedAddr(t), refusedAddr(t))

		_, err := c.Acquire(ctx, "dcctl/lock", []byte("me"), "sid")

		reason, ok := source.ReasonOf(err)
		require.True(t, ok)
		assert.Equal(t, source.ReasonRefused, reason)
	})
}

func TestExecuteQuery(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/query/webitel-api/execute", r.URL.Path)

		_, _ = w.Write([]byte(`{"Service":"webitel-api","Nodes":` + healthBody + `}`))
	})
	c := newTestClients(t, config.ProfileStretch, f.addr()).mustClient(t, "dc_a")

	nodes, _, err := c.ExecuteQuery(t.Context(), "webitel-api")
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, "dc_a", nodes[0].NodeMeta["webitel_datacenter"])
}
