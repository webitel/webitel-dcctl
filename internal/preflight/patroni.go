package preflight

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

const (
	// postgresPort is the port standby_cluster points at: the primitives write it, and
	// Patroni takes it when the config does not set one.
	postgresPort = 5432
	// defaultMaximumLagOnFailover is what Patroni takes when its config does not set
	// maximum_lag_on_failover.
	defaultMaximumLagOnFailover = 1 << 20
	maxPort                     = 1<<16 - 1
	// maxExactFloat is the largest integer a JSON number decoded as float64 holds exactly.
	maxExactFloat  = 1 << 53
	stateRunning   = "running"
	stateStreaming = "streaming"
)

// pgScope is the part of Patroni a check looks at: what runs in one datacenter, or the
// whole cluster of stretch.
type pgScope struct {
	// dc is the datacenter of the results; empty for the whole cluster.
	dc      string
	src     status.Source
	members []status.Member
	// leader is nil when the leader does not run in the scope or is not known.
	leader *status.Member
	// raw are the members as read, for what the model does not keep.
	raw     []collect.Member
	cluster *collect.Cluster
}

func (in *Input) pgDatacenter(dc string) pgScope {
	s := pgScope{dc: dc, src: status.Source{System: status.SystemPatroni, Reason: source.ReasonNoEndpoints}}

	d, ok := in.datacenter(dc)
	if !ok {
		return s
	}

	s.src = d.Sources[2]
	s.members = d.Patroni.Members
	s.leader = memberOf(s.members, d.Patroni.Leader)

	if cl, ok := in.Snapshot.ClusterOf(dc); ok {
		s.cluster = cl

		for _, m := range cl.Patroni.Data.Members {
			if m.Datacenter == dc {
				s.raw = append(s.raw, m)
			}
		}
	}

	return s
}

// pgCluster is the one cluster of stretch. A member in no known datacenter is not in
// any model of a datacenter and so not among its members.
func (in *Input) pgCluster() pgScope {
	s := pgScope{src: status.Source{System: status.SystemPatroni, Reason: source.ReasonNoEndpoints}}

	if len(in.Snapshot.Clusters) == 0 {
		return s
	}

	s.cluster = &in.Snapshot.Clusters[0]
	s.raw = s.cluster.Patroni.Data.Members
	s.src = status.Source{
		System: status.SystemPatroni, Reachable: s.cluster.Patroni.Reachable, Reason: s.cluster.Patroni.Reason,
	}

	for _, d := range in.Status.Datacenters {
		s.members = append(s.members, d.Patroni.Members...)
	}

	for _, d := range in.Status.Datacenters {
		if d.Patroni.Leader != "" {
			s.leader = memberOf(s.members, d.Patroni.Leader)
		}
	}

	return s
}

func memberOf(ms []status.Member, name string) *status.Member {
	if name == "" {
		return nil
	}

	i := slices.IndexFunc(ms, func(m status.Member) bool { return m.Name == name })
	if i < 0 {
		return nil
	}

	return &ms[i]
}

func (s pgScope) what() string {
	if s.dc == "" {
		return "Patroni"
	}

	return "Patroni of " + s.dc
}

func memberFacts(m status.Member) map[string]any {
	f := map[string]any{
		"name": m.Name, "host": m.Host, "datacenter": m.Datacenter, "reachable": m.Reachable,
		"role": string(m.Role), "state": m.PatroniState,
	}

	if m.Reason != "" {
		f["reason"] = string(m.Reason)
	}

	if m.Version != "" {
		f["version"] = m.Version
	}

	if m.ReplicationState != "" {
		f["replication_state"] = m.ReplicationState
	}

	if m.Timeline != nil {
		f["timeline"] = *m.Timeline
	}

	if m.LagBytes != nil {
		f["lag_bytes"] = *m.LagBytes
	}

	return f
}

func membersFacts(ms []status.Member) []any {
	return list(ms, func(m status.Member) any { return memberFacts(m) })
}

