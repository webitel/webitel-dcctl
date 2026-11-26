package consul

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
)

const healthBody = `[{"Node":{"Node":"n1","Address":"10.0.0.1","Meta":{"webitel_datacenter":"dc_a"}},` +
	`"Service":{"ID":"web-1","Service":"web","Address":"10.0.1.1","Port":8080},` +
	`"Checks":[{"Status":"passing"}]}]`

// fake is a Consul HTTP API stand-in that counts the requests it gets.
type fake struct {
	*httptest.Server

	hits atomic.Int32
}

func newFake(t *testing.T, h http.HandlerFunc) *fake {
	t.Helper()

	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(f.Close)

	return f
}

func (f *fake) addr() string { return f.Listener.Addr().String() }

func respond(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

// refusedAddr returns an address nothing listens on.
func refusedAddr(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()

	return addr
}

func newTestClient(t *testing.T, addrs ...string) *Client {
	t.Helper()

	return newTestClients(t, config.ProfileWarmStandby, addrs...).mustClient(t, "dc_a")
}

func newTestClients(t *testing.T, profile config.Profile, addrs ...string) *Clients {
	t.Helper()

	servers := make([]config.ConsulServer, 0, len(addrs))
	for _, a := range addrs {
		servers = append(servers, config.ConsulServer{Address: a, Datacenter: "dc_a"})
	}

	c, err := New(Options{
		Profile:     profile,
		NodeMetaKey: "webitel_datacenter",
		Servers:     servers,
		SessionTTL:  15 * time.Second,
		LockDelay:   15 * time.Second,
		Timeout:     500 * time.Millisecond,
	})
	require.NoError(t, err)

	return c
}

// newStaleClient returns the client of dc_a with stale reads.
func newStaleClient(t *testing.T, addr string) *Client {
	t.Helper()

	c, err := New(Options{
		Profile:    config.ProfileWarmStandby,
		Servers:    []config.ConsulServer{{Address: addr, Datacenter: "dc_a"}},
		SessionTTL: 15 * time.Second,
		Timeout:    500 * time.Millisecond,
		AllowStale: true,
	})
	require.NoError(t, err)

	return c.mustClient(t, "dc_a")
}

func (c *Clients) mustClient(t *testing.T, dc string) *Client {
	t.Helper()

	cl, err := c.Client(dc)
	require.NoError(t, err)

	return cl
}

func TestNew(t *testing.T) {
	valid := Options{
		Servers:    []config.ConsulServer{{Address: "10.1.0.89:8500", Datacenter: "dc_a"}},
		SessionTTL: 15 * time.Second,
		Timeout:    time.Second,
	}

	t.Run("groups_servers_by_datacenter_in_order", func(t *testing.T) {
		opts := valid
		opts.Servers = []config.ConsulServer{
			{Address: "10.2.0.89:8500", Datacenter: "dc_b"},
			{Address: "10.1.0.89:8500", Datacenter: "dc_a"},
			{Address: "10.1.0.90:8500", Datacenter: "dc_a"},
		}

		c, err := New(opts)
		require.NoError(t, err)
		assert.Equal(t, []string{"dc_a", "dc_b"}, c.Datacenters())
		assert.Len(t, c.mustClient(t, "dc_a").pool.endpoints, 2)
		assert.Equal(t, "10.1.0.89:8500", c.mustClient(t, "dc_a").pool.endpoints[0].addr)
	})

	t.Run("unknown_datacenter", func(t *testing.T) {
		c, err := New(valid)
		require.NoError(t, err)

		_, err = c.Client("dc_x")
		require.ErrorIs(t, err, ErrUnknownDatacenter)
	})

	t.Run("invalid_options", func(t *testing.T) {
		noServers := valid
		noServers.Servers = nil

		noTimeout := valid
		noTimeout.Timeout = 0

		noTTL := valid
		noTTL.SessionTTL = 0

		for name, opts := range map[string]Options{"no servers": noServers, "no timeout": noTimeout, "no session ttl": noTTL} {
			_, err := New(opts)
			require.Error(t, err, name)
		}
	})
}

func TestFailover(t *testing.T) {
	ctx := t.Context()

	t.Run("refused_server_falls_over_to_the_next", func(t *testing.T) {
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, refusedAddr(t), ok.addr())

		got, _, err := c.Instances(ctx, "web", "", true)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, int32(1), ok.hits.Load())
	})

	t.Run("5xx_falls_over_and_the_working_server_stays_current", func(t *testing.T) {
		broken := newFake(t, respond(http.StatusInternalServerError, "No cluster leader"))
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, broken.addr(), ok.addr())

		for range 2 {
			_, _, err := c.Instances(ctx, "web", "", true)
			require.NoError(t, err)
		}

		assert.Equal(t, int32(1), broken.hits.Load())
		assert.Equal(t, int32(2), ok.hits.Load())
	})

	t.Run("wraps_around_to_the_first_server", func(t *testing.T) {
		var up atomic.Bool

		first := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			if up.Load() {
				respond(http.StatusOK, healthBody)(w, r)

				return
			}

			respond(http.StatusServiceUnavailable, "")(w, r)
		})
		second := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, first.addr(), second.addr())

		_, _, err := c.Instances(ctx, "web", "", true)
		require.NoError(t, err)

		second.Close()
		up.Store(true)

		_, _, err = c.Instances(ctx, "web", "", true)
		require.NoError(t, err)
	})

	t.Run("403_does_not_fall_over_and_is_auth", func(t *testing.T) {
		denied := newFake(t, respond(http.StatusForbidden, "ACL not found"))
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, denied.addr(), ok.addr())

		_, _, err := c.Instances(ctx, "web", "", true)
		require.Error(t, err)

		reason, found := source.ReasonOf(err)
		require.True(t, found)
		assert.Equal(t, source.ReasonAuth, reason)
		assert.Zero(t, ok.hits.Load())
	})

	t.Run("all_servers_down_is_classified_refused", func(t *testing.T) {
		c := newTestClient(t, refusedAddr(t), refusedAddr(t))

		_, _, err := c.Instances(ctx, "web", "", true)
		require.Error(t, err)

		reason, found := source.ReasonOf(err)
		require.True(t, found)
		assert.Equal(t, source.ReasonRefused, reason)
	})

	t.Run("hung_server_times_out_and_falls_over", func(t *testing.T) {
		hung := newFake(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, hung.addr(), ok.addr())

		_, _, err := c.Instances(ctx, "web", "", true)
		require.NoError(t, err)
	})

	t.Run("all_servers_hung_is_timeout", func(t *testing.T) {
		hung := newFake(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		c := newTestClient(t, hung.addr())

		_, _, err := c.Instances(ctx, "web", "", true)
		require.Error(t, err)

		reason, _ := source.ReasonOf(err)
		assert.Equal(t, source.ReasonTimeout, reason)
	})

	t.Run("invalid_body_is_invalid", func(t *testing.T) {
		bad := newFake(t, respond(http.StatusOK, "{not json"))
		c := newTestClient(t, bad.addr())

		_, _, err := c.Instances(ctx, "web", "", true)
		require.Error(t, err)

		reason, _ := source.ReasonOf(err)
		assert.Equal(t, source.ReasonInvalid, reason)
	})

	t.Run("empty_body_is_invalid", func(t *testing.T) {
		empty := newFake(t, respond(http.StatusOK, ""))
		c := newTestClient(t, empty.addr())

		_, _, err := c.Instances(ctx, "web", "", true)
		require.Error(t, err)

		reason, _ := source.ReasonOf(err)
		assert.Equal(t, source.ReasonInvalid, reason)
	})

	t.Run("canceled_stays_unclassified_and_stops", func(t *testing.T) {
		hung := newFake(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, hung.addr(), ok.addr())

		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(50*time.Millisecond, cancel)

		_, _, err := c.Instances(cctx, "web", "", true)
		require.ErrorIs(t, err, context.Canceled)

		_, classified := source.ReasonOf(err)
		assert.False(t, classified)
		assert.Zero(t, ok.hits.Load())
	})

	t.Run("expired_caller_deadline_does_not_try_other_servers", func(t *testing.T) {
		hung := newFake(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := newTestClient(t, hung.addr(), ok.addr())

		dctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		_, _, err := c.Instances(dctx, "web", "", true)
		require.Error(t, err)

		reason, _ := source.ReasonOf(err)
		assert.Equal(t, source.ReasonTimeout, reason)
		assert.Zero(t, ok.hits.Load())
	})
}

func TestQueryMeta_endpointIsTheServerThatAnswered(t *testing.T) {
	ctx := t.Context()

	routes := http.NewServeMux()
	routes.HandleFunc("/v1/health/service/", respond(http.StatusOK, healthBody))
	routes.HandleFunc("/v1/query/", respond(http.StatusOK, `{"Nodes":`+healthBody+`}`))
	routes.HandleFunc("/v1/kv/present", respond(http.StatusOK, `[{"Key":"present","Value":"dg==","ModifyIndex":1}]`))
	routes.HandleFunc("/v1/kv/missing", respond(http.StatusNotFound, ""))

	ok := newFake(t, routes.ServeHTTP)

	tests := []struct {
		name    string
		read    func(c *Client) (QueryMeta, error)
		wantErr error
	}{
		{name: "instances", read: func(c *Client) (QueryMeta, error) {
			_, meta, err := c.Instances(ctx, "web", "", true)

			return meta, err
		}},
		{name: "cluster_instances", read: func(c *Client) (QueryMeta, error) {
			_, meta, err := c.ClusterInstances(ctx, "web", "", true)

			return meta, err
		}},
		{name: "execute_query", read: func(c *Client) (QueryMeta, error) {
			_, meta, err := c.ExecuteQuery(ctx, "webitel-api")

			return meta, err
		}},
		{name: "kv_get", read: func(c *Client) (QueryMeta, error) {
			_, meta, err := c.KVGet(ctx, "present")

			return meta, err
		}},
		{name: "kv_get_missing_key", wantErr: ErrKeyNotFound, read: func(c *Client) (QueryMeta, error) {
			_, meta, err := c.KVGet(ctx, "missing")

			return meta, err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broken := newFake(t, respond(http.StatusInternalServerError, "No cluster leader"))
			c := newTestClient(t, broken.addr(), ok.addr())

			meta, err := tt.read(c)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, int32(1), broken.hits.Load(), "the read falls over from the first server")
			assert.Equal(t, ok.addr(), meta.Endpoint)
		})
	}
}
