package status

import (
	"slices"

	"github.com/webitel/webitel-dcctl/internal/collect"
)

// Health is whether a datacenter works in its role.
type Health string

const (
	HealthOK       Health = "ok"
	HealthDegraded Health = "degraded"
	HealthDown     Health = "down"
	// HealthUnknown is a datacenter whose health cannot be told for lack of data.
	HealthUnknown Health = "unknown"
)

// HealthReason is why a datacenter is not ok. Each reason has one [HealthReason.Health];
// the health of a datacenter is the worst of its reasons, down before unknown before
// degraded: data that is missing may hide a datacenter that is down.
type HealthReason string

const (
	// Reasons that make a datacenter down.

	// ReasonNoPrimary: warm_standby, the leader of the active datacenter is not a primary.
	ReasonNoPrimary HealthReason = "no_primary"
	// ReasonNoPatroniLeader: the Patroni cluster has no leader in stretch, or the
	// inactive datacenter has none in warm_standby.
	ReasonNoPatroniLeader HealthReason = "no_patroni_leader"
	// ReasonNoHealthyJobs: the active datacenter has managed jobs and none is healthy.
	ReasonNoHealthyJobs HealthReason = "no_healthy_jobs"
	// ReasonNoMembers: stretch, no Patroni member runs in the inactive datacenter, and
	// every member of the cluster has a known datacenter.
	ReasonNoMembers HealthReason = "no_members"

	// Reasons that leave the health unknown.

	ReasonDatacenterUnreachable HealthReason = "datacenter_unreachable"
	ReasonUninitialized         HealthReason = "uninitialized"
	// ReasonRoleUndetermined: the voters give no effective state, so it is not known
	// whether the datacenter is active.
	ReasonRoleUndetermined         HealthReason = "role_undetermined"
	ReasonPatroniUnreachable       HealthReason = "patroni_unreachable"
	ReasonNomadUnreachable         HealthReason = "nomad_unreachable"
	ReasonPatroniConfigUnreachable HealthReason = "patroni_config_unreachable"
	// ReasonActivePatroniUnreachable: the standby cannot be compared with the active
	// datacenter, whose Patroni does not answer.
	ReasonActivePatroniUnreachable HealthReason = "active_patroni_unreachable"
	// ReasonUpstreamUnknown: standby_cluster.host does not point at the nodes of one
	// known datacenter.
	ReasonUpstreamUnknown HealthReason = "upstream_unknown"
	ReasonLagUnknown      HealthReason = "lag_unknown"
	// ReasonLeaderDatacenterUnknown: stretch, it is not known where the leader runs.
	ReasonLeaderDatacenterUnknown HealthReason = "leader_datacenter_unknown"
	// ReasonMemberDatacenterUnknown: stretch, a member of the cluster runs in no known
	// datacenter, so it may be a member of this one that is not fine.
	ReasonMemberDatacenterUnknown HealthReason = "member_datacenter_unknown"

	// Reasons that make a datacenter degraded.

	ReasonJobsNotHealthy HealthReason = "jobs_not_healthy"
	// ReasonNoManagedJobs: the active datacenter has no managed job, so dcctl does not
	// see its apps at all.
	ReasonNoManagedJobs  HealthReason = "no_managed_jobs"
	ReasonJobsNotStopped HealthReason = "jobs_not_stopped"
	// ReasonLeaderNotInDatacenter: stretch, the leader runs outside the active datacenter.
	ReasonLeaderNotInDatacenter HealthReason = "leader_not_in_datacenter"
	// ReasonLeaderInDatacenter: stretch, the leader runs in an inactive datacenter.
	ReasonLeaderInDatacenter     HealthReason = "leader_in_datacenter"
	ReasonLeaderNotStandbyLeader HealthReason = "leader_not_standby_leader"
	ReasonNotStreaming           HealthReason = "not_streaming"
	ReasonNotFollowingActive     HealthReason = "not_following_active"
	ReasonTimelineMismatch       HealthReason = "timeline_mismatch"
	ReasonLagExceeded            HealthReason = "lag_exceeded"
	ReasonMemberUnreachable      HealthReason = "member_unreachable"
	ReasonMemberUnusable         HealthReason = "member_unusable"
	// ReasonMemberNotRunning: a member is neither running nor streaming.
	ReasonMemberNotRunning HealthReason = "member_not_running"
	ReasonPaused           HealthReason = "patroni_paused"
	ReasonNeedsRecovery    HealthReason = "needs_recovery"
)

