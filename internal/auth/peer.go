package auth

import (
	"context"
	"crypto/x509"
	"maps"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"
)

const (
	// peerCacheTTL is how long the known instances are trusted without
	// asking Consul.
	peerCacheTTL = 30 * time.Second
	// peerMissRefresh is the least time between two Consul queries caused
	// by an unknown certificate.
	peerMissRefresh = time.Second
	// peerRefreshTimeout bounds one query of every datacenter.
	peerRefreshTimeout = 10 * time.Second
)

// Catalog lists the dcctl instances registered in Consul.
type Catalog interface {
	Datacenters() []string
	// RegisteredAddrs returns host:port of every instance of service in
	// datacenter dc, passing or not.
	RegisteredAddrs(ctx context.Context, dc, service string) ([]string, error)
}

// Peers admits on listen.peer only calls whose client certificate has a SAN
// IP of a dcctl instance registered in the Consul of any datacenter. The TLS
// handshake has already checked that the certificate is signed by the CA.
type Peers struct {
	catalog Catalog
	log     *logging.Logger

	// known is replaced as a whole after each query of Consul; nil until
	// the first query ends.
	known   atomic.Pointer[peerSnapshot]
	refresh singleflight.Group
}

// peerSnapshot is the result of one query of Consul.
type peerSnapshot struct {
	// byDC holds the instance IPs of each datacenter from its last
	// successful query.
	byDC map[string]map[netip.Addr]struct{}
	// queried is when Consul was asked, successfully or not.
	queried time.Time
}

func (s *peerSnapshot) contains(ips []netip.Addr) bool {
	if s == nil {
		return false
	}

	for _, set := range s.byDC {
		for _, ip := range ips {
			if _, ok := set[ip]; ok {
				return true
			}
		}
	}

	return false
}

// NewPeers returns the peer check over the instances in c.
func NewPeers(c Catalog, log *logging.Logger) *Peers {
	return &Peers{catalog: c, log: log}
}

// UnaryServerInterceptor admits the call; see StreamServerInterceptor.
func (p *Peers) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := p.admit(ctx)
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor admits the call of a dcctl instance and puts its
// InitiatorKey metadata in the context; any other call fails with
// PermissionDenied. Consul unreachable with nothing known yet denies too.
func (p *Peers) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := p.admit(ss.Context())
		if err != nil {
			return err
		}

		return handler(srv, serverStream{ServerStream: ss, ctx: ctx})
	}
}

func (p *Peers) admit(ctx context.Context) (context.Context, error) {
	cl, leaf, ok := clientCert(ctx)
	if !ok {
		return nil, errNotPeer()
	}

	if !p.registered(ctx, certIPs(leaf)) {
		p.log.WarnCtx(ctx, "peer certificate is not of a registered dcctl instance",
			logging.KV(semconv.NetworkPeerAddress(addrString(cl.Addr)), semconv.TLSClientSubject(leaf.Subject.String())))

		return nil, errNotPeer()
	}

	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(InitiatorKey); len(v) > 0 {
		ctx = context.WithValue(ctx, initiatorKey{}, v[0])
	}

	return ctx, nil
}

// clientCert returns the peer of the call and its client certificate, as
// verified against the CA by the TLS handshake.
func clientCert(ctx context.Context) (*peer.Peer, *x509.Certificate, bool) {
	cl, ok := peer.FromContext(ctx)
	if !ok {
		return nil, nil, false
	}

	info, ok := cl.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return nil, nil, false
	}

	return cl, info.State.VerifiedChains[0][0], true
}

// certIPs returns the SAN IPs of cert, IPv4-mapped IPv6 ones as IPv4.
func certIPs(cert *x509.Certificate) []netip.Addr {
	ips := make([]netip.Addr, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		if a, ok := netip.AddrFromSlice(ip); ok {
			ips = append(ips, a.Unmap())
		}
	}

	return ips
}

func errNotPeer() error {
	return status.Error(codes.PermissionDenied, "client certificate is not of a registered dcctl instance")
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}

	return a.String()
}

// registered reports whether any of ips is a known instance.
//
// A known IP is admitted at once, also from a cache older than peerCacheTTL,
// which then starts a query of Consul in the background. An unknown IP
// waits for a query: always when the cache is older than peerCacheTTL, and
// at most every peerMissRefresh otherwise, so an instance that has just
// registered is admitted soon.
func (p *Peers) registered(ctx context.Context, ips []netip.Addr) bool {
	if len(ips) == 0 {
		return false
	}

	known := p.known.Load()
	fresh := known != nil && time.Since(known.queried) < peerCacheTTL

	if known.contains(ips) {
		if !fresh {
			p.query(ctx)
		}

		return true
	}

	if fresh && time.Since(known.queried) < peerMissRefresh {
		return false
	}

	select {
	case <-p.query(ctx):
		return p.known.Load().contains(ips)
	case <-ctx.Done():
		return false
	}
}

// query asks Consul for the instances unless a query is running already.
// The query does not end with the call that started it: a canceled call
// would otherwise leave the cache as it was.
func (p *Peers) query(ctx context.Context) <-chan singleflight.Result {
	return p.refresh.DoChan("", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerRefreshTimeout)
		defer cancel()

		p.known.Store(p.fetch(ctx, p.known.Load()))

		return nil, nil
	})
}

// fetch queries every datacenter at once. A datacenter whose Consul fails
// keeps its instances from prev: during a failover one datacenter is
// usually unreachable, and its instances must still be admitted.
func (p *Peers) fetch(ctx context.Context, prev *peerSnapshot) *peerSnapshot {
	next := &peerSnapshot{byDC: make(map[string]map[netip.Addr]struct{}), queried: time.Now()}
	if prev != nil {
		maps.Copy(next.byDC, prev.byDC)
	}

	dcs := p.catalog.Datacenters()
	results := make([]map[netip.Addr]struct{}, len(dcs))

	var wg sync.WaitGroup

	for i, dc := range dcs {
		wg.Go(func() {
			addrs, err := p.catalog.RegisteredAddrs(ctx, dc, buildinfo.ServiceName)
			if err != nil {
				p.log.WarnCtx(ctx, "list dcctl instances", logging.Str("datacenter", dc), logging.Err(err))

				return
			}

			results[i] = p.parse(ctx, dc, addrs)
		})
	}

	wg.Wait()

	for i, dc := range dcs {
		if results[i] != nil {
			next.byDC[dc] = results[i]
		}
	}

	return next
}

// parse returns the IPs of host:port addrs.
func (p *Peers) parse(ctx context.Context, dc string, addrs []string) map[netip.Addr]struct{} {
	set := make(map[netip.Addr]struct{}, len(addrs))

	for _, a := range addrs {
		ap, err := netip.ParseAddrPort(a)
		if err != nil {
			p.log.WarnCtx(ctx, "dcctl instance address is not ip:port",
				logging.Str("datacenter", dc), logging.Str("address", a))

			continue
		}

		set[ap.Addr().Unmap()] = struct{}{}
	}

	return set
}
