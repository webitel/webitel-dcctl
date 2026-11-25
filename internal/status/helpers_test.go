package status_test

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

const (
	dcA = "dc_a"
	dcB = "dc_b"

	nodeMetaKey = "datacenter"
	minHealthy  = 10 * time.Second
	maxLag      = 1 << 20
)

var (
	now      = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	observed = now.Add(-time.Second)
	lost     = now.Add(-time.Minute)
	errDown  = errors.New("connection refused")
)

func testConfig() status.Config {
	return status.Config{
		Interval:           5 * time.Second,
		NodeMetaKey:        nodeMetaKey,
		MinHealthyTime:     minHealthy,
		PlannedMaxLagBytes: maxLag,
		RabbitMQService:    "rabbitmq",
		InfoServices:       []string{"engine", "opensips"},
	}
}

func up[T any](data T) collect.Source[T] {
	return collect.Source[T]{Reachable: true, Data: data, ObserveTime: observed}
}

// unreachable turns s into a source that stopped answering and keeps its last data.
func unreachable[T any](s collect.Source[T]) collect.Source[T] {
	s.Reachable = false
	s.Reason = source.ReasonRefused
	s.Err = errDown
	s.UnreachableSince = lost
	s.ObserveTime = lost

	return s
}

func i64(v int64) *int64 { return &v }

func entry(key string, v any) *consul.KVEntry {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return &consul.KVEntry{Key: key, Value: b, ModifyIndex: 10}
}

func stateEntry(generation int64, active string, needsRecovery ...string) *consul.KVEntry {
	return entry(kv.StateKey, kv.State{
		Generation: generation, ActiveDatacenter: active, OpID: "op-1", PatroniTimeline: 3,
		NeedsRecovery: needsRecovery, UpdatedAt: lost,
	})
}

func leaseEntry(holder string, generation, counter int64) *consul.KVEntry {
	return entry(kv.LeaseKey, kv.Lease{Holder: holder, Generation: generation, Counter: counter, Fencing: true})
}

func kvVoter(dc string, state *consul.KVEntry) collect.Voter {
	return collect.Voter{Datacenter: dc, KV: up(collect.KV{
		State: state, Lease: leaseEntry(dcA, 5, 7), Meta: consul.QueryMeta{KnownLeader: true},
	})}
}

func arbiterRecord(v any) *arbiter.Record {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return &arbiter.Record{Value: b, Etag: 3}
}

func instance(service, dc string, passing bool) consul.ServiceNode {
	st := "critical"
	if passing {
		st = "passing"
	}

	return consul.ServiceNode{
		Service: service, Node: service + "-" + dc, NodeAddress: "10.1.0.1",
		NodeMeta: map[string]string{nodeMetaKey: dc}, Status: st,
	}
}

func services(dc string) collect.Services {
	return collect.Services{
		RabbitMQ: []consul.ServiceNode{instance("rabbitmq", dc, true)},
		Info: map[string][]consul.ServiceNode{
			"engine":   {instance("engine", dc, true)},
			"opensips": {instance("opensips", dc, true), instance("opensips", dc, false)},
		},
		Meta: consul.QueryMeta{KnownLeader: true},
	}
}

func readyNode(id string, services ...string) nomad.Node {
	return nomad.Node{ID: id, Name: id, Status: "ready", SchedulingEligibility: "eligible", Services: services}
}

func healthyAlloc(job, node string) nomad.Alloc {
	return nomad.Alloc{
		ID: job + "-" + node, JobID: job, NodeID: node, ClientStatus: "running", DesiredStatus: "run",
		TaskStates: map[string]nomad.TaskState{"main": {State: "running", StartedAt: now.Add(-time.Hour)}},
	}
}

// runningNomad has the managed jobs engine (service, stage 2) and freeswitch (system,
// stage 1) healthy, and one unmanaged job.
func runningNomad() collect.Nomad {
	return collect.Nomad{
		Version: "1.9.0",
		Managed: []nomad.Job{
			{ID: "engine", Name: "engine", Type: "service", Stage: 2, JobModifyIndex: 12},
			{ID: "freeswitch", Name: "freeswitch", Type: "system", Stage: 1, JobModifyIndex: 11},
		},
		Unmanaged: []nomad.Job{{ID: "consul-esm", Name: "consul-esm", Type: "service", Status: "running"}},
		Allocs: map[string][]nomad.Alloc{
			"engine":     {healthyAlloc("engine", "n1")},
			"freeswitch": {healthyAlloc("freeswitch", "n1")},
		},
		Nodes: []nomad.Node{readyNode("n1", "freeswitch", "engine")},
	}
}

