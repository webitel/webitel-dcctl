package status

import (
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Role is the role of a Patroni member in its cluster.
type Role string

const (
	// RoleUnknown is a role that is not reported, or a datacenter without a leader.
	RoleUnknown       Role = ""
	RolePrimary       Role = "primary"
	RoleStandbyLeader Role = "standby_leader"
	RoleReplica       Role = "replica"
	RoleSyncStandby   Role = "sync_standby"
	RoleQuorumStandby Role = "quorum_standby"
)

func roleOf(r patroni.ClusterRole) Role {
	switch r {
	case patroni.ClusterRoleLeader:
		return RolePrimary
	case patroni.ClusterRoleStandbyLeader:
		return RoleStandbyLeader
	case patroni.ClusterRoleReplica:
		return RoleReplica
	case patroni.ClusterRoleSyncStandby:
		return RoleSyncStandby
	case patroni.ClusterRoleQuorumStandby:
		return RoleQuorumStandby
	}

	return RoleUnknown
}

// Patroni is the Patroni cluster of a datacenter in warm_standby, or the part of the
// cluster in it in stretch.
type Patroni struct {
	// Leader is the leader member when it runs in the datacenter.
	Leader string
	Role   Role
	// Timeline is of the leader in warm_standby and the highest of the members of the
	// datacenter in stretch; nil when not known.
	Timeline *int64
	Paused   bool
	// StandbyClusterHost is standby_cluster.host of the config, warm_standby only.
	StandbyClusterHost string
	// UpstreamDatacenter is the datacenter whose nodes StandbyClusterHost points at;
	// empty when the hosts are not all of one known datacenter.
	UpstreamDatacenter string
	// LagBytes is how far the datacenter is behind the primary of the active one,
	// approximate since the two are read apart. In stretch it is the least lag of a
	// member of the datacenter. nil when it does not apply, for the datacenter of the
	// primary, or is not known.
	LagBytes   *int64
	Divergence Divergence
	// Members are those running in the datacenter, in the order of the cluster.
	Members []Member
}

// Divergence is whether the timeline of a datacenter left the history of the active one.
type Divergence struct {
	// Known is false when a timeline, a WAL position or a history needed to tell is
	// missing; the datacenter may then have diverged.
	Known    bool
	Diverged bool
	// LSN is where the timelines diverged; Bytes is the WAL the datacenter wrote after
	// it, which a rewind discards.
	LSN   int64
	Bytes int64
	// Unverified is set for a standby whose upstream is not read: the history of the
	// active datacenter alone told it.
	Unverified bool
	// Rebuild is set, with Known false, for such a standby on the timeline the active
	// datacenter took on promotion or newer: it may follow a line only its upstream had,
	// and it needs a rebuild by hand.
	Rebuild bool
}

// Member is a Patroni member.
type Member struct {
	Name   string
	Host   string
	APIURL string
	// Datacenter is empty when it is not known where the member runs.
	Datacenter string
	// Reachable is set when GET /patroni of the member answered.
	Reachable bool
	Reason    source.Reason
	Role      Role
	// PatroniState is the state the member reports, or the one of GET /cluster when it
	// does not answer.
	PatroniState     string
	Timeline         *int64
	ReplicationState string
	Xlog             Xlog
	// LagBytes is the lag behind the leader of the cluster; nil for the leader or when
	// Patroni does not know it.
	LagBytes         *int64
	Version          string
	FailoverPriority int
	NoFailover       bool
	Usable           bool
	UnusableReason   patroni.UnusableReason
}

// Xlog holds the WAL positions of a member, in bytes; nil when not reported.
type Xlog struct {
	Location         *int64
	ReceivedLocation *int64
	ReplayedLocation *int64
	// ReplayedTimestamp is the commit time of the last replayed transaction as
	// Patroni reports it.
	ReplayedTimestamp string
}

// dcPatroni is the part of a Patroni cluster in one datacenter.
type dcPatroni struct {
	// cluster is nil when no cluster spans the datacenter.
	cluster *collect.Cluster
	members []collect.Member
	// leader is the leader of the cluster when it runs in the datacenter.
	leader *collect.Member
	// clusterLeader is the leader of the cluster wherever it runs.
	clusterLeader *collect.Member
	// unplaced counts the members of the cluster in no known datacenter.
	unplaced int
	src      Source
}

func (b *builder) patroniIn(dc string) dcPatroni {
	cl, ok := b.snap.ClusterOf(dc)
	if !ok {
		return dcPatroni{src: Source{System: SystemPatroni, Reason: source.ReasonNoEndpoints}}
	}

	p := dcPatroni{cluster: cl, src: sourceOf(SystemPatroni, cl.Patroni, cl.Patroni.Data.Endpoint)}

	all := cl.Patroni.Data.Members
	for i := range all {
		m := &all[i]

		if m.Role == patroni.ClusterRoleLeader || m.Role == patroni.ClusterRoleStandbyLeader {
			if p.clusterLeader == nil {
				p.clusterLeader = m
			}
		}

		switch m.Datacenter {
		case dc:
			p.members = append(p.members, *m)
		case "":
			p.unplaced++
		}
	}

	if p.clusterLeader != nil && p.clusterLeader.Datacenter == dc {
		p.leader = p.clusterLeader
	}

	// A cluster over several datacenters may answer while every member of this one
	// does not: then Patroni of the datacenter is unreachable.
	if len(cl.Datacenters) > 1 && p.src.Reachable && len(p.members) > 0 &&
		!slices.ContainsFunc(p.members, memberReachable) {
		p.src.Reachable = false
		p.src.Reason = source.ReasonTimeout

		if r, ok := source.ReasonOf(p.members[0].Err); ok {
			p.src.Reason = r
		}
	}

	return p
}

func memberReachable(m collect.Member) bool {
	return m.Err == nil && m.Probe != nil
}

func memberTimeline(m *collect.Member) *int64 {
	if m.Probe != nil && m.Probe.Timeline != nil {
		return m.Probe.Timeline
	}

	return m.Timeline
}

func memberState(m *collect.Member) string {
	if m.Probe != nil {
		return m.Probe.State
	}

	return m.State
}

// streaming reports whether the member replicates from its upstream, by GET /patroni,
// or by GET /cluster when the member itself does not answer.
func streaming(m *collect.Member) bool {
	if m.Probe != nil {
		return m.Probe.ReplicationState == "streaming"
	}

	return m.State == "streaming"
}

// MemberOf returns the member m as the model shows it.
func MemberOf(m collect.Member) Member {
	out := Member{
		Name:             m.Name,
		Host:             m.Host,
		APIURL:           m.APIURL,
		Datacenter:       m.Datacenter,
		Reachable:        memberReachable(m),
		Role:             roleOf(m.Role),
		PatroniState:     memberState(&m),
		Timeline:         memberTimeline(&m),
		LagBytes:         m.Lag,
		FailoverPriority: m.FailoverPriority,
		NoFailover:       m.NoFailover,
		Usable:           m.Usable,
		UnusableReason:   m.UnusableReason,
	}

	if m.Err != nil {
		out.Reason, _ = source.ReasonOf(m.Err)
	}

	if p := m.Probe; p != nil {
		out.ReplicationState = p.ReplicationState
		out.Version = p.Version
		out.Xlog = Xlog{
			Location:          p.Xlog.Location,
			ReceivedLocation:  p.Xlog.ReceivedLocation,
			ReplayedLocation:  p.Xlog.ReplayedLocation,
			ReplayedTimestamp: p.Xlog.ReplayedTimestamp,
		}
	}

	return out
}

func (b *builder) patroni(dc string, p dcPatroni) Patroni {
	var out Patroni

	for _, m := range p.members {
		out.Members = append(out.Members, MemberOf(m))
	}

	if p.cluster == nil {
		return out
	}

	if l := p.leader; l != nil {
		out.Leader = l.Name
		out.Role = roleOf(l.Role)
		out.Timeline = memberTimeline(l)
	}

	for _, m := range p.cluster.Patroni.Data.Members {
		if m.Probe != nil && m.Probe.Pause {
			out.Paused = true
		}
	}

	if b.warm() {
		out.StandbyClusterHost = standbyHost(p.cluster.Patroni.Data.Config.Data)
		out.UpstreamDatacenter = b.upstream(out.StandbyClusterHost)
	} else {
		out.Timeline = nil

		for i := range p.members {
			if t := memberTimeline(&p.members[i]); t != nil && (out.Timeline == nil || *t > *out.Timeline) {
				out.Timeline = t
			}
		}
	}

	out.LagBytes = b.lag(dc, p)
	out.Divergence = b.divergence(dc, p, out.UpstreamDatacenter)

	return out
}

func standbyHost(cfg map[string]any) string {
	sc, ok := cfg["standby_cluster"].(map[string]any)
	if !ok {
		return ""
	}

	host, ok := sc["host"].(string)
	if !ok {
		return ""
	}

	return host
}

// upstream returns the datacenter of the hosts, a comma separated list, when they
// are all of one known datacenter.
func (b *builder) upstream(hosts string) string {
	var dc string

	for h := range strings.SplitSeq(hosts, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}

		d := b.hostDatacenter(h)
		if d == "" || (dc != "" && d != dc) {
			return ""
		}

		dc = d
	}

	return dc
}

