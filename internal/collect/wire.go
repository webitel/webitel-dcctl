package collect

import (
	"context"
	"fmt"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// attemptsPerRequest is how many attempts fit in one status.request_timeout: a source
// walks its endpoints (Consul or Nomad servers) or its steps (the Patroni cluster, then its
// members) in turn, and one endpoint that never answers must not take the whole wait.
const attemptsPerRequest = 3

// Reads is how current the reads of a collector are.
type Reads int

const (
	// ReadsConsistent asks the Consul and Nomad leaders, for a snapshot an action is
	// decided on: a server cut off from its leader may show a lock free or a job stopped.
	ReadsConsistent Reads = iota
	// ReadsStale lets any server answer, also one without a leader, as the status shows
	// what the minority side of a partition sees.
	ReadsStale
)

// FromConfig builds a collector of serve. It reads through clients of its own, which
// only read: Consul and Nomad as reads tells, and Patroni without credentials, each attempt
// against one endpoint bounded by a third of status.request_timeout. arb is the arbiter of
// serve, nil when none is configured.
//
// In warm_standby every datacenter has its own Patroni cluster; in stretch one cluster
// spans all of them and its members are found in the whole Consul catalog.
func FromConfig(cfg *config.Serve, instance Instance, arb *arbiter.Client, reads Reads, log *logging.Logger) (*Collector, error) {
	attempt := cfg.Status.RequestTimeout / attemptsPerRequest

	clients, err := consul.New(consul.Options{
		Profile:     cfg.Profile,
		NodeMetaKey: cfg.Consul.NodeMetaKey,
		Servers:     cfg.Consul.Servers,
		SessionTTL:  cfg.Coordination.SessionTTL,
		LockDelay:   cfg.Coordination.LockDelay,
		Timeout:     attempt,
		AllowStale:  reads == ReadsStale,
	})
	if err != nil {
		return nil, fmt.Errorf("consul: %w", err)
	}

	var src Sources

	// A nil *arbiter.Client in the interface would not be a nil Arbiter.
	if arb != nil {
		src.Arbiter = arb
	}

	for _, v := range clients.Voters() {
		src.Voters = append(src.Voters, VoterSource{Datacenter: v.Datacenter(), KV: v})
	}

	newPatroni := func(dc string, r patroni.Resolver) (*patroni.Client, error) {
		return patroni.New(patroni.Options{
			Scope:          cfg.Patroni.Scope,
			Datacenter:     dc,
			Endpoints:      cfg.Patroni.Endpoints,
			Resolver:       r,
			CAFile:         cfg.TLS.CA,
			RequestTimeout: attempt,
		})
	}

	for _, dc := range clients.Datacenters() {
		cl, err := clients.Client(dc)
		if err != nil {
			return nil, err
		}

		nc, err := nomad.New(nomad.Options{
			Datacenter:      dc,
			Service:         nomad.ServiceName(cfg.Profile, dc),
			CAFile:          cfg.TLS.CA,
			CertFile:        cfg.TLS.Cert,
			KeyFile:         cfg.TLS.Key,
			Resolver:        clients,
			RequestTimeout:  attempt,
			ConsistentReads: reads == ReadsConsistent,
		})
		if err != nil {
			return nil, fmt.Errorf("nomad of %s: %w", dc, err)
		}

		src.Datacenters = append(src.Datacenters, DatacenterSource{Name: dc, Services: cl, Nomad: nc})

		if cfg.Profile == config.ProfileStretch {
			continue
		}

		pc, err := newPatroni(dc, clients)
		if err != nil {
			return nil, fmt.Errorf("patroni of %s: %w", dc, err)
		}

		src.Clusters = append(src.Clusters, ClusterSource{Datacenters: []string{dc}, Catalog: cl, Patroni: pc})
	}

	if cfg.Profile == config.ProfileStretch {
		voter := clients.Voters()[0]

		pc, err := newPatroni(voter.Datacenter(), clusterResolver{voter})
		if err != nil {
			return nil, fmt.Errorf("patroni: %w", err)
		}

		src.Clusters = []ClusterSource{{Datacenters: clients.Datacenters(), Catalog: voter, Patroni: pc}}
	}

	return New(Config{
		Profile:     cfg.Profile,
		Instance:    instance,
		Status:      cfg.Status,
		Scope:       cfg.Patroni.Scope,
		NodeMetaKey: cfg.Consul.NodeMetaKey,
	}, src, log), nil
}

// clusterResolver finds the Patroni members of every datacenter of a stretch cluster,
// whatever datacenter the client asks for.
type clusterResolver struct {
	catalog ClusterCatalog
}

func (r clusterResolver) HealthyAddrs(ctx context.Context, _, service, tag string) ([]string, error) {
	nodes, _, err := r.catalog.ClusterInstances(ctx, service, tag, true)
	if err != nil {
		return nil, err
	}

	addrs := make([]string, len(nodes))
	for i, n := range nodes {
		addrs[i] = n.HostPort()
	}

	return addrs, nil
}