func (in *Input) membersFound(s pgScope) check.Check {
	c := block(check.PatroniMembersFound, s.dc)

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	facts := map[string]any{"members": membersFacts(s.members)}

	if len(s.members) == 0 {
		return c.fail("no Patroni member is found", facts)
	}

	return c.pass(fmt.Sprintf("%d Patroni members are found", len(s.members)), facts)
}

// reachable needs the REST API of the leader over verified TLS. A replica that does
// not answer is a warning: the operation acts on the leader.
func (in *Input) pgReachable(s pgScope) check.Check {
	c := block(check.PatroniReachable, s.dc)

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	facts := map[string]any{"members": membersFacts(s.members)}

	var down []string

	for _, m := range s.members {
		if !m.Reachable {
			down = append(down, m.Name)
		}
	}

	switch {
	case s.leader != nil && !s.leader.Reachable:
		return c.fail(fmt.Sprintf("the leader %s does not answer: %s", s.leader.Name, s.leader.Reason), facts)
	case s.leader == nil && len(down) == len(s.members):
		return c.fail("no Patroni member answers", facts)
	case len(down) > 0:
		return c.warn("members do not answer: "+strings.Join(down, ", "), facts)
	}

	return c.pass("every Patroni member answers", facts)
}

// version fails on a member that answers with a Patroni older than the operations
// need, and when the version of the leader is not known.
func (in *Input) pgVersion(s pgScope) check.Check {
	c := block(check.PatroniVersion, s.dc)

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	versions := make(map[string]any, len(s.members))

	var old []string

	for _, m := range s.members {
		versions[m.Name] = m.Version
		if m.Reachable && m.UnusableReason == patroni.UnusableVersion {
			old = append(old, m.Name)
		}
	}

	facts := map[string]any{"versions": versions, "min_version": patroni.MinVersion}

	switch {
	case len(old) > 0:
		return c.fail(fmt.Sprintf("members run Patroni older than %s: %s", patroni.MinVersion, strings.Join(old, ", ")), facts)
	case s.leader == nil || !s.leader.Reachable:
		return c.fail("the version of the leader is not known", facts)
	}

	return c.pass("every member that answers runs Patroni "+patroni.MinVersion+" or newer", facts)
}

func (in *Input) notPaused(s pgScope) check.Check {
	c := block(check.PatroniNotPaused, s.dc)

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	answered := false

	for _, m := range s.raw {
		if m.Probe == nil {
			continue
		}

		answered = true

		if m.Probe.Pause {
			return c.fail("the cluster is paused", map[string]any{"member": m.Name})
		}
	}

	if !answered {
		return c.fail("no member answers, the pause is not known", nil)
	}

	return c.pass("the cluster is not paused", nil)
}

// leaderRunning reports whether the leader runs: a primary in state running, a
// standby leader running or streaming from its upstream.
func leaderRunning(m *status.Member) bool {
	return m.PatroniState == stateRunning || (m.Role == status.RoleStandbyLeader && m.PatroniState == stateStreaming)
}

func (in *Input) hasLeader(s pgScope) check.Check {
	c := block(check.PatroniHasLeader, s.dc)

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	if s.leader == nil {
		return c.fail("there is no leader", nil)
	}

	facts := map[string]any{"leader": memberFacts(*s.leader)}

	if !leaderRunning(s.leader) {
		return c.fail(fmt.Sprintf("the leader %s is %s", s.leader.Name, s.leader.PatroniState), facts)
	}

	return c.pass("the leader "+s.leader.Name+" runs", facts)
}

// basic is the checks of Patroni every action that touches it runs on a scope.
func (in *Input) basic(s pgScope) []check.Check {
	return []check.Check{in.membersFound(s), in.pgReachable(s), in.pgVersion(s), in.notPaused(s), in.hasLeader(s)}
}

