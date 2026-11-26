package collect

import (
	"context"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// KVReader reads the Consul KV of a voter; [consul.Client] is one.
type KVReader interface {
	// KVGet returns the key, or [consul.ErrKeyNotFound].
	KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error)
}

// ServiceReader lists the instances of a service in one datacenter; [consul.Client] is one.
type ServiceReader interface {
	Instances(ctx context.Context, service, tag string, passingOnly bool) ([]consul.ServiceNode, consul.QueryMeta, error)
}

// ClusterCatalog reads the Consul catalog without the datacenter filter; [consul.Client] is one.
type ClusterCatalog interface {
	ClusterInstances(ctx context.Context, service, tag string, passingOnly bool) ([]consul.ServiceNode, consul.QueryMeta, error)
	ExecuteQuery(ctx context.Context, name string) ([]consul.ServiceNode, consul.QueryMeta, error)
}

// NomadReader reads the Nomad cluster of a datacenter; [nomad.Client] is one.
type NomadReader interface {
	Jobs(ctx context.Context) (managed, unmanaged []nomad.Job, err error)
	Allocs(ctx context.Context, jobID string) ([]nomad.Alloc, error)
	Nodes(ctx context.Context) ([]nomad.Node, error)
	Version(ctx context.Context) (string, error)
	Leader(ctx context.Context) (leader, endpoint string, err error)
}

// PatroniReader reads a Patroni cluster; [patroni.Client] is one.
type PatroniReader interface {
	Discover(ctx context.Context) (members []patroni.Member, endpoint string, err error)
	Config(ctx context.Context, endpoint string) (map[string]any, error)
	History(ctx context.Context, endpoint string) ([]patroni.HistoryEntry, error)
}

// ArbiterReader reads the records of the arbiter; [arbiter.Client] is one.
type ArbiterReader interface {
	// Get returns the record name, [arbiter.ErrNotFound] or [arbiter.ErrUninitialized].
	Get(ctx context.Context, name string) (arbiter.Record, error)
}

// Sources are what a [Collector] reads on every cycle.
type Sources struct {
	Voters []VoterSource
	// Arbiter is nil when no arbiter is configured.
	Arbiter     ArbiterReader
	Datacenters []DatacenterSource
	Clusters    []ClusterSource
}

// VoterSource is the Consul KV of one voter.
type VoterSource struct {
	Datacenter string
	KV         KVReader
}

// DatacenterSource is what is read per datacenter.
type DatacenterSource struct {
	Name     string
	Services ServiceReader
	Nomad    NomadReader
}

// ClusterSource is a Patroni cluster and the Consul catalog that holds its scope service.
type ClusterSource struct {
	// Datacenters are the datacenters the cluster spans.
	Datacenters []string
	Catalog     ClusterCatalog
	Patroni     PatroniReader
}
