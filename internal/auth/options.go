package auth

import (
	"context"
	"fmt"

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