func (in *Input) singlePrimary() check.Check {
	c := block(check.PatroniSinglePrimary, "")

	var primaries, unreachable []any

	for _, d := range in.Status.Datacenters {
		if !d.Sources[2].Reachable {
			unreachable = append(unreachable, d.Name)

			continue
		}

		if d.Patroni.Role == status.RolePrimary {
			primaries = append(primaries, d.Name)
		}
	}

	facts := map[string]any{"primaries": primaries, "unreachable": unreachable}

	switch {
	case len(unreachable) == len(in.Status.Datacenters):
		return c.fail("no Patroni answers", facts)
	case len(primaries) > 1:
		return c.fail(fmt.Sprintf("%d datacenters have a primary", len(primaries)), facts)
	}

	return c.pass("at most one datacenter has a primary", facts)
}

// leaderRole checks that the leader of dc has role want.
func (in *Input) leaderRole(id check.ID, dc string, want status.Role) check.Check {
	c := block(id, dc)

	if dc == "" {
		return c.fail("the active datacenter is not known", nil)
	}

	s := in.pgDatacenter(dc)
	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	if s.leader == nil {
		return c.fail(dc+" has no leader", nil)
	}

	facts := map[string]any{"leader": memberFacts(*s.leader)}

	if s.leader.Role != want {
		return c.fail(fmt.Sprintf("the leader of %s is %s, not %s", dc, roleName(s.leader.Role), want), facts)
	}

	return c.pass(fmt.Sprintf("the leader of %s is %s", dc, want), facts)
}

func roleName(r status.Role) string {
	if r == status.RoleUnknown {
		return "of an unknown role"
	}

	return string(r)
}

func (in *Input) activeName() string {
	a, _ := in.active()

	return a
}

func (in *Input) sourcePrimary() check.Check {
	return in.leaderRole(check.PatroniSourcePrimary, in.activeName(), status.RolePrimary)
}

func (in *Input) targetStandbyLeader() check.Check {
	return in.leaderRole(check.PatroniTargetStandbyLeader, in.Params.Datacenter, status.RoleStandbyLeader)
}

func (in *Input) dcPrimary() check.Check {
	return in.leaderRole(check.PatroniDCPrimary, in.Params.Datacenter, status.RolePrimary)
}

func (in *Input) dcStandby() check.Check {
	return in.leaderRole(check.PatroniDCStandby, in.Params.Datacenter, status.RoleStandbyLeader)
}

// follows tells why the standby leader of dc does not stream from the primary of a
// on its timeline; nil when it does. The datacenter of standby_cluster.host must be a,
// its port the PostgreSQL port, and the primary must show the standby leader among its
// replication clients by the IP of its conn_url.
func (in *Input) follows(dc, a string) ([]string, map[string]any) {
	sd, sa := in.pgDatacenter(dc), in.pgDatacenter(a)
	facts := map[string]any{"source": a}

	switch {
	case !sd.src.Reachable:
		return []string{sd.what() + " does not answer"}, facts
	case !sa.src.Reachable:
		return []string{sa.what() + " does not answer"}, facts
	case sd.leader == nil:
		return []string{dc + " has no leader"}, facts
	case sa.leader == nil:
		return []string{a + " has no leader"}, facts
	}

	d, _ := in.datacenter(dc)
	ad, _ := in.datacenter(a)
	l := sd.leader

	facts["replication_state"] = l.ReplicationState
	facts["host"] = d.Patroni.StandbyClusterHost
	facts["upstream_datacenter"] = d.Patroni.UpstreamDatacenter

	var why []string

	if l.Role != status.RoleStandbyLeader {
		why = append(why, fmt.Sprintf("the leader of %s is %s", dc, roleName(l.Role)))
	}

	if !l.Reachable || l.ReplicationState != stateStreaming {
		why = append(why, fmt.Sprintf("the leader of %s does not stream", dc))
	}

	if d.Patroni.UpstreamDatacenter != a {
		why = append(why, fmt.Sprintf("standby_cluster.host %q is not the nodes of %s", d.Patroni.StandbyClusterHost, a))
	}

	tl, ta := d.Patroni.Timeline, ad.Patroni.Timeline
	if tl != nil {
		facts["timeline"] = *tl
	}

	if ta != nil {
		facts["source_timeline"] = *ta
	}

	if tl == nil || ta == nil || *tl != *ta {
		why = append(why, fmt.Sprintf("the timeline of %s is not the one of %s", dc, a))
	}

	port, ok := standbyPort(sd.cluster)
	facts["port"] = port

	if !ok || port != postgresPort {
		why = append(why, fmt.Sprintf("standby_cluster.port is not %d", postgresPort))
	}

	clients := replicationClients(sa)
	facts["source_clients"] = strs(clients)

	if !slices.Contains(clients, l.Host) {
		why = append(why, fmt.Sprintf("the primary of %s has no replication client %s", a, l.Host))
	}

	return why, facts
}