// stoppedNomad is runningNomad with every managed job stopped.
func stoppedNomad() collect.Nomad {
	n := runningNomad()
	n.Managed = []nomad.Job{
		{ID: "engine", Name: "engine", Type: "service", Stage: 2, Stop: true, JobModifyIndex: 12},
		{ID: "freeswitch", Name: "freeswitch", Type: "system", Stage: 1, Stop: true, JobModifyIndex: 11},
	}
	n.Allocs = map[string][]nomad.Alloc{"engine": nil, "freeswitch": nil}

	return n
}

func member(name, host, dc string, role patroni.ClusterRole, state string, probe *patroni.State) collect.Member {
	m := collect.Member{
		Member: patroni.Member{
			Name: name, Host: host, APIURL: "https://" + host + ":8008/patroni", Endpoint: host + ":8008",
			Role: role, State: state, Timeline: i64(3), FailoverPriority: 1, Probe: probe, Usable: probe != nil,
		},
		Datacenter: dc,
	}

	if role != patroni.ClusterRoleLeader && role != patroni.ClusterRoleStandbyLeader {
		m.Lag = i64(0)
	}

	if probe == nil {
		m.UnusableReason = patroni.UnusableUnreachable
		m.Err = &source.Error{Endpoint: m.Endpoint, Reason: source.ReasonTimeout, Err: errDown}
	}

	return m
}

func probe(role patroni.Role, state, replication string, xlog patroni.Xlog) *patroni.State {
	return &patroni.State{
		Role: role, State: state, Timeline: i64(3), Xlog: xlog, ReplicationState: replication, Version: "4.1.0",
	}
}

func scopeOf(dc string, hosts ...string) collect.Instances {
	var in collect.Instances

	for _, h := range hosts {
		in.Nodes = append(in.Nodes, consul.ServiceNode{
			Service: "pg", Address: h, NodeAddress: h, NodeMeta: map[string]string{nodeMetaKey: dc}, Status: "passing",
		})
	}

	return in
}

func api(dcs ...string) collect.Instances {
	var in collect.Instances

	for _, dc := range dcs {
		in.Nodes = append(in.Nodes, instance("webitel-api", dc, true))
	}

	return in
}

func history(rows ...patroni.HistoryEntry) collect.Source[[]patroni.HistoryEntry] {
	return up(rows)
}

// warm returns a healthy warm_standby: dc_a active with the primary and its apps
// running, dc_b a standby that streams from dc_a with its apps stopped, an arbiter.
func warm() *collect.Snapshot {
	state := stateEntry(5, dcA)

	primary := probe(patroni.RolePrimary, "running", "", patroni.Xlog{Location: i64(5000)})
	replicaA := probe(patroni.RoleReplica, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(5000)})
	standby := probe(patroni.RoleStandbyLeader, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(4900)})
	replicaB := probe(patroni.RoleReplica, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(4900)})

	return &collect.Snapshot{
		Seq:           9,
		ObserveTime:   observed,
		CycleDuration: 800 * time.Millisecond,
		Observer:      collect.Instance{Datacenter: dcA, Node: "dcctl-a"},
		Profile:       config.ProfileWarmStandby,
		Voters:        []collect.Voter{kvVoter(dcA, state), kvVoter(dcB, state)},
		Arbiter: new(up(collect.Arbiter{
			Initialized: true,
			State:       arbiterRecord(kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "op-1", PatroniTimeline: 3}),
			Lease:       arbiterRecord(kv.Lease{Holder: dcA, Generation: 5, Counter: 6}),
		})),
		Datacenters: []collect.Datacenter{
			{Name: dcA, Services: up(services(dcA)), Nomad: up(runningNomad())},
			{Name: dcB, Services: up(services(dcB)), Nomad: up(stoppedNomad())},
		},
		Clusters: []collect.Cluster{
			{
				Datacenters: []string{dcA},
				Scope:       up(scopeOf(dcA, "10.0.0.1", "10.0.0.2")),
				API:         up(api(dcA)),
				Patroni: up(collect.Patroni{
					Leader: "pg-a1",
					Members: []collect.Member{
						member("pg-a1", "10.0.0.1", dcA, patroni.ClusterRoleLeader, "running", primary),
						member("pg-a2", "10.0.0.2", dcA, patroni.ClusterRoleReplica, "streaming", replicaA),
					},
					Config: up(map[string]any{"ttl": 30}),
					History: history(
						patroni.HistoryEntry{Timeline: 1, LSN: 1000},
						patroni.HistoryEntry{Timeline: 2, LSN: 3000},
					),
				}),
			},
			{
				Datacenters: []string{dcB},
				Scope:       up(scopeOf(dcB, "10.0.1.1", "10.0.1.2")),
				API:         up(api(dcB)),
				Patroni: up(collect.Patroni{
					Leader: "pg-b1",
					Members: []collect.Member{
						member("pg-b1", "10.0.1.1", dcB, patroni.ClusterRoleStandbyLeader, "streaming", standby),
						member("pg-b2", "10.0.1.2", dcB, patroni.ClusterRoleReplica, "streaming", replicaB),
					},
					Config: up(map[string]any{"standby_cluster": map[string]any{"host": "10.0.0.1", "port": 5432}}),
					History: history(
						patroni.HistoryEntry{Timeline: 1, LSN: 1000},
						patroni.HistoryEntry{Timeline: 2, LSN: 3000},
					),
				}),
			},
		},
	}
}

