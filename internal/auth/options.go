package auth

import (
	"context"
	"fmt"
	"net/netip"

	"buf.build/go/protovalidate"
	"google.golang.org/grpc"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/validation"
)

// ServerOptions returns the interceptors of the serve listeners: basic auth
// of users, then request validation, on listen.http; the instance
// certificate check over catalog, then request validation, on listen.peer.
func ServerOptions(ctx context.Context, users []config.User, catalog Catalog, log *logging.Logger) (httpOpts, peerOpts []grpc.ServerOption, err error) {
	basic, err := NewBasic(ctx, users, log)
	if err != nil {
		return nil, nil, err
	}

	v, err := protovalidate.New()
	if err != nil {
		return nil, nil, fmt.Errorf("protovalidate: %w", err)
	}

	peers := NewPeers(catalog, log)

	httpOpts = []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(basic.UnaryServerInterceptor(), validation.UnaryServerInterceptor(v)),
		grpc.ChainStreamInterceptor(basic.StreamServerInterceptor(), validation.StreamServerInterceptor(v)),
	}
	peerOpts = []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(peers.UnaryServerInterceptor(), validation.UnaryServerInterceptor(v)),
		grpc.ChainStreamInterceptor(peers.StreamServerInterceptor(), validation.StreamServerInterceptor(v)),
	}

	return httpOpts, peerOpts, nil
}

// ArbiterServerOptions returns the interceptors of the arbiter listener: the
// client certificate check against the IPs of clients, then request
// validation.
func ArbiterServerOptions(clients []string, log *logging.Logger) ([]grpc.ServerOption, error) {
	ips := make([]netip.Addr, 0, len(clients))

	for i, c := range clients {
		ip, err := netip.ParseAddr(c)
		if err != nil {
			return nil, fmt.Errorf("clients[%d]: %q is not an IP address", i, c)
		}

		ips = append(ips, ip)
	}

	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("protovalidate: %w", err)
	}

	peers := NewStaticPeers(ips, log)

	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(peers.UnaryServerInterceptor(), validation.UnaryServerInterceptor(v)),
		grpc.ChainStreamInterceptor(peers.StreamServerInterceptor(), validation.StreamServerInterceptor(v)),
	}, nil
}