// standbyPort returns standby_cluster.port of the config of the cluster: the
// PostgreSQL port when the config does not set one, false when the config is not
// known or the port is not a number.
func standbyPort(cl *collect.Cluster) (int64, bool) {
	if cl == nil || !cl.Patroni.Data.Config.Reachable {
		return 0, false
	}

	sc, ok := cl.Patroni.Data.Config.Data["standby_cluster"].(map[string]any)
	if !ok {
		return 0, false
	}

	p, ok := sc["port"]
	if !ok {
		return postgresPort, true
	}

	return whole(p, maxPort)
}

// replicationClients returns the client_addr of every replication of the leader of s.
func replicationClients(s pgScope) []string {
	var out []string

	for _, m := range s.raw {
		if s.leader == nil || m.Name != s.leader.Name || m.Probe == nil {
			continue
		}

		for _, r := range m.Probe.Replication {
			out = append(out, r.ClientAddr)
		}
	}

	return out
}

func (in *Input) targetFollowsSource() check.Check {
	b := in.Params.Datacenter
	c := block(check.PatroniTargetFollowsSource, b)

	a, ok := in.active()
	if !ok {
		return c.fail("the active datacenter is not known", nil)
	}

	why, facts := in.follows(b, a)
	if len(why) > 0 {
		return c.fail(strings.Join(why, "; "), facts)
	}

	return c.pass(b+" streams from "+a+" on its timeline", facts)
}

func (in *Input) targetLag() check.Check {
	b := in.Params.Datacenter
	c := block(check.PatroniTargetLag, b)
	facts := map[string]any{"limit_bytes": in.Config.PlannedMaxLagBytes}

	d, ok := in.datacenter(b)
	if !ok || d.Patroni.LagBytes == nil {
		return c.fail("the lag of "+b+" is not known", facts)
	}

	lag := *d.Patroni.LagBytes
	facts["lag_bytes"] = lag

	if lag > in.Config.PlannedMaxLagBytes {
		return c.fail(fmt.Sprintf("%s lags %d bytes, more than %d", b, lag, in.Config.PlannedMaxLagBytes), facts)
	}

	return c.pass(fmt.Sprintf("%s lags %d bytes", b, lag), facts)
}

// others are the datacenters besides the active one and the target.
func (in *Input) others() []string {
	a, _ := in.active()

	var out []string

	for _, d := range in.Snapshot.Datacenters {
		if d.Name != a && d.Name != in.Params.Datacenter {
			out = append(out, d.Name)
		}
	}

	return out
}

// othersFollowSource has a result for each other standby; one that does not answer is
// a warning only, it is pointed at the new primary later.
func (in *Input) othersFollowSource() []check.Check {
	cs := in.others()
	if len(cs) == 0 {
		return []check.Check{warnOnly(check.PatroniOthersFollowSource, "").skip("there is no other standby datacenter")}
	}

	a, ok := in.active()
	out := make([]check.Check, 0, len(cs))

	for _, dc := range cs {
		c := warnOnly(check.PatroniOthersFollowSource, dc)

		if !ok {
			out = append(out, c.fail("the active datacenter is not known", nil))

			continue
		}

		if why, facts := in.follows(dc, a); len(why) > 0 {
			out = append(out, c.fail(strings.Join(why, "; "), facts))
		} else {
			out = append(out, c.pass(dc+" streams from "+a+" on its timeline", facts))
		}
	}

	return out
}

