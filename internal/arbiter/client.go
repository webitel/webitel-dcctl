package arbiter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/arbiter/store"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
)

const (
	reconnectBaseDelay = time.Second
	reconnectMaxDelay  = 5 * time.Second
	// keepaliveTime is gRPC's least client ping interval; the peer listener
	// accepts pings every 5s.
	keepaliveTime = 10 * time.Second
)

// Record names.
const (
	RecordState = store.RecordState
	RecordLease = store.RecordLease
)

var (
	// ErrUninitialized means the arbiter has no state record yet and does
	// not vote.
	ErrUninitialized = errors.New("arbiter is not initialized")
	// ErrNotFound means an initialized arbiter has no such record.
	ErrNotFound = errors.New("arbiter record not found")
	// ErrEtagMismatch is matched by [*MismatchError].
	ErrEtagMismatch = errors.New("arbiter record etag mismatch")
	// ErrOutcomeUnknown marks a write that failed without an answer from the
	// arbiter, a canceled one included: it may have been applied, so re-read
	// the record. Unless canceled, the error is also classified, see
	// [source.ReasonOf].
	ErrOutcomeUnknown = errors.New("outcome of the arbiter write is unknown")
)

// Record is a record of the arbiter.
type Record struct {
	Value []byte
	// Etag is the record's index, to pass back to [Client.CAS].
	Etag       uint64
	UpdateTime time.Time
}

// MismatchError is returned by [Client.CAS] when the etag does not match
// the record, or the record to create exists. It matches [ErrEtagMismatch].
type MismatchError struct {
	Current Record
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s: current etag %d", ErrEtagMismatch, e.Current.Etag)
}

func (e *MismatchError) Is(target error) bool {
	return target == ErrEtagMismatch
}

// ClientOptions configures [NewClient] from the serve config.
type ClientOptions struct {
	// Address is host:port of the arbiter's listen.api.
	Address string
	// TLS is the node certificate presented to the arbiter and the CA its
	// certificate must be signed by.
	TLS config.TLS
	// Timeout bounds one call.
	Timeout time.Duration
}

// Client calls the arbiter over one connection. A failure to reach or use
// the arbiter is classified by [source.Classify]; the arbiter's answers
// are [ErrUninitialized], [ErrNotFound] and [*MismatchError].
type Client struct {
	addr    string
	timeout time.Duration
	conn    *grpc.ClientConn
	api     dcctlv1.ArbiterServiceClient
	// lastConnErr is why the last connection attempt or connection failed.
	// gRPC fails a call on a broken connection with Unavailable and only
	// the text of the cause, so the client keeps the cause to classify it.
	lastConnErr atomic.Pointer[error]
}

// NewClient reads the TLS files and returns a client of the arbiter. It
// connects on the first call.
func NewClient(opts ClientOptions) (*Client, error) {
	switch {
	case opts.Address == "":
		return nil, errors.New("arbiter address is empty")
	case opts.Timeout <= 0:
		return nil, errors.New("arbiter timeout must be positive")
	}

	cert, pool, err := opts.TLS.Load()
	if err != nil {
		return nil, err
	}

	c := &Client{addr: opts.Address, timeout: opts.Timeout}

	creds := recordingCreds{
		TransportCredentials: credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		}),
		c: c,
	}

	// passthrough dials the address as is, so a DNS failure reaches the
	// dialer and is classified. The arbiter is a quorum voter: the capped
	// backoff and keepalive bring it back within seconds after an outage or
	// a silently dropped connection.
	c.conn, err = grpc.NewClient("passthrough:///"+opts.Address,
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(c.dial),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  reconnectBaseDelay,
				Multiplier: backoff.DefaultConfig.Multiplier,
				Jitter:     backoff.DefaultConfig.Jitter,
				MaxDelay:   reconnectMaxDelay,
			},
			MinConnectTimeout: opts.Timeout,
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepaliveTime,
			Timeout:             opts.Timeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("arbiter client: %w", err)
	}

	c.api = dcctlv1.NewArbiterServiceClient(c.conn)

	return c, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Get returns the named record, [RecordState] or [RecordLease].