// hostDatacenter finds the datacenter of a Patroni host by the members of every
// cluster and the nodes of their scope service.
func (b *builder) hostDatacenter(host string) string {
	for _, cl := range b.snap.Clusters {
		for _, m := range cl.Patroni.Data.Members {
			if m.Host == host && m.Datacenter != "" {
				return m.Datacenter
			}
		}

		for _, n := range cl.Scope.Data.Nodes {
			if n.Address != host && n.NodeAddress != host {
				continue
			}

			if dc := n.NodeMeta[b.cfg.NodeMetaKey]; dc != "" {
				return dc
			}

			if len(cl.Datacenters) == 1 {
				return cl.Datacenters[0]
			}
		}
	}

	return ""
}

func (b *builder) active() string {
	if b.effective == nil {
		return ""
	}

	return b.effective.ActiveDatacenter
}

// activePrimary returns the leader of the active datacenter when it is a primary that
// answered, read from a cluster that answered.
func (b *builder) activePrimary() *collect.Member {
	a := b.patroniIn(b.active())
	if !a.src.Reachable || a.leader == nil || a.leader.Role != patroni.ClusterRoleLeader || a.leader.Probe == nil {
		return nil
	}

	return a.leader
}

func (b *builder) lag(dc string, p dcPatroni) *int64 {
	if !p.src.Reachable {
		return nil
	}

	if !b.warm() {
		if p.leader != nil {
			return nil
		}

		var least *int64

		for _, m := range p.members {
			if m.Lag != nil && (least == nil || *m.Lag < *least) {
				least = m.Lag
			}
		}

		return least
	}

	if b.effective == nil || dc == b.active() || p.leader == nil || p.leader.Probe == nil {
		return nil
	}

	primary := b.activePrimary()
	if primary == nil {
		return nil
	}

	loc, recv := primary.Probe.Xlog.Location, p.leader.Probe.Xlog.ReceivedLocation
	if loc == nil || recv == nil {
		return nil
	}

	// The standby is read after the primary, so it may have received WAL the primary
	// wrote after its read: that is no lag, not a negative one.
	lag := max(0, *loc-*recv)

	return &lag
}