// targetNotStreaming fails while the target still streams from the old primary: then
// that one is alive and not isolated, and a failover would make two primaries.
func (in *Input) targetNotStreaming() check.Check {
	b := in.Params.Datacenter
	c := block(check.PatroniTargetNotStreaming, b)

	s := in.pgDatacenter(b)
	switch {
	case !s.src.Reachable:
		return c.unreachable(s.what(), s.src.Reason)
	case s.leader == nil:
		return c.fail(b+" has no leader", nil)
	case !s.leader.Reachable:
		return c.fail("the leader of "+b+" does not answer, whether it streams is not known", nil)
	}

	d, _ := in.datacenter(b)
	facts := map[string]any{"replication_state": s.leader.ReplicationState, "host": d.Patroni.StandbyClusterHost}

	if s.leader.ReplicationState == stateStreaming {
		return c.fail("the leader of "+b+" still streams from its upstream", facts)
	}

	return c.pass("the leader of "+b+" does not stream", facts)
}

func received(s pgScope) (*int64, map[string]any) {
	if s.leader == nil || !s.leader.Reachable {
		return nil, map[string]any{"datacenter": s.dc, "reachable": false}
	}

	f := map[string]any{"datacenter": s.dc, "replayed_timestamp": s.leader.Xlog.ReplayedTimestamp}
	if r := s.leader.Xlog.ReceivedLocation; r != nil {
		f["received_location"] = *r
	}

	return s.leader.Xlog.ReceivedLocation, f
}

// targetMaxLSN needs a confirmation when another standby that answers received more WAL
// than the target, or when what the target received is not known.
func (in *Input) targetMaxLSN() check.Check {
	b := in.Params.Datacenter
	c := confirm(check.PatroniTargetMaxLSN, check.ConfirmNotMaxLSN, b)

	cs := in.others()
	if len(cs) == 0 {
		return c.skip("there is no other standby datacenter")
	}

	rb, fb := received(in.pgDatacenter(b))
	facts := map[string]any{"target": fb}

	var (
		more   []string
		others []any
	)

	for _, dc := range cs {
		r, f := received(in.pgDatacenter(dc))
		others = append(others, f)

		if rb != nil && r != nil && *r > *rb {
			more = append(more, dc)
		}
	}

	facts["others"] = others

	switch {
	case rb == nil:
		return c.fail("the WAL "+b+" received is not known", facts)
	case len(more) > 0:
		return c.fail("received more WAL than "+b+": "+strings.Join(more, ", "), facts)
	}

	return c.pass(b+" received the most WAL of the standbys that answer", facts)
}

// candidate is a member of dc that Patroni may promote: it answers over verified TLS
// with a supported version and has neither nofailover nor failover_priority 0. Of
// them the one with the highest failover_priority wins, then the one with the least
// known lag.
func candidates(s pgScope, dc string) []status.Member {
	var out []status.Member

	for _, m := range s.members {
		if m.Datacenter == dc && m.Usable && !m.NoFailover && m.FailoverPriority > 0 {
			out = append(out, m)
		}
	}

	slices.SortStableFunc(out, func(x, y status.Member) int {
		if c := cmp.Compare(y.FailoverPriority, x.FailoverPriority); c != 0 {
			return c
		}

		switch {
		case x.LagBytes == nil && y.LagBytes == nil:
			return 0
		case x.LagBytes == nil:
			return 1
		case y.LagBytes == nil:
			return -1
		}

		return cmp.Compare(*x.LagBytes, *y.LagBytes)
	})

	return out
}

func memberStreams(m status.Member) bool {
	if m.Reachable {
		return m.ReplicationState == stateStreaming
	}

	return m.PatroniState == stateStreaming
}

func maximumLagOnFailover(cl *collect.Cluster) (int64, bool) {
	if cl == nil || !cl.Patroni.Data.Config.Reachable {
		return 0, false
	}

	v, ok := cl.Patroni.Data.Config.Data["maximum_lag_on_failover"]
	if !ok {
		return defaultMaximumLagOnFailover, true
	}

	return whole(v, maxExactFloat)
}

