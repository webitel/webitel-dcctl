package pgctl_test

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func TestPlace(t *testing.T) {
	w := newWorld(t)
	f := stretch(w)
	f.config["maximum_lag_on_failover"] = float64(1 << 20)
	f.add(dcC, member("s9", "10.0.9.1", patroni.RoleReplica, 7, 1))

	p, err := w.ctl.Place(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, []string{dcA, dcB})
	require.NoError(t, err)

	require.NotNil(t, p.Leader)
	assert.Equal(t, "s1", p.Leader.Name)
	assert.Equal(t, dcA, p.Leader.Datacenter)

	where := map[string]string{}
	for _, m := range p.Members {
		where[m.Name] = m.Datacenter
	}

	assert.Equal(t, map[string]string{"s1": dcA, "s2": dcA, "s3": dcB, "s9": ""}, where,
		"a member of a datacenter the read does not ask about is not placed")
	assert.Equal(t, map[string][]string{dcA: {"10.0.1.1"}}, p.Primaries)
	assert.Equal(t, float64(1<<20), p.Config["maximum_lag_on_failover"])
	assert.Equal(t, []string{"s1"}, names(p.Primary()))
}

func TestPlace_noLeader(t *testing.T) {
	w := newWorld(t)
	f := stretch(w)
	f.do(func(f *fakeCluster) { f.setRole("s1", patroni.RoleReplica) })

	p, err := w.ctl.Place(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, []string{dcA, dcB})
	require.NoError(t, err)

	assert.Nil(t, p.Leader)
	assert.Empty(t, p.Primaries)
	assert.Empty(t, p.Primary())
	assert.NotNil(t, p.Config, "without a leader the config comes from a member")
}

func TestPlace_fails(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world, f *fakeCluster)
	}{
		{name: "members_not_read", setup: func(_ *world, f *fakeCluster) { f.membersErr = errDown }},
		{name: "consul_not_read", setup: func(w *world, _ *fakeCluster) { w.catalog.err = errDown }},
		{name: "two_leaders", setup: func(_ *world, f *fakeCluster) { f.m("s3").Role = patroni.ClusterRoleLeader }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			f := stretch(w)
			tt.setup(w, f)

			_, err := w.ctl.Place(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, []string{dcA, dcB})
			assert.Error(t, err)
		})
	}
}

// switchedOver is the cluster of stretch after s3 in dc_b took over on timeline 8: s1
// and s2 in dc_a are replicas, still on timeline 7 and not streaming.
func switchedOver(w *world) *fakeCluster {
	f := stretch(w)

	f.do(func(f *fakeCluster) {
		f.setRole("s1", patroni.RoleReplica)
		f.setRole("s3", patroni.RolePrimary)
		f.setTimeline("s3", 8)
	})

	return f
}

// streamsOn makes the replica name stream on timeline.
func streamsOn(f *fakeCluster, name string, timeline int64) {
	f.setTimeline(name, timeline)
	f.m(name).Probe.ReplicationState = patroni.ReplicationStreaming
}

func TestRejoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := switchedOver(w)

		f.do(func(f *fakeCluster) {
			f.after(5*time.Second, func(f *fakeCluster) { streamsOn(f, "s2", 8) })
			f.after(30*time.Second, func(f *fakeCluster) { streamsOn(f, "s1", 8) })
		})

		start := time.Now()

		res, err := w.ctl.Rejoin(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: f}, pgctl.Options{})
		require.NoError(t, err)

		assert.GreaterOrEqual(t, time.Since(start), 30*time.Second, "the old leader is waited for")
		assert.Empty(t, res.Unread)
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.Empty(t, f.postCalls())
		assert.Empty(t, f.patchCalls())
	})
}

// A member that does not answer the first read is not waited for, and is named.
func TestRejoin_unreadMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := switchedOver(w)

		f.do(func(f *fakeCluster) {
			f.readErr["s2"] = errDown
			streamsOn(f, "s1", 8)
		})

		res, err := w.ctl.Rejoin(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: f}, pgctl.Options{})
		require.NoError(t, err)
		assert.Equal(t, []string{"s2"}, res.Unread)
	})
}

func TestRejoin_timesOut(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeCluster)
	}{
		{name: "old_timeline", setup: func(f *fakeCluster) { streamsOn(f, "s2", 8); streamsOn(f, "s1", 7) }},
		{name: "not_streaming", setup: func(f *fakeCluster) { streamsOn(f, "s2", 8); f.setTimeline("s1", 8) }},
		{name: "leader_in_datacenter", setup: func(f *fakeCluster) {
			f.setRole("s3", patroni.RoleReplica)
			f.setRole("s1", patroni.RolePrimary)
		}},
		{name: "no_leader", setup: func(f *fakeCluster) { f.setRole("s3", patroni.RoleReplica) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				f := switchedOver(w)
				f.do(tt.setup)

				_, err := w.ctl.Rejoin(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: f}, pgctl.Options{})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.NotErrorIs(t, err, pgctl.ErrOutcomeUnknown)
			})
		})
	}
}

func names(ms []pgctl.Placed) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}

	return out
}

// A healthy primary instance on a node of no configured datacenter is seen too.
func TestPlace_primaryOutsideKnownDatacenters(t *testing.T) {
	w := newWorld(t)
	f := stretch(w)
	f.do(func(f *fakeCluster) { f.setRole("s1", patroni.RoleReplica) })

	w.catalog.primaryOverride["dc_x"] = []string{"10.0.9.9"}
	w.catalog.hosts["10.0.9.9"] = "dc_x"

	p, err := w.ctl.Place(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, []string{dcA, dcB, dcC})
	require.NoError(t, err)
	assert.Nil(t, p.Leader)
	assert.Equal(t, map[string][]string{"": {"10.0.9.9"}}, p.Primaries)
}

// A host registered in two datacenters has no datacenter to tell.
func TestPlace_hostInTwoDatacenters(t *testing.T) {
	w := newWorld(t)
	f := stretch(w)
	w.catalog.alsoIn = map[string]string{"10.0.1.2": dcB}

	_, err := w.ctl.Place(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, []string{dcA, dcB})
	require.ErrorContains(t, err, "10.0.1.2")
}

// s2 of dc_a is an instance of the scope, but its member key expired from the DCS: it is
// named, not waited for.
func TestRejoin_instanceMissingFromCluster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := switchedOver(w)

		f.do(func(f *fakeCluster) {
			streamsOn(f, "s1", 8)
			f.members = slices.DeleteFunc(f.members, func(m patroni.Member) bool { return m.Name == "s2" })
		})

		res, err := w.ctl.Rejoin(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: f}, pgctl.Options{})
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.1.2"}, res.Missing)
		assert.Empty(t, res.Unread)
	})
}
