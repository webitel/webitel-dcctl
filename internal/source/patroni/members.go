package patroni

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/webitel/webitel-dcctl/internal/source"
)

// UnusableReason is why a member is not fit for operations.
type UnusableReason string

const (
	// UnusableVersion is a Patroni older than [MinVersion], or one that does not report its version.
	UnusableVersion UnusableReason = "version"
	// UnusableTLS is a certificate that does not verify against the CA.
	UnusableTLS UnusableReason = "tls"
	// UnusableUnreachable is any other failure to get the state of the member.
	UnusableUnreachable UnusableReason = "unreachable"
)

// Member is a member of the Patroni cluster.
type Member struct {
	Name string
	Host string
	// APIURL is the api_url the member published.
	APIURL string
	// Endpoint is host:port of the REST API taken from APIURL; empty when APIURL is not an https URL.
	Endpoint string
	Role     ClusterRole
	State    string
	// Timeline is nil when Patroni does not report it.
	Timeline *int64
	// Lag is the replication lag in bytes; nil when Patroni reports it as unknown.
	Lag *int64
	// FailoverPriority is the failover_priority tag; 1, the Patroni default, when it is not set.
	FailoverPriority int
	// NoFailover is the nofailover tag.
	NoFailover bool

	// Probe is GET /patroni of the member; nil when it could not be read or [Client.Cluster]
	// was used, which does not probe.
	Probe *State
	// Usable is set by [Client.Discover]: the member runs Patroni [MinVersion] or newer
	// and answers over verified TLS.
	Usable         bool
	UnusableReason UnusableReason
	// Err is why the state of the member could not be read.
	Err error
}

// defaultFailoverPriority is what Patroni uses when the tag is not set.
const defaultFailoverPriority = 1

type memberWire struct {
	Name     string          `json:"name"`
	Host     string          `json:"host"`
	APIURL   string          `json:"api_url"`
	Role     ClusterRole     `json:"role"`
	State    string          `json:"state"`
	Timeline *int64          `json:"timeline"`
	Lag      json.RawMessage `json:"lag"`
	Tags     struct {
		FailoverPriority *int `json:"failover_priority"`
		NoFailover       bool `json:"nofailover"`
	} `json:"tags"`
}

// Cluster reads GET /cluster of the member at endpoint (host:port): all members of the
// cluster with the role, state, lag and tags each one has. Probe and Usable are not set.
func (c *Client) Cluster(ctx context.Context, endpoint string) ([]Member, error) {
	var w struct {
		Members []memberWire `json:"members"`
	}
	if err := c.call(ctx, endpoint, http.MethodGet, "/cluster", nil, &w); err != nil {
		return nil, err
	}

	members := make([]Member, 0, len(w.Members))

	for _, m := range w.Members {
		lag, err := parseLag(m.Lag)
		if err != nil {
			return nil, source.Classify(endpoint, fmt.Errorf("decode GET /cluster member %q: %w: %w", m.Name, source.ErrInvalidResponse, err))
		}

		priority := defaultFailoverPriority
		if m.Tags.FailoverPriority != nil {
			priority = *m.Tags.FailoverPriority
		}

		members = append(members, Member{
			Name:             m.Name,
			Host:             m.Host,
			APIURL:           m.APIURL,
			Endpoint:         endpointOf(m.APIURL),
			Role:             m.Role,
			State:            m.State,
			Timeline:         m.Timeline,
			Lag:              lag,
			FailoverPriority: priority,
			NoFailover:       m.Tags.NoFailover,
		})
	}

	return members, nil
}

// parseLag reads the lag, which is a number of bytes or the string "unknown", the latter as nil.
func parseLag(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil //nolint:nilnil // an absent lag is unknown, not an error
	}

	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return &n, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s != "unknown" {
		return nil, fmt.Errorf("lag %s", raw)
	}

	return nil, nil //nolint:nilnil // "unknown" is a valid value, nil means unknown
}

// Members is [Client.Discover] without the endpoint.
func (c *Client) Members(ctx context.Context) ([]Member, error) {
	members, _, err := c.Discover(ctx)

	return members, err
}

// Discover discovers the members of the cluster and checks each one. The seeds are
// Options.Endpoints, or the passing instances of Options.Scope in Options.Datacenter when
// no endpoints are set. The first seed that answers GET /cluster gives all members, then
// GET /patroni of every member gives its version and whether it is usable. endpoint is
// the URL of that seed.
func (c *Client) Discover(ctx context.Context) (members []Member, endpoint string, err error) {
	seeds, err := c.seeds(ctx)
	if err != nil {
		return nil, "", err
	}

	members, seed, err := c.firstCluster(ctx, seeds)
	if err != nil {
		return nil, "", err
	}

	var wg sync.WaitGroup

	for i := range members {
		wg.Go(func() { c.check(ctx, &members[i]) })
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	return members, (&url.URL{Scheme: "https", Host: seed}).String(), nil
}

func (c *Client) seeds(ctx context.Context) ([]string, error) {
	if len(c.opts.Endpoints) > 0 {
		return c.opts.Endpoints, nil
	}

	if c.opts.Resolver == nil || c.opts.Scope == "" {
		return nil, ErrNoEndpoints
	}

	addrs, err := c.opts.Resolver.HealthyAddrs(ctx, c.opts.Datacenter, c.opts.Scope, "")
	if err != nil {
		return nil, fmt.Errorf("find service %s in %s: %w", c.opts.Scope, c.opts.Datacenter, err)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("service %s in %s: %w", c.opts.Scope, c.opts.Datacenter, ErrNoEndpoints)
	}

	// Consul holds the PostgreSQL port of the service, the REST API listens on another one.
	seeds := make([]string, 0, len(addrs))

	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}

		seeds = append(seeds, net.JoinHostPort(host, c.opts.APIPort))
	}

	return seeds, nil
}

// firstCluster returns the members from the first seed that answers, and that seed.
func (c *Client) firstCluster(ctx context.Context, seeds []string) ([]Member, string, error) {
	var errs []error

	for _, seed := range seeds {
		members, err := c.Cluster(ctx, seed)
		if err == nil {
			return members, seed, nil
		}

		if ctx.Err() != nil {
			return nil, "", err
		}

		errs = append(errs, err)
	}

	return nil, "", errors.Join(errs...)
}

// check fills Probe, Usable and UnusableReason of m from GET /patroni.
func (c *Client) check(ctx context.Context, m *Member) {
	if m.Endpoint == "" {
		// A member that publishes a plain http URL cannot be talked to without TLS.
		m.UnusableReason = UnusableTLS
		m.Err = source.Classify(m.Host, fmt.Errorf("member %q: api_url %q is not an https URL: %w", m.Name, m.APIURL, source.ErrInvalidResponse))

		return
	}

	st, err := c.State(ctx, m.Endpoint)
	if err != nil {
		m.Err = err
		m.UnusableReason = UnusableUnreachable

		if reason, ok := source.ReasonOf(err); ok && reason == source.ReasonTLS {
			m.UnusableReason = UnusableTLS
		}

		return
	}

	m.Probe = &st

	if !VersionSupported(st.Version) {
		m.UnusableReason = UnusableVersion

		return
	}

	m.Usable = true
}
