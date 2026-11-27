package patroni_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func TestNotFollowing(t *testing.T) {
	tl := func(v int64) *int64 { return &v }
	member := func(name string, role patroni.Role, timeline int64, replication string) patroni.Member {
		return patroni.Member{Name: name, Probe: &patroni.State{Role: role, Timeline: tl(timeline), ReplicationState: replication}}
	}

	leader := member("a1", patroni.RoleStandbyLeader, 6, patroni.ReplicationStreaming)
	replica := member("a2", patroni.RoleReplica, 6, patroni.ReplicationStreaming)

	tests := []struct {
		name    string
		members []patroni.Member
		leader  string
		want    []string
	}{
		{name: "every_member_follows", members: []patroni.Member{leader, replica}, leader: "a1"},
		{name: "replica_on_old_timeline", members: []patroni.Member{leader, member("a2", patroni.RoleReplica, 5, "streaming")}, leader: "a1", want: []string{"a2"}},
		{name: "replica_not_streaming", members: []patroni.Member{leader, member("a2", patroni.RoleReplica, 6, "stopped")}, leader: "a1", want: []string{"a2"}},
		{name: "replica_not_read", members: []patroni.Member{leader, {Name: "a2"}}, leader: "a1", want: []string{"a2"}},
		{name: "leader_primary", members: []patroni.Member{member("a1", patroni.RolePrimary, 6, ""), replica}, leader: "a1", want: []string{"a1"}},
		{name: "no_leader", members: []patroni.Member{replica}, want: []string{"no leader"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, patroni.NotFollowing(tt.members, tt.leader, 6))
		})
	}
}