// Health returns the health the reason gives a datacenter.
func (r HealthReason) Health() Health {
	switch r {
	case ReasonNoPrimary, ReasonNoPatroniLeader, ReasonNoHealthyJobs, ReasonNoMembers:
		return HealthDown
	case ReasonDatacenterUnreachable, ReasonUninitialized, ReasonRoleUndetermined, ReasonPatroniUnreachable,
		ReasonNomadUnreachable, ReasonPatroniConfigUnreachable, ReasonActivePatroniUnreachable,
		ReasonUpstreamUnknown, ReasonLagUnknown, ReasonLeaderDatacenterUnknown, ReasonMemberDatacenterUnknown:
		return HealthUnknown
	case ReasonJobsNotHealthy, ReasonNoManagedJobs, ReasonJobsNotStopped, ReasonLeaderNotInDatacenter, ReasonLeaderInDatacenter,
		ReasonLeaderNotStandbyLeader, ReasonNotStreaming, ReasonNotFollowingActive, ReasonTimelineMismatch,
		ReasonLagExceeded, ReasonMemberUnreachable, ReasonMemberUnusable, ReasonMemberNotRunning, ReasonPaused,
		ReasonNeedsRecovery:
	}

	return HealthDegraded
}

func rank(h Health) int {
	switch h {
	case HealthOK:
		return 0
	case HealthDegraded:
		return 1
	case HealthUnknown:
		return 2
	case HealthDown:
		return 3
	}

	return 0
}

// reasons collects the reasons of a datacenter, each once.
type reasons []HealthReason

func (rs *reasons) add(r HealthReason) {
	if !slices.Contains(*rs, r) {
		*rs = append(*rs, r)
	}
}

// worst returns the health of a datacenter with the reasons.
func worst(rs []HealthReason) Health {
	h := HealthOK

	for _, r := range rs {
		if rank(r.Health()) > rank(h) {
			h = r.Health()
		}
	}

	return h
}

func (b *builder) health(f dcFacts, state State, p Patroni) (Health, []HealthReason) {
	switch {
	case state == StateUnreachable:
		return HealthUnknown, []HealthReason{ReasonDatacenterUnreachable}
	case state == StateUninitialized:
		return HealthUnknown, []HealthReason{ReasonUninitialized}
	case b.effective == nil:
		// What does not depend on the role is still worth showing.
		rs := reasons{ReasonRoleUndetermined}
		b.clusterHealth(&rs, f.patroni, p)

		if !f.nomad.Reachable {
			rs.add(ReasonNomadUnreachable)
		}

		return HealthUnknown, rs
	}

	var rs reasons

	active := f.name == b.effective.ActiveDatacenter

	b.patroniHealth(&rs, f.patroni, p, active)

	switch total := f.jobs.Total(); {
	case !f.nomad.Reachable:
		rs.add(ReasonNomadUnreachable)
	case active && total == 0:
		rs.add(ReasonNoManagedJobs)
	case active && f.jobs.Healthy == 0:
		rs.add(ReasonNoHealthyJobs)
	case active && f.jobs.Healthy < total:
		rs.add(ReasonJobsNotHealthy)
	case !active && f.jobs.Stopped < total:
		rs.add(ReasonJobsNotStopped)
	}

	if !active && f.recovery {
		rs.add(ReasonNeedsRecovery)
	}

	return worst(rs), rs
}

