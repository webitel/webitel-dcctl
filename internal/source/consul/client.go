// Package consul is the Consul source of dcctl: a client per datacenter over the servers of
// consul.servers, with every network or HTTP error classified by [source.Classify].
package consul

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/hashicorp/consul/api"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// Options configures [New] from the serve config.
type Options struct {
	Profile config.Profile
	// NodeMetaKey is the node_meta key that holds the datacenter of a node.
	NodeMetaKey string
	Servers     []config.ConsulServer
	// SessionTTL and LockDelay are those of the sessions of [Client.CreateSession].
	// Consul accepts a TTL of 10s to 24h and a lock delay of up to 60s; zero lock delay
	// leaves Consul's default of 15s.
	SessionTTL time.Duration
	LockDelay  time.Duration
	// Timeout bounds one request to one server.
	Timeout time.Duration
	// AllowStale lets any server answer a read, also one without a leader, as on the
	// minority side of a partition; [QueryMeta] tells how current the answer is. It is for
	// status reads; coordination reads stay consistent. Writes are never stale.
	AllowStale bool
}

// Clients holds the [Client] of every datacenter.
type Clients struct {
	byDC   map[string]*Client
	voters []*Client
}

// Client talks to the Consul of one datacenter, trying its servers in order.
type Client struct {
	dc   string
	pool *pool
	// nodeMeta filters services by datacenter; set only in stretch, where
	// one Consul serves every datacenter.
	nodeMeta   map[string]string
	sessionTTL time.Duration
	lockDelay  time.Duration
	allowStale bool
}

// QueryMeta is how current a read is.
type QueryMeta struct {
	// KnownLeader is false when the server that answered has no leader.
	KnownLeader bool
	// LastContact is how long ago that server last heard from the leader; zero on the
	// leader itself.
	LastContact time.Duration
}

// New builds the clients of opts.Servers, which are tried in the order they are listed.
//
// In warm_standby every datacenter has its own Consul, and so its own servers. In stretch
// all servers belong to ONE Consul and the datacenter of a server is only where it runs:
// every datacenter shares all unique addresses and its [Client] differs only by the
// node-meta filter of its services.
func New(opts Options) (*Clients, error) {
	switch {
	case len(opts.Servers) == 0:
		return nil, errors.New("no consul servers")
	case opts.Timeout <= 0:
		return nil, errors.New("consul request timeout must be positive")
	case opts.SessionTTL <= 0:
		return nil, errors.New("consul session ttl must be positive: a session without checks and without ttl never expires")
	}

	var (
		order []string // datacenters in config order
		addrs = make(map[string][]string)
		all   []string
	)

	for _, s := range opts.Servers {
		if _, ok := addrs[s.Datacenter]; !ok {
			order = append(order, s.Datacenter)
		}

		if !slices.Contains(addrs[s.Datacenter], s.Address) {
			addrs[s.Datacenter] = append(addrs[s.Datacenter], s.Address)
		}

		if !slices.Contains(all, s.Address) {
			all = append(all, s.Address)
		}
	}

	stretch := opts.Profile == config.ProfileStretch
	clients := &Clients{byDC: make(map[string]*Client, len(order))}

	var shared *pool

	if stretch {
		var err error

		if shared, err = newPool(all, opts.Timeout); err != nil {
			return nil, err
		}
	}

	for _, dc := range order {
		p := shared

		if p == nil {
			var err error

			if p, err = newPool(addrs[dc], opts.Timeout); err != nil {
				return nil, fmt.Errorf("datacenter %s: %w", dc, err)
			}
		}

		c := &Client{dc: dc, pool: p, sessionTTL: opts.SessionTTL, lockDelay: opts.LockDelay, allowStale: opts.AllowStale}
		if stretch {
			c.nodeMeta = map[string]string{opts.NodeMetaKey: dc}
		}

		clients.byDC[dc] = c
	}

	if stretch {
		clients.voters = []*Client{clients.byDC[order[0]]}
	} else {
		for _, dc := range clients.Datacenters() {
			clients.voters = append(clients.voters, clients.byDC[dc])
		}
	}

	return clients, nil
}

// Client returns the client of datacenter dc, or [ErrUnknownDatacenter].
func (c *Clients) Client(dc string) (*Client, error) {
	cl, ok := c.byDC[dc]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownDatacenter, dc)
	}

	return cl, nil
}

// Datacenters returns the datacenters in name order.
func (c *Clients) Datacenters() []string {
	dcs := make([]string, 0, len(c.byDC))
	for dc := range c.byDC {
		dcs = append(dcs, dc)
	}

	sort.Strings(dcs)

	return dcs
}

// Voters returns the clients that hold KV and locks, which coordination reads and locks
// through. There is one per datacenter in warm_standby, in name order. In stretch there
// is a single entry, because one Consul holds the KV and the lock for all datacenters:
// looping over [Clients.Datacenters] there would repeat one key as if it were several
// voters. Use [Clients.Client] for what is per datacenter, the services.
func (c *Clients) Voters() []*Client { return slices.Clone(c.voters) }

// Datacenter returns the name of the datacenter of the client.
func (c *Client) Datacenter() string { return c.dc }

func (c *Client) queryOptions(ctx context.Context) *api.QueryOptions {
	opts := new(api.QueryOptions).WithContext(ctx)
	opts.AllowStale = c.allowStale

	return opts
}

func queryMeta(qm *api.QueryMeta) QueryMeta {
	if qm == nil {
		return QueryMeta{}
	}

	return QueryMeta{KnownLeader: qm.KnownLeader, LastContact: qm.LastContact}
}
