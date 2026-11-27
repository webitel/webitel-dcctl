package pgctl_test

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// stretch is one cluster over A and B: the leader s1 in A on timeline 7, s2 in A, s3 in B.
func stretch(w *world) *fakeCluster {
	f := w.cluster(dcA,
		member("s1", "10.0.1.1", patroni.RolePrimary, 7, 1),
		member("s2", "10.0.1.2", patroni.RoleReplica, 7, 1),
	)
	f.add(dcB, member("s3", "10.0.2.1", patroni.RoleReplica, 7, 2))
	w.catalog.hosts["10.0.2.1"] = dcB

	return f
}

// leads makes winner the primary on timeline after d, and the old leader a replica.
func leads(d time.Duration, winner string, timeline int64) func(*fakeCluster, string, string) {
	return func(f *fakeCluster, _, _ string) {
		f.after(d, func(f *fakeCluster) {
			f.setRole("s1", patroni.RoleReplica)
			f.setRole(winner, patroni.RolePrimary)
			f.setTimeline(winner, timeline)
		})
	}
}

func TestSwitchover(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeCluster)
		want  pgctl.RequestState
	}{
		{name: "answered", want: pgctl.RequestApplied, setup: func(f *fakeCluster) {
			f.onPost, f.postAnswer = leads(3*time.Second, "s3", 8), `Successfully switched over to "s3"`
		}},
		{name: "transport error", want: pgctl.RequestUnknown, setup: func(f *fakeCluster) {
			f.onPost, f.postErr = leads(3*time.Second, "s3", 8), errDown
		}},
		{name: "held past the switch", want: pgctl.RequestUnknown, setup: func(f *fakeCluster) {
			f.onPost, f.postHang = leads(3*time.Second, "s3", 8), true
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				f := stretch(w)
				tt.setup(f)

				var rec recorder

				res, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s1", "s3",
					pgctl.Options{Guard: allow, Progress: rec.add})
				require.NoError(t, err)
				assert.Equal(t, tt.want, res.Request)
				assert.Equal(t, "s3", res.Leader)
				assert.Equal(t, i64(8), res.Timeline)
				assert.Equal(t, []string{"/switchover 10.0.1.1:8008 s1 s3"}, f.postCalls())
				assert.Equal(t, pgctl.PhaseDone, rec.seen[len(rec.seen)-1].Phase)
			})
		})
	}
}

func TestSwitchoverNotDone(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world, f *fakeCluster)
		want  error
		state pgctl.RequestState
	}{
		{name: "another member wins", want: pgctl.ErrTimeout, state: pgctl.RequestApplied, setup: func(_ *world, f *fakeCluster) {
			f.onPost, f.postAnswer = leads(time.Second, "s2", 8), `Switched over to "s2" instead of "s3"`
		}},
		{name: "same timeline", want: pgctl.ErrTimeout, state: pgctl.RequestApplied, setup: func(_ *world, f *fakeCluster) {
			f.onPost = leads(time.Second, "s3", 7)
		}},
		{name: "no primary tag in consul", want: pgctl.ErrTimeout, state: pgctl.RequestApplied, setup: func(w *world, f *fakeCluster) {
			f.onPost = leads(time.Second, "s3", 8)

			w.catalog.setPrimary(dcB)
		}},
		{name: "old leader still primary", want: pgctl.ErrTimeout, state: pgctl.RequestApplied, setup: func(w *world, f *fakeCluster) {
			w.catalog.setPrimary(dcB, "10.0.2.1")

			f.onPost = func(f *fakeCluster, _, _ string) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("s3", patroni.RolePrimary)
					f.setTimeline("s3", 8)
					f.m("s1").Role = patroni.ClusterRoleReplica
				})
			}
		}},
		{name: "another primary in consul", want: pgctl.ErrTimeout, state: pgctl.RequestApplied, setup: func(w *world, f *fakeCluster) {
			f.onPost = leads(time.Second, "s3", 8)

			w.catalog.setPrimary(dcB, "10.0.2.1", "10.0.2.9")
		}},
		{name: "candidate in transition", want: pgctl.ErrTimeout, state: pgctl.RequestApplied, setup: func(w *world, f *fakeCluster) {
			w.catalog.setPrimary(dcB, "10.0.2.1")

			f.onPost = func(f *fakeCluster, _, _ string) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("s1", patroni.RoleReplica)
					f.m("s3").Role = patroni.ClusterRoleLeader
					f.m("s3").Probe.Role = patroni.RolePromoted
					f.setTimeline("s3", 8)
				})
			}
		}},
		{name: "outcome unknown", want: pgctl.ErrOutcomeUnknown, state: pgctl.RequestUnknown, setup: func(_ *world, f *fakeCluster) {
			f.postErr = errDown
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				f := stretch(w)
				tt.setup(w, f)

				start := time.Now()

				res, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s1", "s3", pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, tt.want)
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.Equal(t, timeouts.Promote, time.Since(start))
				assert.Equal(t, tt.state, res.Request)
				assert.Len(t, f.postCalls(), 1)
			})
		})
	}
}

func TestSwitchoverRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := stretch(w)
		f.postErr = refusal(http.StatusPreconditionFailed)

		res, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s1", "s3", pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrRefused)
		assert.Equal(t, pgctl.RequestRefused, res.Request)
		assert.Equal(t, "s1", res.Leader)
	})
}

func TestSwitchoverPrecondition(t *testing.T) {
	tests := []struct {
		name      string
		dc        string
		candidate string
		setup     func(f *fakeCluster)
	}{
		{name: "candidate in another datacenter", dc: dcA, candidate: "s3"},
		{name: "unknown candidate", dc: dcB, candidate: "s9"},
		{name: "candidate api_url on another host", dc: dcB, candidate: "s3", setup: func(f *fakeCluster) {
			f.m("s3").Endpoint = "10.0.1.1:8008"
		}},
		{name: "leader is standby_leader", dc: dcB, candidate: "s3", setup: func(f *fakeCluster) {
			f.setRole("s1", patroni.RoleStandbyLeader)
		}},
		{name: "candidate leads with another primary", dc: dcB, candidate: "s3", setup: func(f *fakeCluster) {
			f.setRole("s3", patroni.RolePrimary)
			f.setTimeline("s3", 8)
			f.m("s1").Role = patroni.ClusterRoleReplica
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				f := stretch(w)

				if tt.setup != nil {
					f.do(tt.setup)
				}

				start := time.Now()

				_, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: tt.dc, Patroni: f}, "s1", tt.candidate, pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrPrecondition)
				assert.Zero(t, time.Since(start))
				assert.Empty(t, f.postCalls())
			})
		})
	}
}

// A candidate that leads alone is the end of a switchover whose outcome was unknown.
func TestSwitchoverAlreadyLeads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := stretch(w)
		f.do(func(f *fakeCluster) {
			f.setRole("s1", patroni.RoleReplica)
			f.setRole("s3", patroni.RolePrimary)
			f.setTimeline("s3", 8)
		})

		res, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s1", "s3", pgctl.Options{Guard: allow})
		require.NoError(t, err)
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.Equal(t, "s3", res.Leader)
		assert.Empty(t, f.postCalls())
	})
}

func TestSwitchoverGuard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := stretch(w)

		_, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s1", "s3",
			pgctl.Options{Guard: remoteGuard(time.Hour)})
		require.ErrorIs(t, err, pgctl.ErrTimeout)
		require.NotErrorIs(t, err, pgctl.ErrGuard)
		assert.Empty(t, f.postCalls())
	})
}

// failoverCluster is the stretch cluster after its leader was lost.
func failoverCluster(w *world) *fakeCluster {
	f := stretch(w)
	f.do(func(f *fakeCluster) {
		f.setRole("s1", patroni.RoleReplica)
		f.readErr["s1"] = errDown
	})

	return f
}

func TestFailover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := failoverCluster(w)
		f.onPost = leads(3*time.Second, "s3", 8)

		res, err := w.ctl.Failover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s3", pgctl.Options{Guard: allow})
		require.NoError(t, err)
		assert.Equal(t, "s3", res.Leader)
		assert.Equal(t, []string{"/failover 10.0.2.1:8008 s3"}, f.postCalls())
	})
}

func TestFailoverWithLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := stretch(w)

		_, err := w.ctl.Failover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s3", pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
		assert.Empty(t, f.postCalls())
	})
}

func TestFailoverTimeout(t *testing.T) {
	tests := []struct {
		name  string
		react func(*fakeCluster, string, string)
	}{
		{name: "no leader", react: func(*fakeCluster, string, string) {}},
		{name: "another member wins", react: leads(time.Second, "s2", 8)},
		{name: "cluster shows another leader", react: func(f *fakeCluster, _, _ string) {
			f.after(time.Second, func(f *fakeCluster) {
				f.m("s2").Role = patroni.ClusterRoleLeader
				f.m("s3").Probe.Role = patroni.RolePrimary
				f.setTimeline("s3", 8)
			})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				f := failoverCluster(w)
				f.onPost = tt.react

				start := time.Now()

				_, err := w.ctl.Failover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s3", pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.Equal(t, timeouts.Promote, time.Since(start))
			})
		})
	}
}

// The leader moved to s9 of dc_c after the caller decided to switch over from s1: the
// switchover is refused before any request.
func TestSwitchover_leaderChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		f := stretch(w)
		f.add(dcC, member("s9", "10.0.9.1", patroni.RoleReplica, 7, 1))
		w.catalog.hosts["10.0.9.1"] = dcC

		f.do(func(f *fakeCluster) {
			f.setRole("s1", patroni.RoleReplica)
			f.setRole("s9", patroni.RolePrimary)
		})

		res, err := w.ctl.Switchover(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: f}, "s1", "s3", pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
		assert.ErrorContains(t, err, "leader is s9, not s1")
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.Empty(t, f.postCalls())
	})
}