// clusterHealth adds the reasons of Patroni that do not depend on the role of the
// datacenter, and reports whether Patroni of the datacenter answered.
func (b *builder) clusterHealth(rs *reasons, pp dcPatroni, p Patroni) bool {
	if !pp.src.Reachable {
		rs.add(ReasonPatroniUnreachable)

		return false
	}

	if !b.warm() {
		switch {
		case pp.clusterLeader == nil:
			rs.add(ReasonNoPatroniLeader)
		case pp.clusterLeader.Datacenter == "":
			rs.add(ReasonLeaderDatacenterUnknown)
		}

		if pp.unplaced > 0 {
			rs.add(ReasonMemberDatacenterUnknown)
		}
	}

	for i := range pp.members {
		m := &pp.members[i]

		switch st := memberState(m); {
		case !memberReachable(*m):
			rs.add(ReasonMemberUnreachable)
		case !m.Usable:
			rs.add(ReasonMemberUnusable)
		case st != "running" && st != "streaming":
			rs.add(ReasonMemberNotRunning)
		}
	}

	if p.Paused {
		rs.add(ReasonPaused)
	}

	return true
}

func (b *builder) patroniHealth(rs *reasons, pp dcPatroni, p Patroni, active bool) {
	if !b.clusterHealth(rs, pp, p) {
		return
	}

	switch {
	case b.warm() && active:
		if pp.leader == nil || roleOf(pp.leader.Role) != RolePrimary {
			rs.add(ReasonNoPrimary)
		}
	case b.warm():
		b.standbyHealth(rs, pp, p)
	case active:
		if pp.clusterLeader != nil && pp.clusterLeader.Datacenter != "" && pp.leader == nil {
			rs.add(ReasonLeaderNotInDatacenter)
		}
	default:
		if pp.leader != nil {
			rs.add(ReasonLeaderInDatacenter)
		}

		if len(pp.members) == 0 && pp.unplaced == 0 {
			rs.add(ReasonNoMembers)
		}

		for i := range pp.members {
			m := &pp.members[i]
			if (pp.leader == nil || m.Name != pp.leader.Name) && !streaming(m) {
				rs.add(ReasonNotStreaming)
			}
		}
	}
}

// standbyHealth judges the leader of an inactive datacenter of warm_standby: it must
// be a standby leader that streams from the active datacenter on its timeline, within
// the planned lag.
func (b *builder) standbyHealth(rs *reasons, pp dcPatroni, p Patroni) {
	l := pp.leader

	switch {
	case l == nil:
		rs.add(ReasonNoPatroniLeader)

		return
	case roleOf(l.Role) != RoleStandbyLeader:
		rs.add(ReasonLeaderNotStandbyLeader)

		return
	}

	if !streaming(l) {
		rs.add(ReasonNotStreaming)
	}

	switch {
	case !pp.cluster.Patroni.Data.Config.Reachable:
		rs.add(ReasonPatroniConfigUnreachable)
	case p.UpstreamDatacenter == "":
		rs.add(ReasonUpstreamUnknown)
	case p.UpstreamDatacenter != b.active():
		rs.add(ReasonNotFollowingActive)
	}

	a := b.patroniIn(b.active())
	if !a.src.Reachable {
		rs.add(ReasonActivePatroniUnreachable)

		return
	}

	if a.leader != nil && timelinesDiffer(a.leader, l) {
		rs.add(ReasonTimelineMismatch)
	}

	switch {
	case p.LagBytes == nil:
		rs.add(ReasonLagUnknown)
	case *p.LagBytes > b.cfg.PlannedMaxLagBytes:
		rs.add(ReasonLagExceeded)
	}
}

func timelinesDiffer(a, b *collect.Member) bool {
	ta, tb := memberTimeline(a), memberTimeline(b)

	return ta != nil && tb != nil && *ta != *tb
}
