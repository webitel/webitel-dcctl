// Package nomad is the client of the Nomad cluster of one datacenter. It finds the
// servers through Consul, talks to them over TLS verified against the configured CA,
// and reports every failure with a [source.Reason].
package nomad

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
)

const (
	defaultRequestTimeout  = 3 * time.Second
	defaultEnforceAttempts = 5

	tlsHandshakeTimeout = 10 * time.Second

	// emptyBodyErrPrefix starts the api package error for an empty body where JSON is expected.
	emptyBodyErrPrefix = "Got 0 byte response"

	maxBodyInError = 256

	// maxIdleConnsPerHost covers concurrent requests to one server, the parallel node
	// reads included, so that a burst does not open and drop connections.
	maxIdleConnsPerHost = 16
)

var (
	// ErrNoEndpoints means Consul has no passing Nomad instance in the datacenter.
	ErrNoEndpoints = fmt.Errorf("no healthy nomad endpoints: %w", source.ErrNoEndpoints)
	// ErrIndexConflict means the job changed between reading it and registering it.
	ErrIndexConflict = errors.New("job modify index conflict")
	// ErrInvalidStage is the meta.dcctl_stage of a job that is not an integer of at least 1.
	ErrInvalidStage = errors.New("invalid dcctl_stage")
)

// httpTag marks the HTTP API instance among the three Nomad registers under one service
// name (the others are rpc and serf).
const httpTag = "http"

// minJobMetaVersion is the first Nomad whose job list can include meta; older servers
// ignore the parameter.
var minJobMetaVersion = [2]int{1, 5}

// Resolver finds the Nomad servers of a datacenter.
type Resolver interface {
	// HealthyAddrs returns host:port of passing instances of service in datacenter dc
	// that carry tag.
	HealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error)
}

// Options configure a [Client].
type Options struct {
	Datacenter string
	// Service is the Consul service of the Nomad servers; see [ServiceName].
	Service string
	// CAFile is the PEM file Nomad certificates are verified against.
	CAFile string
	// CertFile and KeyFile are the client certificate for a Nomad that sets
	// verify_https_client; both empty means none is presented.
	CertFile string
	KeyFile  string
	Resolver Resolver
	// RequestTimeout bounds one HTTP request; zero means 3s.
	RequestTimeout time.Duration
	// EnforceAttempts bounds the register attempts of [Client.Start]; zero means 5.
	EnforceAttempts int
	// ConsistentReads makes every read go to the leader, for a client that acts on what it
	// reads. Without it a follower may answer, which suits the status.
	ConsistentReads bool
}

// Client talks to the Nomad cluster of one datacenter.
type Client struct {
	dc       string
	service  string
	resolver Resolver
	// http is shared by the api clients of every endpoint, so idle connections are pooled
	// once and not per request walk.
	http       *http.Client
	timeout    time.Duration
	attempts   int
	consistent bool

	mu sync.Mutex
	// versionOK holds the endpoints that already passed the version check.
	versionOK map[string]bool
}

// ServiceName is the Consul service Nomad servers of dc are registered as.
func ServiceName(profile config.Profile, dc string) string {
	if profile == config.ProfileStretch {
		return "nomad-" + dc
	}

	return "nomad"
}

// FromConfig builds the client of datacenter dc from the serve config.
func FromConfig(cfg config.Serve, dc string, r Resolver) (*Client, error) {
	return New(Options{
		Datacenter:     dc,
		Service:        ServiceName(cfg.Profile, dc),
		CAFile:         cfg.TLS.CA,
		CertFile:       cfg.TLS.Cert,
		KeyFile:        cfg.TLS.Key,
		Resolver:       r,
		RequestTimeout: cfg.Status.RequestTimeout,
	})
}

// New returns a client that trusts only the CA in opts.CAFile.
//
// The HTTP client is built here and the api config is never taken from the process
// environment (NOMAD_ADDR, NOMAD_TOKEN, NOMAD_SKIP_VERIFY, NOMAD_CACERT and the like),
// so the environment cannot weaken verification or redirect requests.
func New(opts Options) (*Client, error) {
	pem, err := os.ReadFile(opts.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read nomad ca: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("nomad ca %s: no certificates", opts.CAFile)
	}

	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	if opts.CertFile != "" || opts.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load nomad client certificate: %w", err)
		}

		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	c := &Client{
		dc:       opts.Datacenter,
		service:  opts.Service,
		resolver: opts.Resolver,
		timeout:  opts.RequestTimeout,
		attempts: opts.EnforceAttempts,

		consistent: opts.ConsistentReads,

		versionOK: make(map[string]bool),
		http: &http.Client{
			// No Proxy: requests go straight to the address Consul returned.
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				TLSHandshakeTimeout: tlsHandshakeTimeout,
				MaxIdleConnsPerHost: maxIdleConnsPerHost,
				IdleConnTimeout:     time.Minute,
			},
			// A redirect could leave TLS or the verified host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	if c.timeout <= 0 {
		c.timeout = defaultRequestTimeout
	}

	if c.attempts <= 0 {
		c.attempts = defaultEnforceAttempts
	}

	return c, nil
}

