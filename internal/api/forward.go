package api

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

const (
	peerReconnectBaseDelay = time.Second
	peerReconnectMaxDelay  = 5 * time.Second
	// peerKeepaliveTime is gRPC's least client ping interval; the peer
	// listener accepts pings every 5s.
	peerKeepaliveTime = 10 * time.Second
)

// Catalog lists the dcctl instances of a datacenter that pass their checks in
// Consul.
type Catalog interface {
	Instances(ctx context.Context, dc string) ([]consul.ServiceNode, error)
}

// CatalogOf returns the catalog of the Consul of every datacenter in c.
func CatalogOf(c *consul.Clients) Catalog { return consulCatalog{c} }

type consulCatalog struct{ c *consul.Clients }

func (k consulCatalog) Instances(ctx context.Context, dc string) ([]consul.ServiceNode, error) {
	cl, err := k.c.Client(dc)
	if err != nil {
		return nil, err
	}

	nodes, _, err := cl.Instances(ctx, buildinfo.ServiceName, "", true)

	return nodes, err
}

// Forwarder calls method on the dcctl instance at address, the listen.peer it
// registered in Consul; [*PeerClient] is one.
type Forwarder interface {
	Forward(ctx context.Context, address, method string, req, resp proto.Message) error
}

// ForwardTimeout returns how long the leading instance may take to answer a
// forwarded action: the read of dcctl/op for its request_id, its preflight
// and the one of the engine, which take preflightTimeout each, the lock,
// which takes two requests in each of kvs, the reads of dcctl/op and of the
// voters, the first write of the operation, and the call itself; every
// request takes requestTimeout at most.
func ForwardTimeout(requestTimeout, preflightTimeout time.Duration, kvs int) time.Duration {
	return 2*preflightTimeout + time.Duration(2*kvs+5)*requestTimeout
}

// PeerClient calls other dcctl instances on listen.peer with the node
// certificate, over one connection per address.
type PeerClient struct {
	creds          credentials.TransportCredentials
	timeout        time.Duration
	connectTimeout time.Duration

	mu     sync.Mutex
	conns  map[string]*grpc.ClientConn
	closed bool
}

// NewPeerClient reads the TLS files. timeout bounds one forwarded call, see
// [ForwardTimeout], and connectTimeout a connection attempt and a keepalive
// ping.
func NewPeerClient(c config.TLS, timeout, connectTimeout time.Duration) (*PeerClient, error) {
	if timeout <= 0 || connectTimeout <= 0 {
		return nil, errors.New("api: forward timeouts must be positive")
	}

	cert, pool, err := c.Load()
	if err != nil {
		return nil, err
	}

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	})

	return &PeerClient{
		creds: creds, timeout: timeout, connectTimeout: connectTimeout, conns: make(map[string]*grpc.ClientConn),
	}, nil
}

// Forward calls method with the outgoing metadata of ctx and no other.
func (c *PeerClient) Forward(ctx context.Context, address, method string, req, resp proto.Message) error {
	conn, err := c.conn(address)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	return conn.Invoke(ctx, method, req, resp)
}

func (c *PeerClient) conn(address string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, errors.New("peer client is closed")
	}

	if conn, ok := c.conns[address]; ok {
		return conn, nil
	}

	conn, err := grpc.NewClient("passthrough:///"+address,
		grpc.WithTransportCredentials(c.creds),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  peerReconnectBaseDelay,
				Multiplier: backoff.DefaultConfig.Multiplier,
				Jitter:     backoff.DefaultConfig.Jitter,
				MaxDelay:   peerReconnectMaxDelay,
			},
			MinConnectTimeout: c.connectTimeout,
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                peerKeepaliveTime,
			Timeout:             c.connectTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}

	c.conns[address] = conn

	return conn, nil
}

// Close closes every connection; Forward fails afterwards.
func (c *PeerClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true

	errs := make([]error, 0, len(c.conns))
	for _, conn := range c.conns {
		errs = append(errs, conn.Close())
	}

	clear(c.conns)

	return errors.Join(errs...)
}

// leader is the instance that leads an action.
type leader struct {
	// local is set when this instance leads it.
	local bool
	// datacenters lead it in this order: the first whose instance passes in
	// Consul. A datacenter of this instance after the first means this one.
	datacenters []string
	// node, when set, is the instance of the first datacenter that leads.
	node string
}

// leaderOf returns the leader of an action of typ on the datacenter dc, by
// name. switch, failover, demote, repoint, mark-recovered and resolve are led
// by an instance of dc, apps sync by one of the active datacenter, init and
// the arbiter init by this instance.
func (s *actionServer) leaderOf(typ dcctlv1.Operation_Type, dc string) leader {
	switch typ {
	case dcctlv1.Operation_INITIALIZE, dcctlv1.Operation_INITIALIZE_ARBITER:
		return leader{local: true}
	case dcctlv1.Operation_SYNC_APPS:
		// An active datacenter that is not known leaves the call here, its
		// preflight refuses it.
		st, ok := s.status.Status(s.now())
		if !ok || st.Effective == nil || !slices.Contains(s.cfg.Datacenters, st.Effective.ActiveDatacenter) {
			return leader{local: true}
		}

		dc = st.Effective.ActiveDatacenter
	case dcctlv1.Operation_TYPE_UNSPECIFIED, dcctlv1.Operation_SWITCH, dcctlv1.Operation_FAILOVER, dcctlv1.Operation_DEMOTE,
		dcctlv1.Operation_REPOINT, dcctlv1.Operation_MARK_RECOVERED, dcctlv1.Operation_RESOLVE_CONFLICT:
	}

	return leader{local: dc == s.cfg.Self.Datacenter, datacenters: []string{dc}}
}