// divergence tells whether the timeline of the datacenter left the history of the
// active datacenter, see [patroni.Diverge]. Patroni keeps GET /history current on a
// primary only: a datacenter led by a primary is judged by its own history, and then
// not at all on the timeline of the active one, where two promotions apart are not told
// from each other; a standby by the history of upstream, the datacenter it streams from,
// while that one is led by a primary, and otherwise by the history of the active
// datacenter alone, unverified, below the timeline it took on promotion only.
func (b *builder) divergence(dc string, p dcPatroni, upstream string) Divergence {
	if !b.warm() || dc == b.active() {
		return Divergence{Known: true}
	}

	if b.effective == nil || !p.src.Reachable || p.leader == nil {
		return Divergence{}
	}

	a := b.patroniIn(b.active())
	if !a.src.Reachable || a.leader == nil {
		return Divergence{}
	}

	ta, td := memberTimeline(a.leader), memberTimeline(p.leader)
	if ta == nil || td == nil {
		return Divergence{}
	}

	ha, pos := history(a.cluster.Patroni.Data.History), walPosition(p.leader)

	var (
		d     patroni.Divergence
		ok    bool
		alone bool
	)

	shared := b.effective.PatroniTimeline

	switch line, read := b.line(upstream); {
	case p.leader.Probe != nil && p.leader.Probe.Role == patroni.RolePrimary:
		if *ta != *td && p.cluster.Patroni.Data.History.Reachable {
			d, ok = patroni.Diverge(*ta, ha, *td, history(p.cluster.Patroni.Data.History), pos)
		}
	case read:
		d, ok = patroni.Diverge(*ta, ha, *td, line, pos)
	default:
		d, ok = patroni.DivergeByPrimary(*ta, ha, *td, pos, shared)
		alone = true
	}

	if !ok {
		return Divergence{Rebuild: alone && pos != nil && *td >= shared && *td < *ta}
	}

	return Divergence{Known: true, Diverged: d.Diverged, LSN: d.LSN, Bytes: d.Bytes, Unverified: d.Unverified}
}

// line returns the history of the cluster of dc while it is led by a primary, which
// keeps it current; false otherwise.
func (b *builder) line(dc string) ([]patroni.HistoryEntry, bool) {
	if dc == "" {
		return nil, false
	}

	u := b.patroniIn(dc)

	if u.cluster == nil || !u.src.Reachable || u.leader == nil || u.leader.Probe == nil || u.leader.Probe.Role != patroni.RolePrimary ||
		!u.cluster.Patroni.Data.History.Reachable {
		return nil, false
	}

	return u.cluster.Patroni.Data.History.Data, true
}

// walPosition is the WAL a member holds: written by a primary, received by a replica.
func walPosition(m *collect.Member) *int64 {
	if m.Probe == nil {
		return nil
	}

	return m.Probe.WALPosition()
}

// history is the history of a cluster, none when it was not read.
func history(h collect.Source[[]patroni.HistoryEntry]) []patroni.HistoryEntry {
	if !h.Reachable {
		return nil
	}

	return h.Data
}

// detectedActive returns the datacenter of the one primary of the clusters, or ""
// when some cluster does not answer, since it may hold another primary, or there is
// none, more than one, or its datacenter is not known.
func (b *builder) detectedActive() string {
	var found []string

	for _, cl := range b.snap.Clusters {
		if !cl.Patroni.Reachable {
			return ""
		}

		for _, m := range cl.Patroni.Data.Members {
			if m.Role != patroni.ClusterRoleLeader {
				continue
			}

			if m.Datacenter == "" {
				return ""
			}

			if !slices.Contains(found, m.Datacenter) {
				found = append(found, m.Datacenter)
			}
		}
	}

	if len(found) != 1 {
		return ""
	}

	return found[0]
}
