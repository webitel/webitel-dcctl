package pgctl_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// promotedB is dc_b promoted from timeline 5 at 1000, and dc_c a standby still on
// timeline 5 that received up to received.
func promotedB(w *world, received int64) (b, c *fakeCluster) {
	b = w.cluster(dcB, member("b1", "10.0.2.1", patroni.RolePrimary, 6, 1))
	b.history = []patroni.HistoryEntry{{Timeline: 5, LSN: 1000}}

	c = standbyC(w)
	c.do(func(f *fakeCluster) { f.m("c1").Probe.Xlog.ReceivedLocation = i64(received) })

	return b, c
}

func TestLeader(t *testing.T) {
	w := newWorld(t)
	_, c := promotedB(w, 900)

	l, err := w.ctl.Leader(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c})
	require.NoError(t, err)
	assert.Equal(t, "c1", l.Name)
	assert.Equal(t, patroni.RoleStandbyLeader, l.Probe.Role)
	assert.Equal(t, i64(900), l.Probe.Xlog.ReceivedLocation)
}

func TestLeader_fails(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeCluster)
	}{
		{name: "members_not_read", setup: func(f *fakeCluster) { f.membersErr = errDown }},
		{name: "leader_not_read", setup: func(f *fakeCluster) { f.readErr["c1"] = errDown }},
		{name: "no_leader", setup: func(f *fakeCluster) { f.setRole("c1", patroni.RoleReplica) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			_, c := promotedB(w, 900)
			c.do(tt.setup)

			_, err := w.ctl.Leader(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c})
			require.Error(t, err)
			assert.Contains(t, err.Error(), dcC)
		})
	}
}

func TestDivergence(t *testing.T) {
	tests := []struct {
		name     string
		received int64
		want     patroni.Divergence
	}{
		{name: "behind_the_switch", received: 1000},
		{name: "past_the_switch", received: 1500, want: patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 500}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, tt.received)

			d, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
		})
	}
}

func TestDivergence_fails(t *testing.T) {
	tests := []struct {
		name  string
		setup func(b, c *fakeCluster)
		// is is matched by the error; primary wants a primary found in dc_c.
		is      error
		primary bool
		detail  string
	}{
		{
			name:    "standby_primary",
			setup:   func(_, c *fakeCluster) { c.do(func(f *fakeCluster) { f.setRole("c1", patroni.RolePrimary) }) },
			is:      pgctl.ErrPrecondition,
			primary: true,
		},
		{
			name:  "primary_side_standby",
			setup: func(b, _ *fakeCluster) { b.do(func(f *fakeCluster) { f.setRole("b1", patroni.RoleStandbyLeader) }) },
			is:    pgctl.ErrPrecondition,
		},
		{
			name:  "history_not_read",
			setup: func(b, _ *fakeCluster) { b.do(func(f *fakeCluster) { f.historyErr = errDown }) },
			is:    errDown,
		},
		{
			name:   "history_does_not_tell",
			setup:  func(b, _ *fakeCluster) { b.do(func(f *fakeCluster) { f.history = nil }) },
			detail: "do not tell",
		},
		{
			name:  "standby_not_read",
			setup: func(_, c *fakeCluster) { c.do(func(f *fakeCluster) { f.membersErr = errDown }) },
			is:    errDown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, 1500)
			tt.setup(b, c)

			_, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.Error(t, err)

			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}

			var pe *pgctl.PrimaryError
			assert.Equal(t, tt.primary, errors.As(err, &pe))
			assert.Contains(t, err.Error(), tt.detail)
		})
	}
}

// An old primary on timeline 5 that wrote past the point dc_b left it diverged; one
// that did not follows dc_b without a rewind.
func TestDivergenceOf(t *testing.T) {
	tests := []struct {
		name     string
		location int64
		want     patroni.Divergence
	}{
		{name: "behind_the_switch", location: 1000},
		{name: "past_the_switch", location: 1800, want: patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 800}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, 0)
			c.do(func(f *fakeCluster) {
				f.setRole("c1", patroni.RolePrimary)
				f.m("c1").Probe.Xlog.ReceivedLocation = nil
				f.m("c1").Probe.Xlog.Location = i64(tt.location)
			})

			d, err := w.ctl.DivergenceOf(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
		})
	}
}

func TestDivergenceOf_fails(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(b, c *fakeCluster)
		is     error
		detail string
	}{
		{
			name:  "primary_side_standby",
			setup: func(b, _ *fakeCluster) { b.do(func(f *fakeCluster) { f.setRole("b1", patroni.RoleStandbyLeader) }) },
			is:    pgctl.ErrPrecondition,
		},
		{
			name:  "history_not_read",
			setup: func(b, _ *fakeCluster) { b.do(func(f *fakeCluster) { f.historyErr = errDown }) },
			is:    errDown,
		},
		{
			name: "primary_on_the_same_timeline",
			setup: func(_, c *fakeCluster) {
				c.do(func(f *fakeCluster) { f.setTimeline("c1", 6) })
			},
			detail: "same timeline 6",
		},
		{
			name:  "location_not_known",
			setup: func(_, c *fakeCluster) { c.do(func(f *fakeCluster) { f.m("c1").Probe.Xlog = patroni.Xlog{} }) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, 0)
			c.do(func(f *fakeCluster) {
				f.setRole("c1", patroni.RolePrimary)
				f.m("c1").Probe.Xlog.Location = i64(1800)
			})
			tt.setup(b, c)

			_, err := w.ctl.DivergenceOf(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.detail)

			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
		})
	}
}

func TestDemoted(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *fakeCluster)
		want  bool
	}{
		{
			name: "standby_of_the_target",
			setup: func(c *fakeCluster) {
				c.config["standby_cluster"] = map[string]any{"host": "10.0.2.1", "port": float64(5432)}
			},
			want: true,
		},
		{
			name: "standby_of_another_datacenter",
			setup: func(c *fakeCluster) {
				c.config["standby_cluster"] = map[string]any{"host": "10.0.2.1,10.0.9.9", "port": float64(5432)}
			},
		},
		{name: "no_standby_cluster", setup: func(*fakeCluster) {}},
		{
			name: "primary",
			setup: func(c *fakeCluster) {
				c.setRole("c1", patroni.RolePrimary)
				c.config["standby_cluster"] = map[string]any{"host": "10.0.2.1", "port": float64(5432)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, 900)
			c.do(tt.setup)

			got, err := w.ctl.Demoted(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDemoteLoss(t *testing.T) {
	tests := []struct {
		name string
		// timeline is the timeline of the primary dc_a, which left timeline 5 at 1000.
		timeline   int64
		historyErr error
		want       int64
		fails      bool
	}{
		{name: "same_timeline", timeline: 5, want: 300},
		{name: "newer_timeline", timeline: 6, want: 500},
		{name: "history_not_read", timeline: 6, historyErr: errDown, fails: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			a := w.cluster(dcA, member("a1", "10.0.1.1", patroni.RolePrimary, tt.timeline, 1))
			a.history, a.historyErr = []patroni.HistoryEntry{{Timeline: 5, LSN: 1000}}, tt.historyErr
			a.do(func(f *fakeCluster) { f.m("a1").Probe.Xlog.Location = i64(1500) })

			b := w.cluster(dcB, member("b1", "10.0.2.1", patroni.RoleStandbyLeader, 5, 1))
			b.do(func(f *fakeCluster) { f.m("b1").Probe.Xlog.ReceivedLocation = i64(1200) })

			n, err := w.ctl.DemoteLoss(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			if tt.fails {
				require.ErrorIs(t, err, tt.historyErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, n)
		})
	}
}
