package nomad_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

const (
	dc      = "dc_a"
	service = "nomad"
)

func newClient(t *testing.T, ca *testCA, addrs ...string) *nomad.Client {
	t.Helper()

	c, err := nomad.New(nomad.Options{
		Datacenter:     dc,
		Service:        service,
		CAFile:         ca.writeFile(t),
		Resolver:       &fakeResolver{addrs: map[string][]string{dc + "/" + service: addrs}},
		RequestTimeout: time.Second,
	})
	require.NoError(t, err)

	return c
}

func closedAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

func reasonOf(t *testing.T, err error) source.Reason {
	t.Helper()

	r, ok := source.ReasonOf(err)
	require.True(t, ok, "not classified: %v", err)

	return r
}

func TestEndpointWithInvalidCertificateIsNotUsed(t *testing.T) {
	trusted := newTestCA(t, "dc_a CA")
	stranger := newTestCA(t, "other CA")

	f := newFakeNomad()
	f.addJob("api", 1, map[string]string{"dcctl_stage": "1"}, false)

	t.Run("only endpoint is reported with tls", func(t *testing.T) {
		addr := startTLS(t, stranger, f.handler())
		c := newClient(t, trusted, addr)

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonTLS, reasonOf(t, err))

		var se *source.Error
		require.ErrorAs(t, err, &se)
		assert.Equal(t, addr, se.Endpoint)
	})

	t.Run("next endpoint with a valid certificate serves the request", func(t *testing.T) {
		bad := startTLS(t, stranger, f.handler())
		good := startTLS(t, trusted, f.handler())
		c := newClient(t, trusted, bad, good)

		managed, _, err := c.Jobs(t.Context())
		require.NoError(t, err)
		assert.Len(t, managed, 1)

		statuses, err := c.Endpoints(t.Context())
		require.NoError(t, err)
		require.Len(t, statuses, 2)
		assert.Equal(t, bad, statuses[0].Addr)
		assert.Equal(t, source.ReasonTLS, reasonOf(t, statuses[0].Err))
		assert.Equal(t, good, statuses[1].Addr)
		assert.NoError(t, statuses[1].Err)
	})

	t.Run("plain http endpoint is not used", func(t *testing.T) {
		srv := startPlain(t, f.handler())
		c := newClient(t, trusted, srv)

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonTLS, reasonOf(t, err))
	})
}

func TestProcessEnvDoesNotChangeTheClient(t *testing.T) {
	trusted := newTestCA(t, "dc_a CA")
	stranger := newTestCA(t, "other CA")

	f := newFakeNomad()
	f.addJob("api", 1, map[string]string{"dcctl_stage": "1"}, false)

	t.Setenv("NOMAD_SKIP_VERIFY", "true")
	t.Setenv("NOMAD_CACERT", stranger.writeFile(t))
	t.Setenv("NOMAD_ADDR", "http://10.0.0.99:4646")
	t.Setenv("NOMAD_TOKEN", "00000000-0000-0000-0000-000000000000")
	t.Setenv("NOMAD_CLIENT_CERT", "/nonexistent/cert.pem")
	t.Setenv("HTTPS_PROXY", "http://10.0.0.98:3128")

	t.Run("skip verify is ignored", func(t *testing.T) {
		c := newClient(t, trusted, startTLS(t, stranger, f.handler()))

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonTLS, reasonOf(t, err))
	})

	t.Run("address and token come from the options only", func(t *testing.T) {
		c := newClient(t, trusted, startTLS(t, trusted, f.handler()))

		managed, _, err := c.Jobs(t.Context())
		require.NoError(t, err)
		assert.Len(t, managed, 1)
	})
}

func TestClientCertificate(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	addr := startMutualTLS(t, ca, newFakeNomad().handler())
	resolver := &fakeResolver{addrs: map[string][]string{dc + "/" + service: {addr}}}

	t.Run("without one the server refuses", func(t *testing.T) {
		c := newClient(t, ca, addr)

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonTLS, reasonOf(t, err))
	})

	t.Run("with one it is presented", func(t *testing.T) {
		cert, key := ca.clientCertFiles(t)

		c, err := nomad.New(nomad.Options{
			Datacenter: dc, Service: service, CAFile: ca.writeFile(t),
			CertFile: cert, KeyFile: key, Resolver: resolver,
		})
		require.NoError(t, err)

		_, _, err = c.Jobs(t.Context())
		require.NoError(t, err)
	})
}

