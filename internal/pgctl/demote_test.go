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

// warmStandby is A with the primary a1 on timeline 5 and B with the standby leader b1.
// The failover priorities of B are 1, 3 and 2.
func warmStandby(w *world) (a, b *fakeCluster) {
	a = w.cluster(dcA,
		member("a1", "10.0.1.1", patroni.RolePrimary, 5, 1),
		member("a2", "10.0.1.2", patroni.RoleReplica, 5, 1),
	)
	b = w.cluster(dcB,
		member("b1", "10.0.2.1", patroni.RoleStandbyLeader, 5, 1),
		member("b2", "10.0.2.2", patroni.RoleReplica, 5, 3),
		member("b3", "10.0.2.3", patroni.RoleReplica, 5, 2),
	)

	return a, b
}

// demotesAfter makes a PATCH that sets standby_cluster demote the leader after d.
func demotesAfter(d time.Duration, leader string) func(*fakeCluster, map[string]any) {
	return func(f *fakeCluster, patch map[string]any) {
		if patch["standby_cluster"] != nil {
			f.after(d, func(f *fakeCluster) { f.setRole(leader, patroni.RoleStandbyLeader) })
		}
	}
}

func TestDemote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := warmStandby(w)
		a.onPatch = demotesAfter(5*time.Second, "a1")

		var rec recorder

		res, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: allow, Progress: rec.add})
		require.NoError(t, err)

		assert.Equal(t, pgctl.RequestApplied, res.Request)
		assert.Equal(t, "a1", res.Leader)
		assert.Equal(t, patroni.RoleStandbyLeader, res.Role)
		assert.Empty(t, res.Detail)
		assert.Equal(t, []pgctl.Phase{pgctl.PhaseCheck, pgctl.PhaseRequest, pgctl.PhaseWait, pgctl.PhaseDone}, rec.phases())

		// The host of standby_cluster is the leader of B, then the others by
		// failover_priority, highest first, though b2 has a higher one than the leader.
		require.Equal(t, []patchCall{{endpoint: "10.0.1.1:8008", patch: map[string]any{
			"standby_cluster": map[string]any{
				"host":                   "10.0.2.1,10.0.2.2,10.0.2.3",
				"port":                   5432,
				"create_replica_methods": []string{"basebackup"},
			},
		}}}, a.patchCalls())
		assert.Empty(t, b.patchCalls())
	})
}

func TestDemoteWaitsForConsul(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := warmStandby(w)
		a.onPatch = demotesAfter(time.Second, "a1")

		w.catalog.setPrimary(dcA, "10.0.1.1")

		go func() {
			time.Sleep(30 * time.Second)
			w.catalog.clearPrimary(dcA)
		}()

		start := time.Now()

		_, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: allow})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(start), 30*time.Second)
	})
}

func TestDemoteTimeout(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world, a *fakeCluster)
	}{
		{name: "leader stays primary", setup: func(*world, *fakeCluster) {}},
		{name: "consul keeps a primary", setup: func(w *world, a *fakeCluster) {
			a.onPatch = demotesAfter(time.Second, "a1")

			w.catalog.setPrimary(dcA, "10.0.1.1")
		}},
		{name: "old leader not read", setup: func(w *world, a *fakeCluster) {
			// The leadership moved to a2 while a1, the old primary, cannot be read: it may
			// still be primary, though Consul shows none.
			w.catalog.setPrimary(dcA)

			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.m("a1").Role = patroni.ClusterRoleReplica
					f.setRole("a2", patroni.RoleStandbyLeader)
					f.readErr["a1"] = errDown
				})
			}
		}},
		{name: "another member is primary", setup: func(w *world, a *fakeCluster) {
			w.catalog.setPrimary(dcA)

			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("a1", patroni.RoleStandbyLeader)
					f.m("a2").Probe.Role = patroni.RolePrimary
				})
			}
		}},
		{name: "leader in transition", setup: func(w *world, a *fakeCluster) {
			w.catalog.setPrimary(dcA)

			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.m("a1").Role = patroni.ClusterRoleStandbyLeader
					f.m("a1").Probe.Role = patroni.RoleDemoted
				})
			}
		}},
		{name: "standby_cluster changed after the request", setup: func(_ *world, a *fakeCluster) {
			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("a1", patroni.RoleStandbyLeader)
					f.config["standby_cluster"] = map[string]any{"host": "10.0.3.1", "port": 5432}
				})
			}
		}},
		{name: "standby_cluster on another port", setup: func(_ *world, a *fakeCluster) {
			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("a1", patroni.RoleStandbyLeader)
					f.config["standby_cluster"] = map[string]any{"host": "10.0.2.1,10.0.2.2,10.0.2.3", "port": float64(5433)}
				})
			}
		}},
		{name: "cluster still shows the leader", setup: func(_ *world, a *fakeCluster) {
			// GET /cluster comes from the DCS and may lag behind GET /patroni.
			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) { f.m("a1").Probe.Role = patroni.RoleStandbyLeader })
			}
		}},
		{name: "cluster not read", setup: func(_ *world, a *fakeCluster) {
			a.onPatch = func(f *fakeCluster, _ map[string]any) {
				f.after(time.Second, func(f *fakeCluster) {
					f.setRole("a1", patroni.RoleStandbyLeader)
					f.membersErr = errDown
				})
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := warmStandby(w)
				tt.setup(w, a)

				start := time.Now()

				res, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				require.NotErrorIs(t, err, pgctl.ErrOutcomeUnknown)
				assert.Equal(t, timeouts.Demote, time.Since(start))
				assert.Equal(t, pgctl.RequestApplied, res.Request)
				assert.NotEmpty(t, res.Detail)
				assert.Len(t, a.patchCalls(), 1)
			})
		})
	}
}