// api returns the Nomad API client of one server. Everything the api package would
// otherwise read from the environment is set here, and nothing is taken from it.
func (c *Client) api(addr string) (*api.Client, error) {
	return api.NewClient(&api.Config{
		Address:    "https://" + addr,
		HttpClient: c.http,
	})
}

// addrs returns the HTTP API addresses of the datacenter's Nomad servers.
func (c *Client) addrs(ctx context.Context) ([]string, error) {
	addrs, err := c.resolver.HealthyAddrs(ctx, c.dc, c.service, httpTag)
	if err != nil {
		return nil, fmt.Errorf("resolve %s in %s: %w", c.service, c.dc, err)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: service %s in %s", ErrNoEndpoints, c.service, c.dc)
	}

	return addrs, nil
}

// each runs fn against the endpoints in turn until one succeeds. A failed endpoint is
// skipped; a 4xx answer is about the request, not the endpoint, so it ends the walk.
func (c *Client) each(ctx context.Context, fn func(ctx context.Context, nc *api.Client) error) error {
	addrs, err := c.addrs(ctx)
	if err != nil {
		return err
	}

	var errs []error

	for _, addr := range addrs {
		err := c.try(ctx, addr, fn)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil || isRequestError(err) {
			return err
		}

		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (c *Client) try(ctx context.Context, addr string, fn func(ctx context.Context, nc *api.Client) error) error {
	nc, err := c.api(addr)
	if err != nil {
		return source.Classify(addr, err)
	}

	err = fn(ctx, nc)

	// Exhausted EnforceIndex attempts are a busy job, not an unreachable endpoint.
	if errors.Is(err, ErrIndexConflict) || isCheckError(err) {
		return err
	}

	return source.Classify(addr, err)
}

func isCheckError(err error) bool {
	_, ok := errors.AsType[*checkError](err)

	return ok
}

func isRequestError(err error) bool {
	if errors.Is(err, ErrIndexConflict) || isCheckError(err) {
		return true
	}

	se, ok := errors.AsType[*source.StatusError](err)

	return ok && se.Code >= http.StatusBadRequest && se.Code < http.StatusInternalServerError
}

// request runs one API call under the request timeout and maps its error for
// [source.Classify].
func request[T any](ctx context.Context, c *Client, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	v, err := fn(ctx)

	return v, mapError(err)
}

// query returns the options of a read. A status read may be answered by a follower:
// during a leader election a consistent read fails with 500 and the whole status cycle
// would fail with it. With [Options.ConsistentReads] the leader answers, because a
// follower cut off from it may show a running job as stopped.
func (c *Client) query(ctx context.Context) *api.QueryOptions {
	return (&api.QueryOptions{AllowStale: !c.consistent}).WithContext(ctx)
}

// mapError turns what the api package returns into errors [source.Classify] understands.
// The start of the response body stays in the message, because Nomad puts the reason
// there ("No cluster leader", a validation failure). A rejected EnforceIndex is a plain
// error on the Nomad side and so arrives as HTTP 500; it is told apart by the message
// prefix the api package exports.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	if ure, ok := errors.AsType[api.UnexpectedResponseError](err); ok {
		var out error = &source.StatusError{Code: ure.StatusCode()}

		if body := strings.TrimSpace(ure.Body()); body != "" {
			out = fmt.Errorf("%w: %s", out, truncate(body, maxBodyInError))
		}

		if strings.Contains(ure.Body(), api.RegisterEnforceIndexErrPrefix) {
			return fmt.Errorf("%w: %w", ErrIndexConflict, out)
		}

		return out
	}

	// The api package returns a plain error for an empty body where JSON is expected.
	if strings.HasPrefix(err.Error(), emptyBodyErrPrefix) {
		return fmt.Errorf("%w: %w", source.ErrInvalidResponse, err)
	}

	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}
