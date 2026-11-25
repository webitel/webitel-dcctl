package preflight

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/check"
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
	dcC = "dc_c"

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
		InfoServices:       []string{"engine"},
	}
}

func up[T any](data T) collect.Source[T] {
	return collect.Source[T]{Reachable: true, Data: data, ObserveTime: observed}
}

// down is a source that did not answer the fresh collection: it has no data.
func down[T any]() collect.Source[T] {
	return collect.Source[T]{Reason: source.ReasonRefused, Err: errDown, UnreachableSince: now}
}

func i64(v int64) *int64 { return &v }

func entry(key string, v any) *consul.KVEntry {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return &consul.KVEntry{Key: key, Value: b, ModifyIndex: 10}
}

func state(generation int64, active string, needsRecovery ...string) kv.State {
	return kv.State{
		Generation: generation, ActiveDatacenter: active, OpID: "op-1", PatroniTimeline: 3,
		NeedsRecovery: needsRecovery, UpdatedAt: lost,
	}
}

func kvVoter(dc string, st kv.State) collect.Voter {
	return collect.Voter{Datacenter: dc, KV: up(collect.KV{
		State: entry(kv.StateKey, st), Meta: consul.QueryMeta{KnownLeader: true},
	})}
}

func arbiterRecord(v any) *arbiter.Record {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return &arbiter.Record{Value: b, Etag: 3}
}

func arbiterWith(st kv.State) *collect.Source[collect.Arbiter] {
	return new(up(collect.Arbiter{Initialized: true, State: arbiterRecord(st)}))
}

func instance(service, node, dc string, passing bool) consul.ServiceNode {
	st := "critical"
	if passing {
		st = "passing"
	}

	return consul.ServiceNode{
		Service: service, Node: node, NodeAddress: "10.9.0.1", Port: 10023,
		NodeMeta: map[string]string{nodeMetaKey: dc}, Status: st,
	}
}

