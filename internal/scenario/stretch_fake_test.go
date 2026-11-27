package scenario_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// pgMember is a member of the cluster of stretch.
type pgMember struct {
	name, dc string
	role     patroni.Role
	timeline int64
	// lag is behind the last position the leader reported to the DCS; nil when Patroni
	// does not know it. received is the WAL the member received; when nil it is 10000
	// less lag, unknown without lag.
	lag       *int64
	received  *int64
	streaming bool
	// timelineUnknown makes the member report no timeline.
	timelineUnknown bool
	// unread makes the member not answer GET /patroni.
	unread   bool
	priority int
}

// stretchCluster is the one Patroni cluster of stretch.
type stretchCluster struct {
	members []*pgMember
	// leader is the member that leads; empty when none does.
	leader string
	// maxLag is maximum_lag_on_failover; nil when the config is not read.
	maxLag *int64
	// consulPrimaries replaces the healthy primary instances in Consul when set.
	consulPrimaries map[string][]string
	// missing are the hosts of instances of a datacenter that are not members, by
	// datacenter.
	missing map[string][]string
}

func (c *stretchCluster) member(name string) *pgMember {
	i := slices.IndexFunc(c.members, func(m *pgMember) bool { return m.name == name })
	if i < 0 {
		panic("no member " + name)
	}

	return c.members[i]
}

func lagOf(n int64) *int64 { return &n }

// healthyCluster is the cluster of stretch with its leader a1 in dc_a on timeline 5;
// every other member streams. b1 in dc_b received the most WAL of dc_b.
func healthyCluster() *stretchCluster {
	replica := func(name, dc string, lag int64, priority int) *pgMember {
		return &pgMember{name: name, dc: dc, role: patroni.RoleReplica, timeline: 5, lag: lagOf(lag), streaming: true, priority: priority}
	}

	return &stretchCluster{
		members: []*pgMember{
			{name: "a1", dc: dcA, role: patroni.RolePrimary, timeline: 5, priority: 1},
			replica("a2", dcA, 0, 1),
			replica("b1", dcB, 0, 1),
			replica("b2", dcB, 20, 2),
			replica("c1", dcC, 0, 1),
		},
		leader: "a1",
		maxLag: lagOf(1 << 20),
	}
}

// loseLeader makes the cluster lose a1 with dc_a: Patroni of dc_a does not answer and
// nobody leads.
func loseLeader(c *stretchCluster) {
	c.leader = ""

	for _, m := range c.members {
		if m.dc == dcA {
			m.unread = true
		}
	}

	for _, m := range c.members {
		m.streaming = false
	}
}

// newStretchWorld is newWorld in stretch: one Patroni cluster over the datacenters and
// one global Consul, the only KV and voter.
func newStretchWorld(t *testing.T) *world {
	t.Helper()

	w := newWorld(t)
	base := kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 5}

	w.stretch = true
	w.infra.cluster = healthyCluster()
	w.consuls = map[string]*fakeConsul{coord.StretchName: newFakeConsul()}
	w.kvs, w.opKV = []string{coord.StretchName}, coord.StretchName
	w.voters = []*fakeVoter{newVoter(t, coord.StretchName, coord.KindKV, base)}
	w.useVoters(w.voters[0])

	return w
}

// pg changes the cluster of stretch.
func (w *infra) pg(fn func(c *stretchCluster)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	fn(w.cluster)
}

// pgMember returns a copy of the member called name.
func (w *infra) pgMember(name string) pgMember {
	w.mu.Lock()
	defer w.mu.Unlock()

	return *w.cluster.member(name)
}

func (w *infra) pgLeader() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.cluster.leader
}

func (m *pgMember) patroni(leader string) patroni.Member {
	var tl *int64
	if !m.timelineUnknown {
		tl = lagOf(m.timeline)
	}

	out := patroni.Member{
		Name: m.name, Host: m.name, Endpoint: m.name + ":8008", Role: patroni.ClusterRoleReplica, Timeline: tl,
		Lag: m.lag, FailoverPriority: m.priority, Usable: !m.unread,
	}

	if m.name == leader {
		out.Role = patroni.ClusterRoleLeader
	}

	if m.unread {
		out.Err = errors.New("dial: connection refused")

		return out
	}

	st := &patroni.State{Role: m.role, State: "running", Timeline: tl, Version: "4.1.5"}

	if m.streaming {
		st.State, st.ReplicationState = "streaming", patroni.ReplicationStreaming
	}

	switch {
	case m.role == patroni.RolePrimary:
		st.Xlog.Location = lagOf(10000)
	case m.received != nil:
		st.Xlog.ReceivedLocation = lagOf(*m.received)
	case m.lag != nil:
		st.Xlog.ReceivedLocation = lagOf(10000 - *m.lag)
	}

	out.Probe = st

	return out
}

