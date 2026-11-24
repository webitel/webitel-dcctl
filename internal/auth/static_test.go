package auth_test

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/peer"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/auth"
)

func newStaticPeers(ips ...string) *auth.StaticPeers {
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, netip.MustParseAddr(ip))
	}

	return auth.NewStaticPeers(addrs, logging.New(logging.WithProvider(logtest.NewRecorder())))
}

func callStatic(p *auth.StaticPeers, ctx context.Context) (handled, error) {
	var h handled

	_, err := p.UnaryServerInterceptor()(ctx, "req", unaryInfo, h.unary)

	return h, err
}

// plainAuthInfo is the AuthInfo of a connection without TLS.
type plainAuthInfo struct{}

func (plainAuthInfo) AuthType() string { return "insecure" }

func TestStaticPeersAdmits(t *testing.T) {
	p := newStaticPeers("10.1.0.81", "fd00::81")

	tests := []struct {
		name string
		sans []string
	}{
		{"listed_ipv4", []string{"10.1.0.81"}},
		{"listed_ipv6", []string{"fd00::81"}},
		{"one_of_several", []string{"192.0.2.1", "10.1.0.81"}},
		{"ipv4_mapped", []string{"::ffff:10.1.0.81"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := callStatic(p, fromPeer(t.Context(), tt.sans))
			require.NoError(t, err)
			assert.True(t, h.called)
		})
	}
}

func TestStaticPeersDenies(t *testing.T) {
	plainPeer := func(ctx context.Context) context.Context {
		return peer.NewContext(ctx, &peer.Peer{
			Addr:     &net.TCPAddr{IP: net.ParseIP("10.1.0.81"), Port: 40000},
			AuthInfo: plainAuthInfo{},
		})
	}

	tests := []struct {
		name string
		p    *auth.StaticPeers
		ctx  func(context.Context) context.Context
	}{
		{"not_listed", newStaticPeers("10.1.0.81"), func(ctx context.Context) context.Context {
			return fromPeer(ctx, []string{"10.1.0.82"})
		}},
		{"no_san_ip", newStaticPeers("10.1.0.81"), func(ctx context.Context) context.Context {
			return fromPeer(ctx, nil)
		}},
		{"empty_list", newStaticPeers(), func(ctx context.Context) context.Context {
			return fromPeer(ctx, []string{"10.1.0.81"})
		}},
		{"no_peer", newStaticPeers("10.1.0.81"), func(ctx context.Context) context.Context { return ctx }},
		{"not_tls", newStaticPeers("10.1.0.81"), plainPeer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := callStatic(tt.p, tt.ctx(t.Context()))
			requireDenied(t, h, err)
		})
	}
}

func TestStaticPeersStream(t *testing.T) {
	p := newStaticPeers("10.1.0.81")

	t.Run("admits", func(t *testing.T) {
		var h handled

		err := p.StreamServerInterceptor()(nil, ctxStream{ctx: fromPeer(t.Context(), []string{"10.1.0.81"})}, streamInfo, h.stream)
		require.NoError(t, err)
		assert.True(t, h.called)
	})

	t.Run("denies", func(t *testing.T) {
		var h handled

		err := p.StreamServerInterceptor()(nil, ctxStream{ctx: fromPeer(t.Context(), []string{"10.1.0.82"})}, streamInfo, h.stream)
		requireDenied(t, h, err)
	})
}

func TestArbiterServerOptions(t *testing.T) {
	log := logging.New(logging.WithProvider(logtest.NewRecorder()))

	opts, err := auth.ArbiterServerOptions([]string{"10.1.0.81", "fd00::81"}, log)
	require.NoError(t, err)
	assert.Len(t, opts, 2)

	_, err = auth.ArbiterServerOptions([]string{"10.1.0.81", "dc-a-dcctl"}, log)
	assert.ErrorContains(t, err, `clients[1]: "dc-a-dcctl"`)
}