func services(dc string) collect.Services {
	return collect.Services{
		Dcctl:    []consul.ServiceNode{instance("webitel-dcctl", "dcctl-"+dc, dc, true)},
		RabbitMQ: []consul.ServiceNode{instance("rabbitmq", "rabbit-"+dc, dc, true)},
		Info:     map[string][]consul.ServiceNode{"engine": {instance("engine", "engine-"+dc, dc, true)}},
		Meta:     consul.QueryMeta{KnownLeader: true},
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
// stage 1) healthy.
func runningNomad() collect.Nomad {
	return collect.Nomad{
		Version: "1.9.0",
		Managed: []nomad.Job{
			{ID: "engine", Name: "engine", Type: "service", Stage: 2, JobModifyIndex: 12},
			{ID: "freeswitch", Name: "freeswitch", Type: "system", Stage: 1, JobModifyIndex: 11},
		},
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
		Role: role, State: state, Timeline: i64(3), Xlog: xlog, ReplicationState: replication, Version: "4.1.5",
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

func history() collect.Source[[]patroni.HistoryEntry] {
	return up([]patroni.HistoryEntry{{Timeline: 1, LSN: 1000}, {Timeline: 2, LSN: 3000}})
}

// standbyCluster is a warm_standby cluster of dc whose leader streams from the
// primary at 10.0.0.1, received up to recv.
func standbyCluster(dc, net string, recv int64) collect.Cluster {
	leader := probe(patroni.RoleStandbyLeader, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(recv)})
	replica := probe(patroni.RoleReplica, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(recv)})

	return collect.Cluster{
		Datacenters: []string{dc},
		Scope:       up(scopeOf(dc, net+".1", net+".2")),
		Patroni: up(collect.Patroni{
			Leader: "pg-" + dc + "-1",
			Members: []collect.Member{
				member("pg-"+dc+"-1", net+".1", dc, patroni.ClusterRoleStandbyLeader, "streaming", leader),
				member("pg-"+dc+"-2", net+".2", dc, patroni.ClusterRoleReplica, "streaming", replica),
			},
			Config:  up(map[string]any{"standby_cluster": map[string]any{"host": "10.0.0.1,10.0.0.2", "port": float64(5432)}}),
			History: history(),
		}),
	}
}

// warm returns a healthy warm_standby: dc_a active with the primary and its apps
// running, dc_b a standby that streams from dc_a with its apps stopped, an arbiter.
// The observer is the instance of dc_b, which leads a switch to dc_b.
func warm() *collect.Snapshot {
	st := state(5, dcA)

	primary := probe(patroni.RolePrimary, "running", "", patroni.Xlog{Location: i64(5000)})
	primary.Replication = []patroni.Replica{{ClientAddr: "10.0.1.1", State: "streaming", SyncState: "async"}}
	replicaA := probe(patroni.RoleReplica, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(5000)})

	return &collect.Snapshot{
		ObserveTime: observed,
		Observer:    collect.Instance{Datacenter: dcB, Node: "dcctl-" + dcB},
		Profile:     config.ProfileWarmStandby,
		Voters:      []collect.Voter{kvVoter(dcA, st), kvVoter(dcB, st)},
		Arbiter:     arbiterWith(st),
		Datacenters: []collect.Datacenter{
			{Name: dcA, Services: up(services(dcA)), Nomad: up(runningNomad())},
			{Name: dcB, Services: up(services(dcB)), Nomad: up(stoppedNomad())},
		},
		Clusters: []collect.Cluster{
			{
				Datacenters: []string{dcA},
				Scope:       up(scopeOf(dcA, "10.0.0.1", "10.0.0.2")),
				Patroni: up(collect.Patroni{
					Leader: "pg-a1",
					Members: []collect.Member{
						member("pg-a1", "10.0.0.1", dcA, patroni.ClusterRoleLeader, "running", primary),
						member("pg-a2", "10.0.0.2", dcA, patroni.ClusterRoleReplica, "streaming", replicaA),
					},
					Config:  up(map[string]any{"ttl": float64(30)}),
					History: history(),
				}),
			},
			standbyCluster(dcB, "10.0.1", 4900),
		},
	}
}

// withC adds dc_c, a standby of warm that streams from dc_a, received up to recv.
func withC(s *collect.Snapshot, recv int64) *collect.Snapshot {
	st := state(5, dcA)
	s.Voters = append(s.Voters, kvVoter(dcC, st))
	s.Datacenters = append(s.Datacenters, collect.Datacenter{Name: dcC, Services: up(services(dcC)), Nomad: up(stoppedNomad())})
	s.Clusters = append(s.Clusters, standbyCluster(dcC, "10.0.2", recv))

	editPatroni(s, dcA, func(p *collect.Patroni) {
		m := memberNamed(p, "pg-a1")
		pr := *m.Probe
		pr.Replication = append(slices.Clone(pr.Replication), patroni.Replica{ClientAddr: "10.0.2.1", State: "streaming"})
		m.Probe = &pr
	})

	return s
}

// stretch returns a healthy stretch: one Patroni cluster with its leader in dc_a,
// active, and replicas in both datacenters; one global Consul voter. The observer is
// the instance of dc_b.
func stretch() *collect.Snapshot {
	leader := probe(patroni.RolePrimary, "running", "", patroni.Xlog{Location: i64(5000)})
	replica := func(recv int64) *patroni.State {
		return probe(patroni.RoleReplica, "streaming", "streaming", patroni.Xlog{ReceivedLocation: i64(recv)})
	}

	b3 := member("pg-3", "10.0.1.1", dcB, patroni.ClusterRoleReplica, "streaming", replica(4990))
	b3.Lag = i64(10)
	b4 := member("pg-4", "10.0.1.2", dcB, patroni.ClusterRoleReplica, "streaming", replica(4970))
	b4.Lag = i64(30)
	b4.FailoverPriority = 2

	return &collect.Snapshot{
		ObserveTime: observed,
		Observer:    collect.Instance{Datacenter: dcB, Node: "dcctl-" + dcB},
		Profile:     config.ProfileStretch,
		Voters:      []collect.Voter{kvVoter(dcA, state(5, dcA))},
		Datacenters: []collect.Datacenter{
			{Name: dcA, Services: up(services(dcA)), Nomad: up(runningNomad())},
			{Name: dcB, Services: up(services(dcB)), Nomad: up(stoppedNomad())},
		},
		Clusters: []collect.Cluster{{
			Datacenters: []string{dcA, dcB},
			Scope: up(collect.Instances{Nodes: append(scopeOf(dcA, "10.0.0.1", "10.0.0.2").Nodes,
				scopeOf(dcB, "10.0.1.1", "10.0.1.2").Nodes...)}),
			Patroni: up(collect.Patroni{
				Leader: "pg-1",
				Members: []collect.Member{
					member("pg-1", "10.0.0.1", dcA, patroni.ClusterRoleLeader, "running", leader),
					member("pg-2", "10.0.0.2", dcA, patroni.ClusterRoleReplica, "streaming", replica(5000)),
					b3,
					b4,
				},
				Config:  up(map[string]any{"ttl": float64(30)}),
				History: up([]patroni.HistoryEntry{{Timeline: 2, LSN: 3000}}),
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
	p.Members = slices.Clone(p.Members)
	fn(&p)
	cl.Patroni.Data = p
}

func memberNamed(p *collect.Patroni, name string) *collect.Member {
	for i := range p.Members {
		if p.Members[i].Name == name {
			return &p.Members[i]
		}
	}

	panic("no member " + name)
}

// cut makes every source of dc unreachable, with no data.
func cut(s *collect.Snapshot, dc string) {
	d, _ := s.Datacenter(dc)
	d.Services = down[collect.Services]()
	d.Nomad = down[collect.Nomad]()

	for i := range s.Voters {
		if s.Voters[i].Datacenter == dc {
			s.Voters[i].KV = down[collect.KV]()
		}
	}

	if s.Profile == config.ProfileWarmStandby {
		cl, _ := s.ClusterOf(dc)
		cl.Scope = down[collect.Instances]()
		cl.Patroni = down[collect.Patroni]()
		cl.Patroni.Data.Config = down[map[string]any]()
		cl.Patroni.Data.History = down[[]patroni.HistoryEntry]()
	}
}

func setNomad(s *collect.Snapshot, dc string, fn func(n *collect.Nomad)) {
	d, _ := s.Datacenter(dc)
	n := d.Nomad.Data
	n.Managed = slices.Clone(n.Managed)
	fn(&n)
	d.Nomad.Data = n
}

// okPlans plans every managed job of every datacenter without failures.
func okPlans(s *collect.Snapshot) map[string]map[string]Plan {
	out := make(map[string]map[string]Plan)

	for _, d := range s.Datacenters {
		out[d.Name] = make(map[string]Plan)
		for _, j := range d.Nomad.Data.Managed {
			out[d.Name][j.ID] = Plan{}
		}
	}

	return out
}

func newInput(s *collect.Snapshot, action Action, p Params) *Input {
	return &Input{
		Action: action, Params: p, Snapshot: s, Status: status.Build(s, testConfig(), now),
		Config: testConfig(), Plans: okPlans(s),
	}
}

func operation(id string, st dcctlv1.Operation_State, leaderDC, target string) *dcctlv1.Operation {
	return &dcctlv1.Operation{
		Name: "operations/" + id, Type: dcctlv1.Operation_SWITCH, State: st, Target: "datacenters/" + target,
		Leader:     &dcctlv1.Instance{Node: "dcctl-" + leaderDC, Datacenter: leaderDC},
		CreateTime: timestamppb.New(lost), UpdateTime: timestamppb.New(lost),
	}
}

func setOp(t *testing.T, s *collect.Snapshot, op *dcctlv1.Operation) {
	t.Helper()

	b, err := kv.EncodeProto(op)
	require.NoError(t, err)

	for i := range s.Voters {
		s.Voters[i].KV.Data.Op = &consul.KVEntry{Key: kv.OpKey, Value: b, ModifyIndex: 20}
	}
}

// find returns the check id about dc among cs.
func find(t *testing.T, cs []check.Check, id check.ID, dc string) check.Check {
	t.Helper()

	for _, c := range cs {
		if c.ID == id && c.Datacenter == dc {
			return c
		}
	}

	t.Fatalf("no check %s about %q", id, dc)

	return check.Check{}
}

// requireStruct asserts that facts go into google.protobuf.Struct, as the API maps them.
func requireStruct(t *testing.T, cs ...check.Check) {
	t.Helper()

	for _, c := range cs {
		_, err := structpb.NewStruct(c.Facts)
		require.NoError(t, err, "facts of %s go into google.protobuf.Struct", c.ID)
	}
}