func TestNewRejectsBadCA(t *testing.T) {
	_, err := nomad.New(nomad.Options{Datacenter: dc, Service: service, CAFile: "/nonexistent/ca.pem", Resolver: &fakeResolver{}})
	require.Error(t, err)

	empty := t.TempDir() + "/empty.pem"
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))

	_, err = nomad.New(nomad.Options{Datacenter: dc, Service: service, CAFile: empty, Resolver: &fakeResolver{}})
	require.Error(t, err)
}

func TestFailover(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	f := newFakeNomad()
	f.addJob("api", 1, map[string]string{"dcctl_stage": "1"}, false)
	good := startTLS(t, ca, f.handler())

	t.Run("refused endpoint is skipped", func(t *testing.T) {
		c := newClient(t, ca, closedAddr(t), good)

		managed, _, err := c.Jobs(t.Context())
		require.NoError(t, err)
		assert.Len(t, managed, 1)
	})

	t.Run("server error is skipped", func(t *testing.T) {
		broken := startTLS(t, ca, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no cluster leader", http.StatusInternalServerError)
		}))
		c := newClient(t, ca, broken, good)

		managed, _, err := c.Jobs(t.Context())
		require.NoError(t, err)
		assert.Len(t, managed, 1)
	})

	t.Run("client error on a read is final", func(t *testing.T) {
		other := newFakeNomad()
		other.addJob("api", 1, nil, false)
		second := startTLS(t, ca, other.handler())
		c := newClient(t, ca, good, second)

		_, err := c.Plan(t.Context(), "missing")
		require.Error(t, err)
		assert.Equal(t, source.ReasonHTTP, reasonOf(t, err))

		se, ok := errors.AsType[*source.StatusError](err)
		require.True(t, ok)
		assert.Equal(t, http.StatusNotFound, se.Code)
		assert.Zero(t, other.gets, "the second endpoint is not asked")
	})

	t.Run("server error keeps the message", func(t *testing.T) {
		broken := startTLS(t, ca, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "No cluster leader", http.StatusInternalServerError)
		}))
		c := newClient(t, ca, broken)

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonHTTP, reasonOf(t, err))
		assert.Contains(t, err.Error(), "No cluster leader")
	})

	t.Run("all endpoints failed reports every one", func(t *testing.T) {
		a, b := closedAddr(t), closedAddr(t)
		c := newClient(t, ca, a, b)

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonRefused, reasonOf(t, err))
		assert.Contains(t, err.Error(), a)
		assert.Contains(t, err.Error(), b)
	})
}

func TestNoEndpoints(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	c := newClient(t, ca)

	_, _, err := c.Jobs(t.Context())
	require.ErrorIs(t, err, nomad.ErrNoEndpoints)

	_, err = c.Endpoints(t.Context())
	require.ErrorIs(t, err, nomad.ErrNoEndpoints)
}

// countingListener counts the connections a port receives.
func countingListener(t *testing.T) (addr string, accepted func() int) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	var n atomic.Int32

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}

			n.Add(1)

			_ = conn.Close()
		}
	}()

	return l.Addr().String(), func() int { return int(n.Load()) }
}

func TestOnlyHTTPInstancesAreUsed(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	f := newFakeNomad()
	f.addJob("api", 1, map[string]string{"dcctl_stage": "1"}, false)

	// Nomad registers rpc and serf under the same service name as the HTTP API.
	rpc, rpcConns := countingListener(t)
	serf, serfConns := countingListener(t)
	httpAddr := startTLS(t, ca, f.handler())

	resolver := &fakeResolver{
		addrs: map[string][]string{dc + "/" + service: {httpAddr}},
		extra: []string{rpc, serf},
	}

	c, err := nomad.New(nomad.Options{Datacenter: dc, Service: service, CAFile: ca.writeFile(t), Resolver: resolver})
	require.NoError(t, err)

	_, _, err = c.Jobs(t.Context())
	require.NoError(t, err)

	statuses, err := c.Endpoints(t.Context())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, httpAddr, statuses[0].Addr)

	assert.Zero(t, rpcConns())
	assert.Zero(t, serfConns())
}

