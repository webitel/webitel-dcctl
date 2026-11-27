package patroni

import "slices"

// NotFollowing returns the members of a standby cluster that do not follow its primary
// on timeline: leader, the standby leader, must stream on timeline, every other member
// must be a replica that streams on it. A member that does not answer does not follow;
// "no leader" stands for a leader that is not among members.
func NotFollowing(members []Member, leader string, timeline int64) []string {
	var out []string

	if leader == "" || !slices.ContainsFunc(members, func(m Member) bool { return m.Name == leader }) {
		out = append(out, "no leader")
	}

	for _, m := range members {
		role := RoleReplica
		if m.Name == leader {
			role = RoleStandbyLeader
		}

		p := m.Probe
		if p == nil || p.Role != role || p.ReplicationState != ReplicationStreaming || p.Timeline == nil || *p.Timeline != timeline {
			out = append(out, m.Name)
		}
	}

	return out
}