func (c *Client) Get(ctx context.Context, name string) (Record, error) {
	if err := checkName(name); err != nil {
		return Record{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.api.GetRecord(ctx, &dcctlv1.GetRecordRequest{Name: recordPrefix + name})
	if err != nil {
		return Record{}, c.callError(ctx, name, "get", err)
	}

	return c.record(name, resp)
}

// CAS writes value to the named record if its etag is still etag, and
// returns the written record. With allowMissing, etag must be 0 and the
// record is created only if it does not exist; creating [RecordState]
// initializes the arbiter. Without allowMissing, etag must be one read
// from the arbiter.
//
// A failure without an answer from the arbiter, cancellation included,
// matches [ErrOutcomeUnknown].
func (c *Client) CAS(ctx context.Context, name string, etag uint64, value []byte, allowMissing bool) (Record, error) {
	if err := checkName(name); err != nil {
		return Record{}, err
	}

	req := &dcctlv1.UpdateRecordRequest{
		Record:       &dcctlv1.Record{Name: recordPrefix + name, Value: value},
		AllowMissing: allowMissing,
	}

	switch {
	case allowMissing && etag != 0:
		return Record{}, fmt.Errorf("update arbiter record %s: etag %d with allow missing", name, etag)
	case !allowMissing && etag == 0:
		return Record{}, fmt.Errorf("update arbiter record %s: no etag", name)
	case !allowMissing:
		req.Record.Etag = strconv.FormatUint(etag, 10)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.api.UpdateRecord(ctx, req)
	if err != nil {
		code := status.Code(err)

		err = c.callError(ctx, name, "update", err)
		if errors.Is(err, context.Canceled) || code == codes.Unavailable || code == codes.DeadlineExceeded {
			return Record{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
		}

		return Record{}, err
	}

	return c.record(name, resp)
}

func checkName(name string) error {
	if name != RecordState && name != RecordLease {
		return fmt.Errorf("arbiter record %q: %w", name, store.ErrUnknownRecord)
	}

	return nil
}

// callError maps the error of a call on record name. ctx is the context of
// the call.
func (c *Client) callError(ctx context.Context, name, op string, err error) error {
	st := status.Convert(err)
	callErr := fmt.Errorf("%s arbiter record %s: %w", op, name, err)

	code, reason := st.Code(), reasonOf(st)
	cause := c.lastConnErr.Load()

	switch {
	case code == codes.Aborted && reason == apierr.ReasonEtagMismatch:
		return c.mismatch(name, st, callErr)
	case code == codes.FailedPrecondition && reason == apierr.ReasonArbiterUninitialized:
		return fmt.Errorf("%w: %w", ErrUninitialized, callErr)
	case code == codes.NotFound && reason == apierr.ReasonRecordNotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, callErr)
	case code == codes.PermissionDenied || code == codes.Unauthenticated:
		return &source.Error{Endpoint: c.addr, Reason: source.ReasonAuth, Err: callErr}
	case code == codes.Canceled && errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("%w: %w", context.Canceled, callErr)
	case code == codes.DeadlineExceeded:
		return source.Classify(c.addr, fmt.Errorf("%w: %w", callErr, context.DeadlineExceeded))
	case code == codes.Unavailable && cause != nil:
		return source.Classify(c.addr, fmt.Errorf("%w: %w", callErr, *cause))
	}

	return source.Classify(c.addr, callErr)
}

func (c *Client) mismatch(name string, st *status.Status, callErr error) error {
	for _, d := range st.Details() {
		r, ok := d.(*dcctlv1.Record)
		if !ok {
			continue
		}

		cur, err := c.record(name, r)
		if err != nil {
			return err
		}

		return &MismatchError{Current: cur}
	}

	return source.Classify(c.addr, fmt.Errorf("%w: %w: no current record", callErr, source.ErrInvalidResponse))
}

func reasonOf(st *status.Status) apierr.Reason {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == apierr.Domain {
			return apierr.Reason(info.GetReason())
		}
	}

	return ""
}

// record converts a record the arbiter returned for name.
func (c *Client) record(name string, r *dcctlv1.Record) (Record, error) {
	if r.GetName() != recordPrefix+name {
		return Record{}, source.Classify(c.addr,
			fmt.Errorf("arbiter returned record %q for %s: %w", r.GetName(), name, source.ErrInvalidResponse))
	}

	etag, err := strconv.ParseUint(r.GetEtag(), 10, 64)
	if err != nil {
		return Record{}, source.Classify(c.addr,
			fmt.Errorf("arbiter record %s etag %q: %w", name, r.GetEtag(), source.ErrInvalidResponse))
	}

	return Record{Value: r.GetValue(), Etag: etag, UpdateTime: r.GetUpdateTime().AsTime()}, nil
}

func (c *Client) dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		c.lastConnErr.Store(&err)

		return nil, err
	}

	return conn, nil
}

// recordingCreds keeps the TLS handshake error, and the errors of the
// connection after it, in the client.
type recordingCreds struct {
	credentials.TransportCredentials

	c *Client
}

func (r recordingCreds) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := r.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err != nil {
		r.c.lastConnErr.Store(&err)

		return nil, nil, err
	}

	r.c.lastConnErr.Store(nil)

	return recordingConn{Conn: conn, c: r.c}, info, nil
}

func (r recordingCreds) Clone() credentials.TransportCredentials {
	return recordingCreds{TransportCredentials: r.TransportCredentials.Clone(), c: r.c}
}

// recordingConn keeps why a connection broke. With TLS 1.3 the server
// rejects the client certificate after the handshake, so its alert is
// the first read.
type recordingConn struct {
	net.Conn

	c *Client
}

func (r recordingConn) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		r.c.lastConnErr.Store(&err)
	}

	return n, err
}
