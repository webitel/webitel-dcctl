package collect_test

import (
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func member(name, host string, role patroni.ClusterRole) patroni.Member {
	return patroni.Member{Name: name, Host: host, Endpoint: host + ":8008", Role: role}
}

func scopeNode(addr, dc string) consul.ServiceNode {
	return consul.ServiceNode{Service: "pg", Address: addr, NodeMeta: map[string]string{nodeMetaKey: dc}}
}

func datacentersOf(members []collect.Member) map[string]string {
	out := make(map[string]string, len(members))
	for _, m := range members {
		out[m.Name] = m.Datacenter
	}

	return out
}

// withLeaders gives every cluster of w a leader and a replica, a config and a history.
func withLeaders(w *world) {
	for i, p := range w.patroni {
		p.members = []patroni.Member{
			member(fmt.Sprintf("pg-%d-1", i+1), fmt.Sprintf("10.0.%d.1", i+1), patroni.ClusterRoleStandbyLeader),
			member(fmt.Sprintf("pg-%d-2", i+1), fmt.Sprintf("10.0.%d.2", i+1), patroni.ClusterRoleReplica),
		}
		p.config = map[string]any{"loop_wait": float64(10)}
		p.history = []patroni.HistoryEntry{{Timeline: int64(i + 1), LSN: 100}}
	}
}

func TestPatroni_stretchMemberDatacenterFromScopeNodeMeta(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileStretch)
		w.catalogs[0].scope = []consul.ServiceNode{
			{Service: "pg", Address: "10.1.0.5", Tags: []string{"primary"}, NodeMeta: map[string]string{nodeMetaKey: "dc_a"}},
			{Service: "pg", NodeAddress: "10.2.0.5", Tags: []string{"replica"}, NodeMeta: map[string]string{nodeMetaKey: "dc_b"}},
		}
		w.patroni[0].members = []patroni.Member{
			member("pg-a", "10.1.0.5", patroni.ClusterRoleLeader),
			member("pg-b", "10.2.0.5", patroni.ClusterRoleReplica),
			member("pg-x", "10.9.9.9", patroni.ClusterRoleReplica),
		}

		c := newCollector(t, w)
		start(t, c)

		cl := waitSeq(c, 1).Clusters[0]
		require.True(t, cl.Patroni.Reachable)
		assert.Equal(t, map[string]string{"pg-a": "dc_a", "pg-b": "dc_b", "pg-x": ""}, datacentersOf(cl.Patroni.Data.Members))
		assert.Len(t, cl.Scope.Data.Nodes, 2, "the scope service is read for the whole cluster")
	})
}

func TestPatroni_configAndHistoryOfEveryLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		withLeaders(w)

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)

		for i, dc := range datacenters {
			assert.Equal(t, []string{fmt.Sprintf("10.0.%d.1:8008", i+1)}, w.patroni[i].historyEndpoints(), dc)

			p := s.Clusters[i].Patroni.Data
			assert.Equal(t, fmt.Sprintf("pg-%d-1", i+1), p.Leader)
			require.True(t, p.Config.Reachable)
			assert.Equal(t, map[string]any{"loop_wait": float64(10)}, p.Config.Data)
			require.True(t, p.History.Reachable)
			assert.Equal(t, w.patroni[i].history, p.History.Data)
			assert.Equal(t, map[string]string{
				fmt.Sprintf("pg-%d-1", i+1): dc,
				fmt.Sprintf("pg-%d-2", i+1): dc,
			}, datacentersOf(p.Members))
		}
	})
}

// No voter, however slow, keeps /history from being read.
func TestPatroni_historyIsReadWhileAVoterHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		withLeaders(w)
		w.kv["dc_a"].set(nil, true)

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)
		require.False(t, s.Voters[0].KV.Reachable)

		for i := range datacenters {
			h := s.Clusters[i].Patroni.Data.History
			require.True(t, h.Reachable)
			assert.Equal(t, w.patroni[i].history, h.Data)
		}
	})
}