// resumeLeader returns the leader of the resume of the operation name: the
// instance that led it, and when that one does not pass in Consul any
// instance of its target, this one included, also when the target is the
// datacenter of the instance that led it. An operation that is not read is
// resumed here, where the engine answers why.
func (s *actionServer) resumeLeader(ctx context.Context, name string) leader {
	view, err := s.ops.Read(ctx)
	if err != nil {
		return leader{local: true}
	}

	c := view.Of(name)
	if c == nil {
		return leader{local: true}
	}

	l := c.Operation.GetLeader()
	dc, node := datacenterID(l.GetDatacenter()), l.GetNode()

	if dc == "" || dc == s.cfg.Self.Datacenter && node == s.cfg.Self.Node {
		return leader{local: true}
	}

	out := leader{datacenters: []string{dc}, node: node}
	if target := datacenterID(c.Operation.GetTarget()); target != "" {
		out.datacenters = append(out.datacenters, target)
	}

	return out
}

// forward calls method with req on the instance of l and returns its answer;
// done is false when this instance leads the action after all. The call
// carries ini and nothing else of the call it forwards. An instance that
// cannot be found or reached gives LEADER_UNAVAILABLE.
func (s *actionServer) forward(ctx context.Context, l leader, method string, req proto.Message, ini *dcctlv1.Initiator) (op *dcctlv1.Operation, done bool, err error) {
	if l.local {
		return nil, false, nil
	}

	var tried []string

	for i, dc := range l.datacenters {
		if dc == s.cfg.Self.Datacenter && (l.node == "" || i > 0) {
			return nil, false, nil
		}

		want := ""
		if i == 0 {
			want = l.node
		}

		node, ok := s.instance(ctx, dc, want)
		if !ok {
			if !slices.Contains(tried, dc) {
				tried = append(tried, dc)
			}

			continue
		}

		fctx, err := auth.Forwarding(ctx, ini)
		if err != nil {
			return nil, true, rpcError(ctx, s.log, "action cannot be forwarded", err)
		}

		resp := &dcctlv1.Operation{}

		err = s.cfg.Forwarder.Forward(fctx, node.HostPort(), method, req, resp)
		if err != nil {
			return nil, true, s.leaderAnswer(ctx, err, dc, node.Node)
		}

		return resp, true, nil
	}

	return nil, true, leaderUnavailable(tried)
}

// instance returns the passing instance node of dc, or with no node the first
// by node name; this instance is not one.
func (s *actionServer) instance(ctx context.Context, dc, node string) (consul.ServiceNode, bool) {
	nodes, err := s.cfg.Catalog.Instances(ctx, dc)
	if err != nil {
		s.log.WarnCtx(ctx, "dcctl instances not listed", logging.Str("datacenter", dc), logging.Err(err))

		return consul.ServiceNode{}, false
	}

	nodes = slices.DeleteFunc(slices.Clone(nodes), func(n consul.ServiceNode) bool {
		return dc == s.cfg.Self.Datacenter && n.Node == s.cfg.Self.Node || node != "" && n.Node != node
	})
	if len(nodes) == 0 {
		return consul.ServiceNode{}, false
	}

	return slices.MinFunc(nodes, func(a, b consul.ServiceNode) int { return strings.Compare(a.Node, b.Node) }), true
}

// leaderAnswer returns err of a forwarded call as this instance answers it.
// A status with a dcctl reason is the answer of the leading instance, which
// went through its error guard, and is returned as it is. Without a reason:
// Unavailable, DeadlineExceeded, PermissionDenied, Unauthenticated, Unknown
// and an error that is not a status mean the instance was not reached or did
// not answer, LEADER_UNAVAILABLE; Internal, ResourceExhausted and DataLoss
// keep their code with a fixed text, as this client may have made them and
// their text may name addresses; any other code is returned as it is. A call
// whose own context ended answers with its context error.
func (s *actionServer) leaderAnswer(ctx context.Context, err error, dc, node string) error {
	if ctx.Err() != nil {
		return rpcError(ctx, s.log, "forwarded call canceled", ctx.Err())
	}

	st := status.Convert(err)
	if reasonOfError(st.Err()) != "" {
		return st.Err()
	}

	switch st.Code() {
	case codes.DeadlineExceeded:
		s.log.WarnCtx(ctx, "leading instance did not answer in time", logging.Str("datacenter", dc),
			logging.Str("node", node), logging.Err(err))

		return apierr.Status(apierr.ReasonLeaderUnavailable,
			"the instance that leads the action did not answer in time: the outcome is unknown, "+
				"repeat the request with the same request_id",
			map[string]string{"datacenters": dc}).Err()
	case codes.Unavailable, codes.PermissionDenied, codes.Unauthenticated, codes.Unknown:
		s.log.WarnCtx(ctx, "leading instance unavailable", logging.Str("datacenter", dc),
			logging.Str("node", node), logging.Err(err))

		return leaderUnavailable([]string{dc})
	case codes.Internal, codes.ResourceExhausted, codes.DataLoss:
		s.log.ErrorCtx(ctx, "forwarded call failed", logging.Str("datacenter", dc),
			logging.Str("node", node), logging.Err(err))

		return status.Error(st.Code(), "the leading instance did not answer the call")
	case codes.OK, codes.Canceled, codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
		codes.FailedPrecondition, codes.Aborted, codes.OutOfRange, codes.Unimplemented:
	}

	return st.Err()
}

func leaderUnavailable(datacenters []string) error {
	var md map[string]string
	if len(datacenters) > 0 {
		md = map[string]string{"datacenters": strings.Join(datacenters, ",")}
	}

	return apierr.Status(apierr.ReasonLeaderUnavailable, "the instance that leads the action is unavailable", md).Err()
}