func TestResolverError(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	consulErr := &source.Error{Endpoint: "10.0.0.5:8500", Reason: source.ReasonRefused, Err: errors.New("dial")}

	c, err := nomad.New(nomad.Options{
		Datacenter: dc, Service: service, CAFile: ca.writeFile(t),
		Resolver: &fakeResolver{err: consulErr},
	})
	require.NoError(t, err)

	_, _, err = c.Jobs(t.Context())
	require.ErrorIs(t, err, consulErr)
	assert.Equal(t, source.ReasonRefused, reasonOf(t, err))
}

func TestClassification(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")

	status := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", code) })
	}
	body := func(s string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, s) })
	}

	hang := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.9/v1/jobs", http.StatusFound)
	})
	reset := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })

	tests := []struct {
		name string
		// handler is served over TLS; nil means the address is given by addr.
		handler http.Handler
		addr    string
		want    source.Reason
	}{
		{name: "timeout", handler: hang, want: source.ReasonTimeout},
		{name: "unauthorized", handler: status(http.StatusUnauthorized), want: source.ReasonAuth},
		{name: "forbidden", handler: status(http.StatusForbidden), want: source.ReasonAuth},
		{name: "server_error", handler: status(http.StatusServiceUnavailable), want: source.ReasonHTTP},
		{name: "redirect", handler: redirect, want: source.ReasonHTTP},
		{name: "invalid_json", handler: body("{"), want: source.ReasonInvalid},
		{name: "wrong_shape", handler: body(`{"a":1}`), want: source.ReasonInvalid},
		{name: "reset", handler: reset, want: source.ReasonHTTP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := tt.addr
			if tt.handler != nil {
				addr = startTLS(t, ca, tt.handler)
			}

			c, err := nomad.New(nomad.Options{
				Datacenter: dc, Service: service, CAFile: ca.writeFile(t),
				Resolver:       &fakeResolver{addrs: map[string][]string{dc + "/" + service: {addr}}},
				RequestTimeout: 200 * time.Millisecond,
			})
			require.NoError(t, err)

			_, _, err = c.Jobs(t.Context())
			require.Error(t, err)
			assert.Equal(t, tt.want, reasonOf(t, err))
		})
	}

	t.Run("refused", func(t *testing.T) {
		c := newClient(t, ca, closedAddr(t))

		_, _, err := c.Jobs(t.Context())
		require.Error(t, err)
		assert.Equal(t, source.ReasonRefused, reasonOf(t, err))
	})
}

func TestCanceledStaysUnclassified(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	f := newFakeNomad()
	c := newClient(t, ca, startTLS(t, ca, f.handler()), startTLS(t, ca, f.handler()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := c.Jobs(ctx)
	require.ErrorIs(t, err, context.Canceled)

	_, classified := source.ReasonOf(err)
	assert.False(t, classified)
}

func TestServiceName(t *testing.T) {
	assert.Equal(t, "nomad", nomad.ServiceName(config.ProfileWarmStandby, "dc_b"))
	assert.Equal(t, "nomad-dc_b", nomad.ServiceName(config.ProfileStretch, "dc_b"))
}

func TestFromConfig(t *testing.T) {
	ca := newTestCA(t, "dc_b CA")
	f := newFakeNomad()
	f.addJob("api", 1, map[string]string{"dcctl_stage": "1"}, false)
	addr := startTLS(t, ca, f.handler())

	cfg := config.Serve{Profile: config.ProfileStretch}
	cfg.TLS.CA = ca.writeFile(t)
	cfg.Status.RequestTimeout = time.Second

	r := &fakeResolver{addrs: map[string][]string{"dc_b/nomad-dc_b": {addr}}}

	c, err := nomad.FromConfig(cfg, "dc_b", r)
	require.NoError(t, err)

	managed, _, err := c.Jobs(t.Context())
	require.NoError(t, err)
	assert.Len(t, managed, 1)
}