func TestPatroni_configFailureKeepsLastKnownConfig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		withLeaders(w)

		c := newCollector(t, w)
		start(t, c)

		first := waitSeq(c, 1)

		w.patroni[0].configCall.set(errRefused, false)

		p := waitSeq(c, 2).Clusters[0].Patroni
		require.True(t, p.Reachable, "the members still answer")
		assert.Len(t, p.Data.Members, 2)

		cfg := p.Data.Config
		assert.False(t, cfg.Reachable)
		assert.Equal(t, source.ReasonRefused, cfg.Reason)
		assert.Equal(t, first.ObserveTime.Add(interval), cfg.UnreachableSince)
		assert.Equal(t, map[string]any{"loop_wait": float64(10)}, cfg.Data, "last known config")
		assert.Equal(t, first.ObserveTime, cfg.ObserveTime)
		assert.True(t, p.Data.History.Reachable)
	})
}

func TestPatroni_noLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.patroni[0].members = []patroni.Member{member("pg-a", "10.0.1.1", patroni.ClusterRoleReplica)}

		c := newCollector(t, w)
		start(t, c)

		p := waitSeq(c, 1).Clusters[0].Patroni
		require.True(t, p.Reachable)
		assert.Empty(t, p.Data.Leader)
		assert.Equal(t, source.ReasonNoEndpoints, p.Data.Config.Reason)
		assert.Equal(t, source.ReasonNoEndpoints, p.Data.History.Reason)
		assert.Empty(t, w.patroni[0].configCall.calls())
		assert.Empty(t, w.patroni[0].historyEndpoints())
	})
}

func TestPatroni_membersFailureFailsConfigAndHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		withLeaders(w)

		c := newCollector(t, w)
		start(t, c)

		first := waitSeq(c, 1)

		tlsErr := &source.Error{Endpoint: "10.0.2.1:8008", Reason: source.ReasonTLS, Err: errRefused}
		w.patroni[1].membersCall.set(tlsErr, false)

		p := waitSeq(c, 2).Clusters[1].Patroni
		assert.False(t, p.Reachable)
		assert.Equal(t, source.ReasonTLS, p.Reason)
		assert.Len(t, p.Data.Members, 2, "last known members")

		cfg, hist := p.Data.Config, p.Data.History
		assert.False(t, cfg.Reachable)
		assert.Equal(t, source.ReasonTLS, cfg.Reason)
		assert.Equal(t, first.ObserveTime, cfg.ObserveTime, "time of the last known config")
		assert.NotNil(t, cfg.Data)
		assert.False(t, hist.Reachable)
		assert.Equal(t, source.ReasonTLS, hist.Reason)
		assert.Equal(t, first.ObserveTime, hist.ObserveTime, "time of the last known history")
	})
}

// A published snapshot does not change when a later cycle keeps its last known data and
// places its members anew.
func TestSnapshot_publishedSnapshotNeverChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileStretch)
		w.catalogs[0].setScope([]consul.ServiceNode{scopeNode("10.1.0.5", "dc_a"), scopeNode("10.2.0.5", "dc_b")})
		w.patroni[0].members = []patroni.Member{
			member("pg-a", "10.1.0.5", patroni.ClusterRoleLeader),
			member("pg-b", "10.2.0.5", patroni.ClusterRoleReplica),
		}
		w.patroni[0].config = map[string]any{"loop_wait": float64(10), "standby_cluster": map[string]any{"host": "x"}}

		c := newCollector(t, w)
		start(t, c)

		s1 := waitSeq(c, 1)
		want := deepCopy(s1)

		w.patroni[0].membersCall.set(errRefused, false)
		w.catalogs[0].setScope([]consul.ServiceNode{scopeNode("10.1.0.5", "dc_b"), scopeNode("10.2.0.5", "dc_a")})

		s2 := waitSeq(c, 2)
		require.False(t, s2.Clusters[0].Patroni.Reachable)
		assert.Equal(t, map[string]string{"pg-a": "dc_b", "pg-b": "dc_a"}, datacentersOf(s2.Clusters[0].Patroni.Data.Members),
			"last known members are placed by the current scope")

		assert.Equal(t, want, s1)
	})
}
