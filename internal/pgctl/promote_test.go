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

// promotesAfter makes a PATCH that removes standby_cluster promote the leader to the
// next timeline after d.
func promotesAfter(d time.Duration, leader string, timeline int64) func(*fakeCluster, map[string]any) {
	return func(f *fakeCluster, patch map[string]any) {
		if v, ok := patch["standby_cluster"]; ok && v == nil {
			f.after(d, func(f *fakeCluster) {
				f.setRole(leader, patroni.RolePrimary)
				f.setTimeline(leader, timeline)
			})
		}
	}
}

func TestPromote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		_, b := warmStandby(w)
		b.config["standby_cluster"] = map[string]any{"host": "10.0.1.1,10.0.1.2"}
		b.onPatch = promotesAfter(5*time.Second, "b1", 6)

		var rec recorder

		res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow, Progress: rec.add})
		require.NoError(t, err)

		assert.Equal(t, pgctl.RequestApplied, res.Request)
		assert.False(t, res.FromPrimary)
		assert.Equal(t, patroni.RolePrimary, res.Role)
		assert.Equal(t, i64(6), res.Timeline)
		assert.Equal(t, []pgctl.Phase{pgctl.PhaseCheck, pgctl.PhaseRequest, pgctl.PhaseWait, pgctl.PhaseDone}, rec.phases())
		require.Equal(t, []patchCall{{endpoint: "10.0.2.1:8008", patch: map[string]any{"standby_cluster": nil}}}, b.patchCalls())
	})
}

// A demotion that did not take leaves the primary with standby_cluster set: promote
// removes it, and the primary stays on its timeline.
func TestPromoteStillPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, _ := warmStandby(w)
		a.config["standby_cluster"] = map[string]any{"host": "10.0.2.1"}

		res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Options{Guard: allow})
		require.NoError(t, err)
		assert.True(t, res.FromPrimary)
		assert.Equal(t, i64(5), res.Timeline)
		assert.NotContains(t, a.config, "standby_cluster")
	})
}

func TestPromoteTimeout(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world, b *fakeCluster)
	}{
		{name: "leader stays standby", setup: func(*world, *fakeCluster) {}},
		{name: "same timeline", setup: func(_ *world, b *fakeCluster) {
			b.onPatch = promotesAfter(time.Second, "b1", 5)
		}},
		{name: "no healthy primary in consul", setup: func(w *world, b *fakeCluster) {
			b.onPatch = promotesAfter(time.Second, "b1", 6)

			w.catalog.setPrimary(dcB)
		}},
		{name: "another primary in consul", setup: func(w *world, b *fakeCluster) {
			b.onPatch = promotesAfter(time.Second, "b1", 6)

			w.catalog.setPrimary(dcB, "10.0.2.1", "10.0.2.2")
		}},
		{name: "standby_cluster set again", setup: func(_ *world, b *fakeCluster) {
			b.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("b1", patroni.RolePrimary)
					f.setTimeline("b1", 6)
					f.config["standby_cluster"] = map[string]any{"host": "10.0.1.1"}
				})
			}
		}},
		{name: "another member is primary too", setup: func(w *world, b *fakeCluster) {
			w.catalog.setPrimary(dcB, "10.0.2.1")

			b.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("b1", patroni.RolePrimary)
					f.setTimeline("b1", 6)
					f.m("b2").Probe.Role = patroni.RolePrimary
				})
			}
		}},
		{name: "primary endpoint not read", setup: func(_ *world, b *fakeCluster) {
			b.onPatch = promotesAfter(time.Second, "b1", 6)
			b.primaryErr = errDown
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				_, b := warmStandby(w)
				tt.setup(w, b)

				start := time.Now()

				res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.Equal(t, timeouts.Promote, time.Since(start))
				assert.Equal(t, pgctl.RequestApplied, res.Request)
				assert.NotEmpty(t, res.Detail)
			})
		})
	}
}

func TestPromoteWrongCluster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := warmStandby(w)

		// The client of B finds the cluster of A.
		_, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: a}, pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
		assert.Empty(t, a.patchCalls())
		assert.Empty(t, b.patchCalls())
	})
}