// stretch returns a healthy stretch: one Patroni cluster with its leader in dc_a,
// active, and replicas in both datacenters; one global Consul voter.
func stretch() *collect.Snapshot {
	leader := probe(patroni.RolePrimary, "running", "", patroni.Xlog{Location: i64(5000)})
	replica := func(recv int64) *patroni.State {
		return probe(patroni.RoleReplica, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(recv)})
	}

	b3 := member("pg-3", "10.0.1.1", dcB, patroni.ClusterRoleReplica, "streaming", replica(4990))
	b3.Lag = i64(10)
	b4 := member("pg-4", "10.0.1.2", dcB, patroni.ClusterRoleReplica, "streaming", replica(4970))
	b4.Lag = i64(30)

	return &collect.Snapshot{
		Seq:           3,
		ObserveTime:   observed,
		CycleDuration: time.Second,
		Observer:      collect.Instance{Datacenter: dcB, Node: "dcctl-b"},
		Profile:       config.ProfileStretch,
		Voters:        []collect.Voter{kvVoter(dcA, stateEntry(5, dcA))},
		Datacenters: []collect.Datacenter{
			{Name: dcA, Services: up(services(dcA)), Nomad: up(runningNomad())},
			{Name: dcB, Services: up(services(dcB)), Nomad: up(stoppedNomad())},
		},
		Clusters: []collect.Cluster{{
			Datacenters: []string{dcA, dcB},
			Scope:       up(collect.Instances{Nodes: append(scopeOf(dcA, "10.0.0.1", "10.0.0.2").Nodes, scopeOf(dcB, "10.0.1.1", "10.0.1.2").Nodes...)}),
			API:         up(api(dcA, dcA)),
			Patroni: up(collect.Patroni{
				Leader: "pg-1",
				Members: []collect.Member{
					member("pg-1", "10.0.0.1", dcA, patroni.ClusterRoleLeader, "running", leader),
					member("pg-2", "10.0.0.2", dcA, patroni.ClusterRoleReplica, "streaming", replica(5000)),
					b3,
					b4,
				},
				Config:  up(map[string]any{"ttl": 30}),
				History: history(patroni.HistoryEntry{Timeline: 2, LSN: 3000}),
			}),
		}},
	}
}

// editPatroni applies fn to a copy of the Patroni data of the cluster spanning dc.
func editPatroni(s *collect.Snapshot, dc string, fn func(p *collect.Patroni)) {
	cl, ok := s.ClusterOf(dc)
	if !ok {
		panic("no cluster of " + dc)
	}

	p := cl.Patroni.Data
	p.Members = append([]collect.Member(nil), p.Members...)
	fn(&p)
	cl.Patroni.Data = p
}

// memberNamed returns the member named name of p.
func memberNamed(p *collect.Patroni, name string) *collect.Member {
	for i := range p.Members {
		if p.Members[i].Name == name {
			return &p.Members[i]
		}
	}

	panic("no member " + name)
}

func datacenter(t interface{ Fatalf(string, ...any) }, st status.Status, name string) status.Datacenter {
	for _, d := range st.Datacenters {
		if d.Name == name {
			return d
		}
	}

	t.Fatalf("no datacenter %s in status", name)

	return status.Datacenter{}
}
