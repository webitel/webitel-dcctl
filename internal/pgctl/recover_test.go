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

// recovering is promoted with a replica c2 in C, which streams on timeline 5 still.
func recovering(w *world) (b, c *fakeCluster) {
	b, c = promoted(w)

	r := member("c2", "10.0.3.2", patroni.RoleReplica, 5, 2)
	r.Probe.ReplicationState = "streaming"

	c.do(func(f *fakeCluster) { f.add(dcC, r) })

	w.catalog.hosts["10.0.3.2"] = dcC

	return b, c
}

// replicaFollows makes c2 stream on timeline 6.
func replicaFollows(c *fakeCluster) {
	c.setTimeline("c2", 6)
	c.m("c2").Probe.ReplicationState = "streaming"
}

func TestRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := recovering(w)
		c.after(100*time.Second, follows(b))
		c.after(300*time.Second, replicaFollows)

		start := time.Now()

		res, err := w.ctl.Recover(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{})
		require.NoError(t, err)
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.GreaterOrEqual(t, time.Since(start), 300*time.Second, "the replica is waited for too")
		assert.Empty(t, c.patchCalls())
	})
}

func TestRecoverTimeout(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(w *world, b, c *fakeCluster)
		detail string
	}{
		{
			name:   "standby_leader_does_not_follow",
			setup:  func(*world, *fakeCluster, *fakeCluster) {},
			detail: "timeline 5",
		},
		{
			name:   "replica_on_the_old_timeline",
			setup:  func(_ *world, b, c *fakeCluster) { c.do(follows(b)) },
			detail: "not streaming on timeline 6: c2",
		},
		{
			name: "replica_not_read",
			setup: func(_ *world, b, c *fakeCluster) {
				c.do(follows(b))
				c.do(replicaFollows)
				c.do(func(f *fakeCluster) { f.readErr["c2"] = errDown })
			},
			detail: "c2",
		},
		{
			name: "instance_not_in_the_cluster",
			setup: func(w *world, b, c *fakeCluster) {
				c.do(follows(b))
				c.do(replicaFollows)

				w.catalog.hosts["10.0.3.3"] = dcC
			},
			detail: "instance 10.0.3.3 is not in the cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				b, c := recovering(w)
				tt.setup(w, b, c)

				start := time.Now()

				res, err := w.ctl.Recover(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.Equal(t, timeouts.Rewind, time.Since(start))
				assert.Contains(t, res.Detail, tt.detail)
			})
		})
	}
}

func TestRecovered(t *testing.T) {
	w := newWorld(t)
	b, c := recovering(w)
	c.do(follows(b))
	c.do(replicaFollows)

	require.NoError(t, w.ctl.Recovered(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b}))
}

func TestRecovered_fails(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world, b, c *fakeCluster)
		// recovered is set when the error is ErrNotRecovered, is otherwise.
		recovered bool
		is        error
	}{
		{name: "standby_leader_does_not_stream", setup: func(*world, *fakeCluster, *fakeCluster) {}, recovered: true},
		{
			name:      "replica_does_not_stream",
			setup:     func(_ *world, b, c *fakeCluster) { c.do(follows(b)) },
			recovered: true,
		},
		{
			name: "standby_not_read",
			setup: func(_ *world, b, c *fakeCluster) {
				c.do(follows(b))
				c.do(replicaFollows)
				c.do(func(f *fakeCluster) { f.membersErr = errDown })
			},
			is: errDown,
		},
		{
			name: "instances_not_read",
			setup: func(w *world, b, c *fakeCluster) {
				c.do(follows(b))
				c.do(replicaFollows)

				w.catalog.err = errDown
			},
			is: errDown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := recovering(w)
			tt.setup(w, b, c)

			err := w.ctl.Recovered(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.Error(t, err)
			assert.Contains(t, err.Error(), dcC)

			if tt.recovered {
				require.ErrorIs(t, err, pgctl.ErrNotRecovered)
			} else {
				require.ErrorIs(t, err, tt.is)
				require.NotErrorIs(t, err, pgctl.ErrNotRecovered)
			}
		})
	}
}