// targetCandidate needs, in stretch, a member of the target Patroni switches over to.
func (in *Input) targetCandidate() check.Check {
	b := in.Params.Datacenter
	c := block(check.PatroniTargetCandidate, b)
	s := in.pgCluster()

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	if s.leader != nil && s.leader.Datacenter == b {
		return c.skip("the leader already runs in " + b)
	}

	limit, ok := maximumLagOnFailover(s.cluster)
	if !ok {
		return c.fail("maximum_lag_on_failover is not known, the config of the leader does not answer", nil)
	}

	all := candidates(s, b)
	facts := map[string]any{"maximum_lag_bytes": limit, "members": membersFacts(all)}

	for _, m := range all {
		if !memberStreams(m) || m.LagBytes == nil || *m.LagBytes > limit {
			continue
		}

		facts["candidate"] = m.Name
		facts["lag_bytes"] = *m.LagBytes
		facts["failover_priority"] = m.FailoverPriority

		return c.pass("the candidate is "+m.Name, facts)
	}

	return c.fail(fmt.Sprintf("no member of %s streams within %d bytes of lag", b, limit), facts)
}

// targetLeader, for a failover in stretch, needs the leader in the target already, or
// no leader and a candidate in the target. A leader elsewhere means Patroni did not
// lose it.
func (in *Input) targetLeader() check.Check {
	b := in.Params.Datacenter
	c := block(check.PatroniTargetLeader, b)
	s := in.pgCluster()

	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	all := candidates(s, b)
	facts := map[string]any{"candidates": membersFacts(all)}

	if s.leader != nil {
		facts["leader"] = memberFacts(*s.leader)

		if s.leader.Datacenter == b {
			return c.pass("the leader already runs in "+b+", Patroni needs no failover", facts)
		}

		return c.fail(fmt.Sprintf("the leader runs in %s", s.leader.Datacenter), facts)
	}

	if slices.ContainsFunc(s.raw, isLeader) {
		return c.fail("the datacenter of the leader is not known", facts)
	}

	if len(all) == 0 {
		return c.fail("there is no leader and no candidate in "+b, facts)
	}

	return c.pass("there is no leader, "+all[0].Name+" is the candidate", facts)
}

func isLeader(m collect.Member) bool {
	return m.Role == patroni.ClusterRoleLeader || m.Role == patroni.ClusterRoleStandbyLeader
}

// candidateLoss, for a failover in stretch without a leader, needs the operator to
// accept the WAL the candidate lacks. A lag that is not known is covered by no bound.
func (in *Input) candidateLoss() check.Check {
	b := in.Params.Datacenter
	c := confirm(check.PatroniCandidateLoss, check.ConfirmMaxDiscardBytes, b)
	s := in.pgCluster()

	if !s.src.Reachable {
		return c.fail(s.what()+" does not answer, the loss is not known",
			map[string]any{"reachable": false, "reason": string(s.src.Reason)})
	}

	if slices.ContainsFunc(s.raw, isLeader) {
		return c.skip("the cluster has a leader")
	}

	all := candidates(s, b)
	facts := map[string]any{"candidates": membersFacts(all)}

	if len(all) == 0 {
		return c.fail("there is no candidate in "+b+", the loss is not known", facts)
	}

	m := all[0]
	facts["candidate"] = m.Name

	if m.LagBytes == nil {
		return c.fail("the lag of the candidate "+m.Name+" is not known", facts)
	}

	facts["lag_bytes"] = *m.LagBytes

	if *m.LagBytes > 0 {
		return c.discard(fmt.Sprintf("the candidate %s lags %d bytes, a failover discards them", m.Name, *m.LagBytes),
			facts, *m.LagBytes)
	}

	return c.pass("the candidate "+m.Name+" does not lag", facts)
}

