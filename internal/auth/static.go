package auth

import (
	"context"
	"net/netip"
	"slices"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"
)

// StaticPeers admits only calls whose client certificate has one of a fixed
// set of SAN IPs. The TLS handshake has already checked that the
// certificate is signed by the CA. An empty set admits nobody.
type StaticPeers struct {
	ips []netip.Addr
	log *logging.Logger
}

// NewStaticPeers returns the check over ips.
func NewStaticPeers(ips []netip.Addr, log *logging.Logger) *StaticPeers {
	allowed := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		allowed = append(allowed, ip.Unmap())
	}

	return &StaticPeers{ips: allowed, log: log}
}

// UnaryServerInterceptor admits the call or fails it with PermissionDenied.
func (p *StaticPeers) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := p.admit(ctx); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor admits the call or fails it with PermissionDenied.
func (p *StaticPeers) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := p.admit(ss.Context()); err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

func (p *StaticPeers) admit(ctx context.Context) error {
	cl, leaf, ok := clientCert(ctx)
	if !ok {
		return errNotAllowed()
	}

	for _, ip := range certIPs(leaf) {
		if slices.Contains(p.ips, ip) {
			return nil
		}
	}

	p.log.WarnCtx(ctx, "client certificate has no allowed IP",
		logging.KV(semconv.NetworkPeerAddress(addrString(cl.Addr)), semconv.TLSClientSubject(leaf.Subject.String())))

	return errNotAllowed()
}

func errNotAllowed() error {
	return status.Error(codes.PermissionDenied, "client certificate has no allowed IP")
}
