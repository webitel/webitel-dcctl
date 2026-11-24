// Package patroni is the client of the Patroni REST API: member discovery, the state of a
// member and of the cluster, and the changes the primitives make.
//
// Every request goes over TLS verified against the configured CA. The basic auth
// credentials are sent only with the methods that change something.
package patroni

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
)

const (
	defaultRequestTimeout = 3 * time.Second
	// DefaultAPIPort is the port of the Patroni REST API.
	DefaultAPIPort = "8008"
	// maxBody caps a response; the largest one, GET /cluster, is far below it.
	maxBody = 4 << 20
	// maxErrBody caps the part of a failed response that is kept in the error.
	maxErrBody = 256
)

// ErrNoEndpoints means there is no member to start the discovery from.
var ErrNoEndpoints = fmt.Errorf("no patroni endpoints: %w", source.ErrNoEndpoints)

// Resolver gives the initial member list from the service catalog.
type Resolver interface {
	// HealthyAddrs returns host:port of passing instances of service in datacenter dc,
	// limited to those with tag when it is not empty.
	HealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error)
}

// Options configure a [Client].
type Options struct {
	// Scope is the Patroni scope, which is also the name of the service in Consul.
	Scope string
	// Datacenter is where the Consul service is looked up.
	Datacenter string
	// Endpoints is the static initial member list as host:port of the REST API; it is used
	// instead of Consul.
	Endpoints []string
	// APIPort is the REST port of the members found in Consul, which registers the
	// PostgreSQL port; zero value means [DefaultAPIPort].
	APIPort string
	// Resolver finds the initial members in Consul; it may be nil when Endpoints is set.
	Resolver Resolver
	// Username and Password are the basic auth credentials for the methods that change something.
	Username string
	Password string
	// CAFile is the PEM file of the CA that signs the members' certificates.
	CAFile string
	// RequestTimeout bounds a read and PATCH /config; zero means 3 seconds. Switchover and
	// failover are bounded by the context of the caller only.
	RequestTimeout time.Duration
}

// Client talks to the Patroni REST API of the members. It is safe for concurrent use.
type Client struct {
	opts Options
	http *http.Client
}

// FromConfig returns a client for datacenter dc configured from the serve config:
// the CA from tls.ca, the timeout from status.request_timeout, the rest from patroni.*.
func FromConfig(cfg config.Serve, dc string, r Resolver) (*Client, error) {
	return New(Options{
		Scope:          cfg.Patroni.Scope,
		Datacenter:     dc,
		Endpoints:      cfg.Patroni.Endpoints,
		Resolver:       r,
		Username:       cfg.Patroni.Username,
		Password:       cfg.Patroni.Password,
		CAFile:         cfg.TLS.CA,
		RequestTimeout: cfg.Status.RequestTimeout,
	})
}

// New returns a client.
func New(opts Options) (*Client, error) {
	if opts.CAFile == "" {
		return nil, errors.New("patroni: CA file is required, plain or unverified TLS is not allowed")
	}

	pem, err := os.ReadFile(opts.CAFile)
	if err != nil {
		return nil, fmt.Errorf("patroni: read CA file: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("patroni: no certificates in CA file %s", opts.CAFile)
	}

	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}

	if opts.APIPort == "" {
		opts.APIPort = DefaultAPIPort
	}

	return &Client{
		opts: opts,
		http: &http.Client{
			// No client timeout: a switchover is held by Patroni longer than a read may take.
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: opts.RequestTimeout}).DialContext,
				TLSHandshakeTimeout: opts.RequestTimeout,
				MaxIdleConns:        16,
				IdleConnTimeout:     30 * time.Second,
			},
			// A redirect would carry the credentials to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// roundTrip sends the request to endpoint (host:port) and returns the status and the body.
// The error is a transport failure, already classified; the status is not checked.
func (c *Client) roundTrip(ctx context.Context, endpoint, method, path string, in any, bounded bool) (int, []byte, error) {
	if bounded {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}

	var body io.Reader

	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, fmt.Errorf("encode %s %s: %w", method, path, err)
		}

		body = bytes.NewReader(b)
	}

	u := url.URL{Scheme: "https", Host: endpoint, Path: path}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, nil, source.Classify(endpoint, fmt.Errorf("build %s %s: %w", method, path, err))
	}

	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if changes(method) && c.opts.Username != "" {
		req.SetBasicAuth(c.opts.Username, c.opts.Password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, source.Classify(endpoint, fmt.Errorf("%s %s: %w", method, path, err))
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, source.Classify(endpoint, fmt.Errorf("read %s %s: %w", method, path, err))
	}

	if len(data) > maxBody {
		return 0, nil, source.Classify(endpoint, fmt.Errorf("%s %s: response over %d bytes: %w", method, path, maxBody, source.ErrInvalidResponse))
	}

	return resp.StatusCode, data, nil
}

// call sends the request under the request timeout, requires a 2xx status and decodes the
// body into out.
func (c *Client) call(ctx context.Context, endpoint, method, path string, in, out any) error {
	status, data, err := c.roundTrip(ctx, endpoint, method, path, in, true)
	if err != nil {
		return err
	}

	if status < 200 || status > 299 {
		return source.Classify(endpoint, statusError(method, path, status, data))
	}

	if err := json.Unmarshal(data, out); err != nil {
		return source.Classify(endpoint, fmt.Errorf("decode %s %s: %w: %w", method, path, source.ErrInvalidResponse, err))
	}

	return nil
}

// statusError keeps the start of the body, which is where Patroni explains a refusal. The
// body comes from the network, so it is cut on a rune boundary and stripped of control
// characters before it reaches a log line.
func statusError(method, path string, status int, body []byte) error {
	msg := printable(body[:min(len(body), maxErrBody)])
	se := &source.StatusError{Code: status}

	if msg == "" {
		return fmt.Errorf("%s %s: %w", method, path, se)
	}

	return fmt.Errorf("%s %s: %w: %s", method, path, se, msg)
}

// changes reports whether the method changes the state of the cluster.
func changes(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// printable returns b as a single line of valid UTF-8 without control characters.
func printable(b []byte) string {
	s := strings.ToValidUTF8(string(b), "")

	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, s))
}