func TestDemoteWrongCluster(t *testing.T) {
	tests := []struct {
		name string
		// clusters returns the clusters given as A and as B.
		clusters func(a, b *fakeCluster) (src, target pgctl.Cluster)
	}{
		{
			// The client of A finds the cluster of B, which has the same scope.
			name: "leader in the other datacenter",
			clusters: func(_, b *fakeCluster) (pgctl.Cluster, pgctl.Cluster) {
				return pgctl.Cluster{Datacenter: dcA, Patroni: b}, pgctl.Cluster{Datacenter: dcB, Patroni: b}
			},
		},
		{
			name: "target hosts in the other datacenter",
			clusters: func(a, _ *fakeCluster) (pgctl.Cluster, pgctl.Cluster) {
				return pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: a}
			},
		},
		{
			name: "same datacenter",
			clusters: func(a, _ *fakeCluster) (pgctl.Cluster, pgctl.Cluster) {
				return pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcA, Patroni: a}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := warmStandby(w)
				src, target := tt.clusters(a, b)

				res, err := w.ctl.Demote(t.Context(), src, target, pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrPrecondition)
				assert.Equal(t, pgctl.RequestNone, res.Request)
				assert.Empty(t, a.patchCalls())
				assert.Empty(t, b.patchCalls())
			})
		})
	}
}

// A leader that is standby_leader already meets every other part of the condition: the
// demotion is done once standby_cluster points at the target.
func TestDemoteFromStandbyLeader(t *testing.T) {
	tests := []struct {
		name  string
		delay time.Duration
		want  error
	}{
		{name: "slow patch", delay: 30 * time.Second},
		{name: "patch never lands", delay: time.Hour, want: pgctl.ErrOutcomeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := warmStandby(w)
				a.do(func(f *fakeCluster) {
					f.setRole("a1", patroni.RoleStandbyLeader)
					f.config["standby_cluster"] = map[string]any{"host": "10.0.3.1", "port": float64(5432)}
					f.patchDelay = tt.delay
				})

				start := time.Now()

				_, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{Guard: allow})
				if tt.want != nil {
					require.ErrorIs(t, err, tt.want)

					return
				}

				require.NoError(t, err)
				assert.GreaterOrEqual(t, time.Since(start), tt.delay)
				assert.Equal(t, "10.0.2.1,10.0.2.2,10.0.2.3", a.config["standby_cluster"].(map[string]any)["host"])
			})
		})
	}
}

func TestDemoteUnusableLeader(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *patroni.Member)
	}{
		{name: "old patroni", setup: func(m *patroni.Member) {
			m.Usable, m.UnusableReason, m.Probe.Version = false, patroni.UnusableVersion, "4.0.4"
		}},
		{name: "no https api_url", setup: func(m *patroni.Member) {
			m.APIURL, m.Endpoint = "http://10.0.1.1:8008/patroni", ""
		}},
		{name: "api_url on another host", setup: func(m *patroni.Member) {
			m.APIURL, m.Endpoint = "https://10.0.2.1:8008/patroni", "10.0.2.1:8008"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := warmStandby(w)
				a.do(func(f *fakeCluster) { tt.setup(f.m("a1")) })

				start := time.Now()

				_, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{Guard: allow})
				require.ErrorIs(t, err, pgctl.ErrPrecondition)
				assert.Zero(t, time.Since(start))
				assert.Empty(t, a.patchCalls())
				assert.Empty(t, b.patchCalls())
			})
		})
	}
}
