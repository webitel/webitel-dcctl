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

// demoted is A demoted at replayed_location 1000 with B behind it.
func demoted(w *world) (a, b *fakeCluster) {
	a, b = warmStandby(w)
	a.do(func(f *fakeCluster) {
		f.setRole("a1", patroni.RoleStandbyLeader)
		f.m("a1").Probe.Xlog = patroni.Xlog{ReceivedLocation: i64(1000), ReplayedLocation: i64(1000)}
	})
	b.do(func(f *fakeCluster) {
		f.m("b1").Probe.Xlog = patroni.Xlog{ReceivedLocation: i64(900), ReplayedLocation: i64(800)}
	})

	return a, b
}

func setXlog(name string, received, replayed *int64) func(*fakeCluster) {
	return func(f *fakeCluster) {
		f.m(name).Probe.Xlog = patroni.Xlog{ReceivedLocation: received, ReplayedLocation: replayed}
	}
}

func TestCatchup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := demoted(w)
		b.after(10*time.Second, setXlog("b1", i64(1000), i64(990)))
		b.after(20*time.Second, setXlog("b1", i64(1000), i64(1000)))

		var rec recorder

		start := time.Now()

		res, err := w.ctl.Catchup(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Progress: rec.add})
		require.NoError(t, err)
		assert.Equal(t, 20*time.Second, time.Since(start))
		assert.Equal(t, pgctl.RequestNone, res.Request)
		assert.Equal(t, "b1", res.Member)
		assert.Equal(t, i64(0), res.Lag)
		assert.Equal(t, []pgctl.Phase{pgctl.PhaseCheck, pgctl.PhaseWait, pgctl.PhaseDone}, rec.phases())

		// The lag behind A is reported while B catches up.
		assert.Equal(t, i64(200), rec.seen[1].Lag)
	})
}

func TestCatchupTimeout(t *testing.T) {
	tests := []struct {
		name  string
		setup func(a, b *fakeCluster)
	}{
		{name: "replay behind", setup: func(_, b *fakeCluster) {
			b.do(setXlog("b1", i64(1000), i64(999)))
		}},
		{name: "received behind", setup: func(_, b *fakeCluster) {
			b.do(setXlog("b1", i64(999), i64(999)))
		}},
		{name: "target positions unknown", setup: func(_, b *fakeCluster) {
			b.do(setXlog("b1", nil, nil))
		}},
		{name: "target replay unknown", setup: func(_, b *fakeCluster) {
			b.do(setXlog("b1", i64(1000), nil))
		}},
		{name: "demoted position unknown", setup: func(a, b *fakeCluster) {
			a.do(setXlog("a1", nil, nil))
			b.do(setXlog("b1", i64(1000), i64(1000)))
		}},
		{name: "demoted position goes back", setup: func(a, b *fakeCluster) {
			a.after(time.Second, setXlog("a1", i64(900), i64(900)))
			b.do(setXlog("b1", i64(950), i64(950)))
		}},
		{name: "demoted leader not read", setup: func(a, b *fakeCluster) {
			a.do(func(f *fakeCluster) { f.readErr["a1"] = errDown })
			b.do(setXlog("b1", i64(1000), i64(1000)))
		}},
		{name: "target cluster not read", setup: func(_, b *fakeCluster) {
			b.do(setXlog("b1", i64(1000), i64(1000)))
			b.do(func(f *fakeCluster) { f.membersErr = errDown })
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := demoted(w)
				tt.setup(a, b)

				start := time.Now()

				res, err := w.ctl.Catchup(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{})
				require.ErrorIs(t, err, pgctl.ErrTimeout)
				assert.Equal(t, timeouts.Catchup, time.Since(start))
				assert.NotEmpty(t, res.Detail)
			})
		})
	}
}

// A primary still takes writes: there is no end to catch up to.
func TestCatchupDemotedIsPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := demoted(w)
		a.do(func(f *fakeCluster) { f.setRole("a1", patroni.RolePrimary) })

		_, err := w.ctl.Catchup(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
	})
}

func TestCatchupPrecondition(t *testing.T) {
	tests := []struct {
		name  string
		setup func(a, b *fakeCluster)
	}{
		{name: "target is primary", setup: func(_, b *fakeCluster) {
			b.do(func(f *fakeCluster) { f.setRole("b1", patroni.RolePrimary) })
		}},
		{name: "target on another timeline", setup: func(_, b *fakeCluster) {
			// Positions on timeline 6 say nothing about the end of timeline 5.
			b.do(func(f *fakeCluster) {
				f.setTimeline("b1", 6)
				setXlog("b1", i64(2000), i64(2000))(f)
			})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := demoted(w)
				tt.setup(a, b)

				_, err := w.ctl.Catchup(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{})
				require.ErrorIs(t, err, pgctl.ErrPrecondition)
			})
		})
	}
}
