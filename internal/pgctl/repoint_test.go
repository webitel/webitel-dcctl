package pgctl_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// promoted is B, the primary b1 on timeline 6, and C, whose standby leader c1 still
// streams from A on timeline 5.
func promoted(w *world) (b, c *fakeCluster) {
	b = w.cluster(dcB,
		member("b1", "10.0.2.1", patroni.RolePrimary, 6, 1),
		member("b2", "10.0.2.2", patroni.RoleReplica, 6, 2),
	)
	c = w.cluster(dcC,
		member("c1", "10.0.3.1", patroni.RoleStandbyLeader, 5, 1),
	)

	b.do(func(f *fakeCluster) { f.m("b1").Probe.Xlog.Location = i64(5000) })
	c.do(func(f *fakeCluster) {
		p := f.m("c1").Probe
		p.ReplicationState = "streaming"
		p.Xlog.ReceivedLocation = i64(4000)
	})

	return b, c
}

// follows makes c1 stream from b1 on timeline 6.
func follows(b *fakeCluster) func(*fakeCluster) {
	return func(c *fakeCluster) {
		c.setTimeline("c1", 6)

		p := c.m("c1").Probe
		p.ReplicationState = "streaming"
		p.Xlog.ReceivedLocation = i64(5000)

		b.do(func(b *fakeCluster) {
			b.m("b1").Probe.Replication = []patroni.Replica{{ClientAddr: "10.0.3.1", State: "streaming"}}
		})
	}
}

func TestRepoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := promoted(w)
		c.onPatch = func(f *fakeCluster, _ map[string]any) { f.after(5*time.Second, follows(b)) }

		var rec recorder

		res, err := w.ctl.Repoint(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: allow, Progress: rec.add})
		require.NoError(t, err)

		assert.Equal(t, pgctl.RequestApplied, res.Request)
		assert.Equal(t, "c1", res.Member)
		assert.Equal(t, i64(6), res.Timeline)
		assert.Equal(t, i64(0), res.Lag)
		assert.Equal(t, []pgctl.Phase{pgctl.PhaseCheck, pgctl.PhaseRequest, pgctl.PhaseWait, pgctl.PhaseDone}, rec.phases())
		require.Equal(t, []patchCall{{endpoint: "10.0.3.1:8008", patch: map[string]any{
			"standby_cluster": map[string]any{
				"host":                   "10.0.2.1,10.0.2.2",
				"port":                   5432,
				"create_replica_methods": []string{"basebackup"},
			},
		}}}, c.patchCalls())
		assert.Empty(t, b.patchCalls())
	})
}

func TestRepointTimeout(t *testing.T) {
	tests := []struct {
		name  string
		react func(b *fakeCluster) func(*fakeCluster)
	}{
		{name: "stays on the old timeline", react: func(*fakeCluster) func(*fakeCluster) {
			return func(*fakeCluster) {}
		}},
		{name: "not in replication of the primary", react: func(b *fakeCluster) func(*fakeCluster) {
			return func(c *fakeCluster) {
				c.setTimeline("c1", 6)
				b.do(func(b *fakeCluster) {
					b.m("b1").Probe.Replication = []patroni.Replica{{ClientAddr: "10.0.9.9", State: "streaming"}}
				})
			}
		}},
		{name: "not streaming", react: func(b *fakeCluster) func(*fakeCluster) {
			return func(c *fakeCluster) {
				follows(b)(c)
				c.m("c1").Probe.ReplicationState = "stopped"
			}
		}},
		{name: "streams on the old timeline", react: func(b *fakeCluster) func(*fakeCluster) {
			return func(*fakeCluster) {
				b.do(func(b *fakeCluster) {
					b.m("b1").Probe.Replication = []patroni.Replica{{ClientAddr: "10.0.3.1", State: "streaming"}}
				})
			}
		}},
		{name: "primary in transition", react: func(b *fakeCluster) func(*fakeCluster) {
			return func(c *fakeCluster) {
				follows(b)(c)
				b.do(func(b *fakeCluster) { b.m("b1").Probe.Role = patroni.RoleDemoted })
			}
		}},
		{name: "primary not read", react: func(b *fakeCluster) func(*fakeCluster) {
			return func(c *fakeCluster) {
				follows(b)(c)
				b.do(func(b *fakeCluster) { b.readErr["b1"] = errDown })
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				b, c := promoted(w)
				react := tt.react(b)
				c.onPatch = func(f *fakeCluster, _ map[string]any) { f.after(time.Second, react) }

				start := time.Now()

				res, err := w.ctl.Repoint(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.Equal(t, timeouts.Repoint, time.Since(start))
				assert.NotEmpty(t, res.Detail)
			})
		})
	}
}

// Pointing a primary at another cluster would demote it.
func TestRepointPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := promoted(w)

		res, err := w.ctl.Repoint(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Cluster{Datacenter: dcC, Patroni: c},
			pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.Empty(t, b.patchCalls())
		assert.Empty(t, c.patchCalls())
	})
}

// Follow waits for the old primary, which Patroni may rewind, longer than a repoint.
func TestFollow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := promoted(w)
		c.do(func(f *fakeCluster) { f.m("c1").Probe.ReplicationState = "" })
		c.after(400*time.Second, follows(b))

		res, err := w.ctl.Follow(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{})
		require.NoError(t, err)
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.Empty(t, c.patchCalls())
	})
}

func TestFollowTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := promoted(w)

		start := time.Now()

		_, err := w.ctl.Follow(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{})
		require.ErrorIs(t, err, pgctl.ErrTimeout)
		assert.Equal(t, timeouts.Rewind, time.Since(start))
	})
}

// A standby cluster is not pointed at another standby.
func TestRepointToStandby(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := promoted(w)
		b.do(func(f *fakeCluster) { f.setRole("b1", patroni.RoleStandbyLeader) })

		_, err := w.ctl.Repoint(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
		assert.Empty(t, c.patchCalls())
	})
}