// Place reads the cluster of stretch as the Patroni of cl.Datacenter does; it records no
// call.
func (f fakePatroni) Place(_ context.Context, cl pgctl.Cluster, dcs []string) (pgctl.Placement, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()

	c := f.w.cluster

	switch {
	case c == nil:
		return pgctl.Placement{}, errors.New("no cluster of stretch")
	case f.w.dcs[cl.Datacenter].unread:
		return pgctl.Placement{}, fmt.Errorf("patroni cluster of %s not read", cl.Datacenter)
	}

	p := pgctl.Placement{Primaries: map[string][]string{}}

	for _, m := range c.members {
		dc := m.dc
		if !slices.Contains(dcs, dc) {
			dc = ""
		}

		p.Members = append(p.Members, pgctl.Placed{Member: m.patroni(c.leader), Datacenter: dc})

		if m.role == patroni.RolePrimary && !m.unread {
			p.Primaries[m.dc] = append(p.Primaries[m.dc], m.name)
		}
	}

	if c.consulPrimaries != nil {
		p.Primaries = c.consulPrimaries
	}

	if i := slices.IndexFunc(p.Members, func(m pgctl.Placed) bool { return m.Name == c.leader }); i >= 0 {
		p.Leader = &p.Members[i]
	}

	if c.maxLag != nil {
		p.Config = map[string]any{"maximum_lag_on_failover": float64(*c.maxLag)}
	}

	return p, nil
}

// lead makes the member name the primary of the cluster on the timeline after the
// highest one; the old leader is left a replica that does not stream yet.
func (c *stretchCluster) lead(name string) int64 {
	var tl int64
	for _, m := range c.members {
		tl = max(tl, m.timeline)
	}

	if c.leader != "" {
		old := c.member(c.leader)
		old.role, old.streaming = patroni.RoleReplica, false
	}

	m := c.member(name)
	m.role, m.timeline, m.lag, m.received, m.streaming = patroni.RolePrimary, tl+1, nil, nil, false
	c.leader = name

	return m.timeline
}

func (f fakePatroni) leaderResult(dc string) pgctl.Result {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()

	res := pgctl.Result{Datacenter: dc, Observation: pgctl.Observation{Leader: f.w.cluster.leader}}

	if l := f.w.cluster.leader; l != "" {
		tl := f.w.cluster.member(l).timeline
		res.Timeline = &tl
	}

	return res
}

func (f fakePatroni) Switchover(ctx context.Context, cl pgctl.Cluster, leader, candidate string, opts pgctl.Options) (pgctl.Result, error) {
	if err := guarded(ctx, opts.Guard, pgctl.ErrGuard); err != nil {
		return f.leaderResult(cl.Datacenter), err
	}

	if err := f.w.call(ctx, "switchover "+cl.Datacenter+" to "+candidate); err != nil {
		return f.leaderResult(cl.Datacenter), err
	}

	var err error

	f.w.pg(func(c *stretchCluster) {
		switch {
		case c.leader == candidate:
		case c.leader != leader:
			err = fmt.Errorf("%w: leader is %s, not %s", pgctl.ErrPrecondition, c.leader, leader)
		case c.member(candidate).dc != cl.Datacenter:
			err = fmt.Errorf("%w: %s does not run in %s", pgctl.ErrPrecondition, candidate, cl.Datacenter)
		default:
			c.lead(candidate)
		}
	})

	return f.leaderResult(cl.Datacenter), err
}

func (f fakePatroni) Failover(ctx context.Context, cl pgctl.Cluster, candidate string, opts pgctl.Options) (pgctl.Result, error) {
	if err := guarded(ctx, opts.Guard, pgctl.ErrGuard); err != nil {
		return f.leaderResult(cl.Datacenter), err
	}

	if err := f.w.call(ctx, "failover "+cl.Datacenter+" to "+candidate); err != nil {
		return f.leaderResult(cl.Datacenter), err
	}

	var err error

	f.w.pg(func(c *stretchCluster) {
		if c.leader != "" {
			err = fmt.Errorf("%w: %s leads the cluster", pgctl.ErrPrecondition, c.leader)

			return
		}

		c.lead(candidate)
	})

	return f.leaderResult(cl.Datacenter), err
}

