package auth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"maps"
	"net"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/auth"
)

var errConsulDown = errors.New("consul down")

// fakeCatalog serves the webitel-dcctl addresses of each datacenter; a
// datacenter in down fails. While gate is set, queries wait for it to close.
type fakeCatalog struct {
	mu    sync.Mutex
	addrs map[string][]string
	down  map[string]bool
	gate  chan struct{}
	calls int
}

func newCatalog(addrs map[string][]string) *fakeCatalog {
	return &fakeCatalog{addrs: addrs, down: map[string]bool{}}
}

func (c *fakeCatalog) Datacenters() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Collect(maps.Keys(c.addrs))
}

func (c *fakeCatalog) RegisteredAddrs(ctx context.Context, dc, service string) ([]string, error) {
	c.mu.Lock()
	c.calls++
	gate := c.gate
	c.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if service != "webitel-dcctl" {
		return nil, errors.New("unexpected service query")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.down[dc] {
		return nil, errConsulDown
	}

	return c.addrs[dc], nil
}

func (c *fakeCatalog) set(dc string, addrs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.addrs[dc] = addrs
}

func (c *fakeCatalog) setDown(dc string, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.down[dc] = down
}

// block makes the next queries wait until the returned func is called.
func (c *fakeCatalog) block() (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	gate := make(chan struct{})
	c.gate = gate

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.gate = nil
		close(gate)
	}
}

func (c *fakeCatalog) fetches() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

// fromPeer returns a context of a call over mTLS with a verified client
// certificate for ips, and the metadata pairs.
func fromPeer(ctx context.Context, ips []string, pairs ...string) context.Context {
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "node"}}
	for _, ip := range ips {
		leaf.IPAddresses = append(leaf.IPAddresses, net.ParseIP(ip))
	}

	ctx = peer.NewContext(ctx, &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.99"), Port: 40000},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{leaf}},
		}},
	})

	return metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
}

func newPeers(c *fakeCatalog) *auth.Peers {
	return auth.NewPeers(c, logging.New(logging.WithProvider(logtest.NewRecorder())))
}

// call runs the unary interceptor of p and reports what the handler saw.
func call(p *auth.Peers, ctx context.Context) (handled, error) {
	var h handled

	_, err := p.UnaryServerInterceptor()(ctx, "req", unaryInfo, h.unary)

	return h, err
}

// callIP calls p from a certificate with the SAN ip.
func callIP(t *testing.T, p *auth.Peers, ip string) (handled, error) {
	t.Helper()

	return call(p, fromPeer(t.Context(), []string{ip}))
}

func requireDenied(t *testing.T, h handled, err error) {
	t.Helper()

	require.Equal(t, codes.PermissionDenied, status.Code(err), "error = %v", err)
	assert.False(t, h.called, "handler must not run")
}

func TestPeersAdmitsRegisteredInstance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPeers(newCatalog(map[string][]string{
			"dc_a": {"10.0.0.1:8443"},
			"dc_b": {"10.1.0.1:8443", "[fd00::1]:8443"},
		}))

		for _, ip := range []string{"10.0.0.1", "10.1.0.1", "fd00::1"} {
			h, err := call(p, fromPeer(t.Context(), []string{"192.0.2.1", ip}))
			require.NoError(t, err, "SAN %s", ip)
			assert.True(t, h.called)
		}
	})
}

func TestPeersInitiator(t *testing.T) {
	p := newPeers(newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}}))

	t.Run("unary", func(t *testing.T) {
		var got string

		handler := func(ctx context.Context, _ any) (any, error) {
			got, _ = auth.InitiatorFromContext(ctx)

			return "ok", nil
		}

		ctx := fromPeer(t.Context(), []string{"10.0.0.1"}, auth.InitiatorKey, "user=alice")
		_, err := p.UnaryServerInterceptor()(ctx, "req", unaryInfo, handler)
		require.NoError(t, err)
		assert.Equal(t, "user=alice", got)
	})

	t.Run("stream", func(t *testing.T) {
		var h handled

		ctx := fromPeer(t.Context(), []string{"10.0.0.1"}, auth.InitiatorKey, "user=alice")
		err := p.StreamServerInterceptor()(nil, ctxStream{ctx: ctx}, streamInfo, h.stream)
		require.NoError(t, err)
		assert.True(t, h.initiator)
	})

	t.Run("absent", func(t *testing.T) {
		h, err := callIP(t, p, "10.0.0.1")
		require.NoError(t, err)
		assert.False(t, h.initiator)
	})
}

func TestPeersDenies(t *testing.T) {
	p := newPeers(newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}}))

	t.Run("san_not_registered", func(t *testing.T) {
		h, err := callIP(t, p, "10.0.0.2")
		requireDenied(t, h, err)
	})

	t.Run("no_san_ip", func(t *testing.T) {
		h, err := call(p, fromPeer(t.Context(), nil))
		requireDenied(t, h, err)
	})

	t.Run("no_tls", func(t *testing.T) {
		ctx := peer.NewContext(t.Context(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1}})
		h, err := call(p, ctx)
		requireDenied(t, h, err)
	})

	t.Run("stream", func(t *testing.T) {
		var h handled

		err := p.StreamServerInterceptor()(nil, ctxStream{ctx: fromPeer(t.Context(), []string{"10.0.0.2"})}, streamInfo, h.stream)
		requireDenied(t, h, err)
	})
}

func TestPeersConsulDownEmptyCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}})
		c.setDown("dc_a", true)

		h, err := callIP(t, newPeers(c), "10.0.0.1")
		requireDenied(t, h, err)
	})
}

func TestPeersCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}})
		p := newPeers(c)

		_, err := callIP(t, p, "10.0.0.1")
		require.NoError(t, err)
		_, err = callIP(t, p, "10.0.0.1")
		require.NoError(t, err)
		assert.Equal(t, 1, c.fetches(), "a hit within the TTL does not query Consul")

		time.Sleep(auth.PeerCacheTTL)

		_, err = callIP(t, p, "10.0.0.1")
		require.NoError(t, err)
		synctest.Wait()
		assert.Equal(t, 2, c.fetches(), "a hit on an expired cache refreshes it")
	})
}

func TestPeersRefreshOnMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}})
		p := newPeers(c)

		_, err := callIP(t, p, "10.0.0.1")
		require.NoError(t, err)

		// A peer that registered after the cache was filled.
		c.set("dc_a", "10.0.0.1:8443", "10.0.0.2:8443")

		h, err := callIP(t, p, "10.0.0.2")
		requireDenied(t, h, err)
		assert.Equal(t, 1, c.fetches(), "misses right after a refresh do not query Consul again")

		time.Sleep(auth.PeerMissRefresh)

		h, err = callIP(t, p, "10.0.0.2")
		require.NoError(t, err)
		assert.True(t, h.called)
		assert.Equal(t, 2, c.fetches())

		h, err = callIP(t, p, "10.0.0.3")
		requireDenied(t, h, err)
		assert.Equal(t, 2, c.fetches())
	})
}

func TestPeersStaleCacheWhenConsulDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCatalog(map[string][]string{
			"dc_a": {"10.0.0.1:8443"},
			"dc_b": {"10.1.0.1:8443"},
		})
		p := newPeers(c)

		_, err := callIP(t, p, "10.0.0.1")
		require.NoError(t, err)

		// The Consul of dc_b goes down while dc_a gets a new instance.
		c.setDown("dc_b", true)
		c.set("dc_a", "10.0.0.3:8443")
		time.Sleep(auth.PeerCacheTTL)

		_, err = callIP(t, p, "10.1.0.1")
		require.NoError(t, err)
		synctest.Wait()

		_, err = callIP(t, p, "10.1.0.1")
		require.NoError(t, err, "dc_b keeps its last known instances")

		_, err = callIP(t, p, "10.0.0.3")
		require.NoError(t, err, "dc_a is refreshed")

		h, err := callIP(t, p, "10.0.0.1")
		requireDenied(t, h, err)
	})
}

func TestPeersCanceledCallStillRefreshes(t *testing.T) {
	canceled := func(t *testing.T) context.Context {
		t.Helper()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		return ctx
	}

	t.Run("expired_cache", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443", "10.0.0.2:8443"}})
			p := newPeers(c)

			_, err := callIP(t, p, "10.0.0.1")
			require.NoError(t, err)

			// 10.0.0.1 is removed; the call that finds the cache expired is
			// canceled.
			c.set("dc_a", "10.0.0.2:8443")
			time.Sleep(auth.PeerCacheTTL)

			_, _ = call(p, fromPeer(canceled(t), []string{"10.0.0.1"}))

			synctest.Wait()

			h, err := callIP(t, p, "10.0.0.1")
			requireDenied(t, h, err)
			assert.Equal(t, 2, c.fetches())
		})
	})

	t.Run("miss", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}})
			p := newPeers(c)

			h, err := call(p, fromPeer(canceled(t), []string{"10.0.0.1"}))
			requireDenied(t, h, err)
			synctest.Wait()

			_, err = callIP(t, p, "10.0.0.1")
			require.NoError(t, err)
			assert.Equal(t, 1, c.fetches(), "the query of the canceled call filled the cache")
		})
	})
}

func TestPeersKnownDoesNotWaitForRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}})
		p := newPeers(c)

		_, err := callIP(t, p, "10.0.0.1")
		require.NoError(t, err)

		release := c.block()

		time.Sleep(auth.PeerMissRefresh)

		missed := make(chan error, 1)

		go func() {
			_, err := callIP(t, p, "10.0.0.9")
			missed <- err
		}()

		synctest.Wait()

		h, err := callIP(t, p, "10.0.0.1")
		require.NoError(t, err, "a known instance is admitted while Consul is queried")
		assert.True(t, h.called)

		release()
		assert.Equal(t, codes.PermissionDenied, status.Code(<-missed))
	})
}