func (in *Input) activePrimary() check.Check {
	a, ok := in.active()
	c := block(check.PatroniActivePrimary, a)

	if !ok {
		return c.fail("the active datacenter is not known", nil)
	}

	if in.warm() {
		s := in.pgDatacenter(a)

		switch {
		case !s.src.Reachable:
			return c.unreachable(s.what(), s.src.Reason)
		case s.leader == nil:
			return c.fail(a+" has no leader", nil)
		}

		facts := map[string]any{"leader": memberFacts(*s.leader)}
		if s.leader.Role != status.RolePrimary || s.leader.PatroniState != stateRunning {
			return c.fail(fmt.Sprintf("the leader of %s is %s and %s", a, roleName(s.leader.Role), s.leader.PatroniState), facts)
		}

		return c.pass("the leader of "+a+" is a running primary", facts)
	}

	s := in.pgCluster()

	switch {
	case !s.src.Reachable:
		return c.unreachable(s.what(), s.src.Reason)
	case s.leader == nil:
		return c.fail("the leader runs in no known datacenter", nil)
	}

	facts := map[string]any{"leader": memberFacts(*s.leader)}
	if s.leader.Datacenter != a {
		return c.fail("the leader runs in "+s.leader.Datacenter, facts)
	}

	return c.pass("the leader runs in "+a, facts)
}

// divergence needs the operator to accept the WAL of the datacenter that left the
// history of the active one; a rewind discards it. A divergence that cannot be told
// has no amount, which no bound covers.
func (in *Input) divergence() check.Check {
	dc := in.Params.Datacenter
	c := confirm(check.PatroniDivergence, check.ConfirmMaxDiscardBytes, dc)

	d, ok := in.datacenter(dc)
	if !ok {
		return c.fail(fmt.Sprintf("%q is not a configured datacenter", dc), nil)
	}

	div := d.Patroni.Divergence
	facts := map[string]any{"known": div.Known, "diverged": div.Diverged}

	if d.Patroni.Timeline != nil {
		facts["timeline"] = *d.Patroni.Timeline
	}

	switch {
	case !div.Known:
		return c.fail("whether "+dc+" diverged from the active datacenter is not known", facts)
	case !div.Diverged:
		return c.pass(dc+" did not diverge from the active datacenter", facts)
	}

	facts["divergence_lsn"] = div.LSN
	facts["divergence_bytes"] = div.Bytes

	return c.discard(fmt.Sprintf("%s diverged, a rewind discards %d bytes", dc, div.Bytes), facts, div.Bytes)
}

// dcRecovered needs every member of the datacenter to stream on the timeline of the
// active one, the standby leader from the active datacenter.
func (in *Input) dcRecovered() check.Check {
	dc := in.Params.Datacenter
	c := block(check.PatroniDCRecovered, dc)

	a, ok := in.active()
	if !ok {
		return c.fail("the active datacenter is not known", nil)
	}

	s := in.pgDatacenter(dc)
	if !s.src.Reachable {
		return c.unreachable(s.what(), s.src.Reason)
	}

	var timeline *int64

	if in.warm() {
		ad, _ := in.datacenter(a)
		timeline = ad.Patroni.Timeline
	} else if l := in.pgCluster().leader; l != nil {
		timeline = l.Timeline
	}

	facts := map[string]any{"members": membersFacts(s.members)}

	if timeline == nil {
		return c.fail("the timeline of the active datacenter is not known", facts)
	}

	facts["timeline"] = *timeline

	var why []string

	if len(s.members) == 0 {
		why = append(why, "no member is found")
	}

	if in.warm() {
		if d, _ := in.datacenter(dc); d.Patroni.UpstreamDatacenter != a || s.leader == nil || s.leader.Role != status.RoleStandbyLeader {
			why = append(why, "the leader is not a standby leader of "+a)
		}
	}

	for _, m := range s.members {
		if !m.Reachable || m.ReplicationState != stateStreaming || m.Timeline == nil || *m.Timeline != *timeline {
			why = append(why, m.Name+" does not stream on the timeline of "+a)
		}
	}

	if len(why) > 0 {
		return c.fail(strings.Join(why, "; "), facts)
	}

	return c.pass("every member streams on the timeline of "+a, facts)
}

// whole returns v, a JSON number decoded into float64, as an integer in [0, upTo];
// false for anything else.
func whole(v any, upTo int64) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 || f > float64(upTo) || f != math.Trunc(f) {
		return 0, false
	}

	return int64(f), true
}