// Rejoin makes the members of cl.Datacenter that answer stream on the timeline of the
// leader, as Patroni does after the rewind; missing are hosts of the datacenter the
// cluster does not list.
func (f fakePatroni) Rejoin(ctx context.Context, cl pgctl.Cluster, _ pgctl.Options) (pgctl.Result, error) {
	res := pgctl.Result{Datacenter: cl.Datacenter, Action: pgctl.ActionRejoin}

	if err := f.w.call(ctx, "rejoin "+cl.Datacenter); err != nil {
		return res, err
	}

	var err error

	f.w.pg(func(c *stretchCluster) {
		res.Missing = c.missing[cl.Datacenter]

		if c.leader == "" || c.member(c.leader).dc == cl.Datacenter {
			err = fmt.Errorf("%w: the leader does not run elsewhere", pgctl.ErrTimeout)

			return
		}

		for _, m := range c.members {
			switch {
			case m.dc != cl.Datacenter:
			case m.unread:
				res.Unread = append(res.Unread, m.name)
			default:
				m.role, m.streaming, m.timeline, m.lag, m.received = patroni.RoleReplica, true, c.member(c.leader).timeline, lagOf(0), nil
			}
		}
	})

	return res, err
}

// stretchChecks decides the confirmations as the sets of stretch do: a failover always
// needs the isolation, and max_discard_bytes for the WAL a failover without a leader
// discards, by the functions of the preflight. A resume needs the isolation
// when it writes forced; one of a failover whose cluster did not fail over yet needs
// both anew. A switch takes none.
func (f fakePreflight) stretchChecks(ctx context.Context, req preflight.Request) (check.Verdict, error) {
	var cs []check.Check

	switch req.Action {
	case preflight.Switch:
	case preflight.Failover:
		cs = append(cs, isolationCheck(), f.candidateLoss(ctx, req.Params.Datacenter))
	case preflight.Resume:
		target, pending := notFailedOver(f.op())
		if req.Params.ToForced || pending {
			cs = append(cs, isolationCheck())
		}

		if pending {
			cs = append(cs, f.candidateLoss(ctx, target))
		}
	case preflight.Init, preflight.AppsSync:
		more, err := f.installationChecks(ctx, req)
		if err != nil {
			return check.Verdict{}, err
		}

		cs = append(cs, more...)
	case preflight.Demote, preflight.Repoint, preflight.MarkRecovered, preflight.Abandon, preflight.Resolve,
		preflight.ArbiterInit, preflight.Verify:
		return check.Verdict{}, fmt.Errorf("unexpected action %s", req.Action)
	}

	return check.Decide(cs, req.Confirmations), nil
}

// notFailedOver returns the target of op when op is a failover whose step of Patroni did
// not succeed or was not skipped.
func notFailedOver(op *dcctlv1.Operation) (string, bool) {
	if op.GetType() != dcctlv1.Operation_FAILOVER {
		return "", false
	}

	for _, st := range op.GetSteps() {
		if st.GetType() == dcctlv1.Operation_Step_PATRONI_FAILOVER {
			done := st.GetState() == dcctlv1.Operation_Step_SUCCEEDED || st.GetState() == dcctlv1.Operation_Step_SKIPPED

			return strings.TrimPrefix(op.GetTarget(), "datacenters/"), !done
		}
	}

	return "", false
}

// candidateLoss is patroni.candidate_loss of a failover to b.
func (f fakePreflight) candidateLoss(ctx context.Context, b string) check.Check {
	c := check.Check{
		ID: check.PatroniCandidateLoss, Result: check.Fail, Effect: check.Confirm, Confirmation: check.ConfirmMaxDiscardBytes,
		Datacenter: b,
	}

	p, err := fakePatroni{f.w}.Place(ctx, pgctl.Cluster{Datacenter: b}, f.w.names())

	switch {
	case err != nil:
		return c
	case p.Leader != nil:
		c.Result = check.Skip

		return c
	}

	members := make([]status.Member, 0, len(p.Members))
	for _, m := range p.Members {
		members = append(members, status.MemberOf(collect.Member{Member: m.Member, Datacenter: m.Datacenter}))
	}

	l, err := preflight.FailoverLoss(members, b)

	switch {
	case err != nil:
	case l.Bytes > 0:
		c.DiscardBytes = &l.Bytes
	default:
		c.Result = check.Pass
	}

	return c
}
