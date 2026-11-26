package collect

import (
	"slices"
	"time"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Snapshot is what one collection cycle read from every source. A published snapshot is
// never changed: readers share it without locks and must not modify anything it holds.
type Snapshot struct {
	// Seq numbers the snapshots of a [Collector] from 1.
	Seq uint64
	// ObserveTime is when the cycle finished.
	ObserveTime   time.Time
	CycleDuration time.Duration
	// Observer is the instance that collected the snapshot.
	Observer Instance
	Profile  config.Profile
	// Voters holds the coordination keys of every voter: one per datacenter in
	// warm_standby, a single one in stretch.
	Voters []Voter
	// Arbiter is nil when no arbiter is configured, and never nil when one is.
	Arbiter *Source[Arbiter]
	// Datacenters are in name order.
	Datacenters []Datacenter
	// Clusters are the Patroni clusters: one per datacenter in warm_standby, a single one
	// over all datacenters in stretch.
	Clusters []Cluster
}

// Instance is a dcctl instance.
type Instance struct {
	Datacenter string
	// Node is the node name of the local Consul agent, or the hostname when the agent did
	// not give it.
	Node string
}

// Voter is the Consul KV of one voter.
type Voter struct {
	Datacenter string
	KV         Source[KV]
}

// KV holds the coordination keys of a voter; a nil entry is a key that does not exist.
type KV struct {
	State *consul.KVEntry
	Lease *consul.KVEntry
	Op    *consul.KVEntry
	Lock  *consul.KVEntry
	// Meta is how current the reads are: KnownLeader only when every server that answered
	// knew its leader, and the longest LastContact. A Consul without a leader, such as the
	// minority side of a partition, still answers, with KnownLeader false. Its Endpoint is
	// the server that answered the first read.
	Meta consul.QueryMeta
}

// Arbiter holds the records of the arbiter; a nil record does not exist.
type Arbiter struct {
	// Initialized is false when the arbiter reports it was never initialized.
	Initialized bool
	State       *arbiter.Record
	Lease       *arbiter.Record
}

// Datacenter is what was read from the sources of one datacenter.
type Datacenter struct {
	Name     string
	Services Source[Services]
	Nomad    Source[Nomad]
}

// Services are the instances of the services of a datacenter in the Consul catalog, with
// every check status. In stretch they are only those on nodes of the datacenter.
type Services struct {
	// Nomad is the service nomad, or nomad-<dc> in stretch.
	Nomad []consul.ServiceNode
	// Dcctl is the service webitel-dcctl.
	Dcctl []consul.ServiceNode
	// RabbitMQ is the service of status.rabbitmq_service for the datacenter.
	RabbitMQ []consul.ServiceNode
	// Info maps every service of status.info_services to its instances.
	Info map[string][]consul.ServiceNode
	// Meta is how current the reads are, as in [KV].
	Meta consul.QueryMeta
}

// Instances are the instances of a service, or the result of a prepared query.
type Instances struct {
	Nodes []consul.ServiceNode
	Meta  consul.QueryMeta
}

// Nomad is what was read from the Nomad cluster of a datacenter.
type Nomad struct {
	// Endpoint is the URL of the server that answered the leader read.
	Endpoint string
	// Leader is the RPC address of the leader; empty when that server knows none.
	Leader string
	// Version is the version of the server that answered.
	Version   string
	Managed   []nomad.Job
	Unmanaged []nomad.Job
	// Allocs maps the ID of every managed job to its allocations.
	Allocs map[string][]nomad.Alloc
	Nodes  []nomad.Node
}

// Cluster is one Patroni cluster with what the Consul catalog knows about it.
type Cluster struct {
	// Datacenters are the datacenters the cluster spans.
	Datacenters []string
	// Scope is every instance of the Patroni scope service, with its role tags and
	// node_meta, not filtered by datacenter.
	Scope Source[Instances]
	// API is the result of the prepared query webitel-api.
	API     Source[Instances]
	Patroni Source[Patroni]
}

// Patroni is the state of the members of a cluster.
type Patroni struct {
	// Endpoint is the URL of the member whose GET /cluster gave Members.
	Endpoint string
	Members  []Member
	// Leader is the name of the member with the cluster role leader or standby_leader;
	// empty when there is none.
	Leader string
	// Config is GET /config of the leader and History is GET /history of the leader. They
	// fail with reason no_endpoints when there is no leader to ask; when the members
	// cannot be read, they fail with the same error.
	Config  Source[map[string]any]
	History Source[[]patroni.HistoryEntry]
}

// Member is a Patroni member with the datacenter it runs in.
type Member struct {
	patroni.Member

	// Datacenter is empty when no instance of the scope service runs on the host of the
	// member, or its node has no node_meta with the datacenter.
	Datacenter string
}

// Datacenter returns the datacenter named name.
func (s *Snapshot) Datacenter(name string) (*Datacenter, bool) {
	i := slices.IndexFunc(s.Datacenters, func(d Datacenter) bool { return d.Name == name })
	if i < 0 {
		return nil, false
	}

	return &s.Datacenters[i], true
}

// ClusterOf returns the Patroni cluster that spans datacenter dc.
func (s *Snapshot) ClusterOf(dc string) (*Cluster, bool) {
	i := slices.IndexFunc(s.Clusters, func(c Cluster) bool { return slices.Contains(c.Datacenters, dc) })
	if i < 0 {
		return nil, false
	}

	return &s.Clusters[i], true
}

// leastCurrent folds the meta of several reads of one source: KnownLeader only when every
// server that answered knew its leader, the longest LastContact, and the Endpoint of the
// first read.
func leastCurrent(metas ...consul.QueryMeta) consul.QueryMeta {
	out := consul.QueryMeta{KnownLeader: len(metas) > 0}
	if len(metas) > 0 {
		out.Endpoint = metas[0].Endpoint
	}

	for _, m := range metas {
		out.KnownLeader = out.KnownLeader && m.KnownLeader
		out.LastContact = max(out.LastContact, m.LastContact)
	}

	return out
}
