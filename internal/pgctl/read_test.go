package pgctl_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// ancestry is the history of a cluster that left timelines 1 to 4 at 100 × t, then the
// timelines of more.
func ancestry(more ...patroni.HistoryEntry) []patroni.HistoryEntry {
	return slices.Concat([]patroni.HistoryEntry{{Timeline: 1, LSN: 100}, {Timeline: 2, LSN: 200}, {Timeline: 3, LSN: 300}, {Timeline: 4, LSN: 400}},
		more)
}

// promotedB is dc_b promoted from timeline 5 at 1000, and dc_c a standby still on
// timeline 5 that received up to received. A standby that never was primary answers GET
// /history with nothing: Patroni writes it on a primary only.
func promotedB(w *world, received int64) (b, c *fakeCluster) {
	b = w.cluster(dcB, member("b1", "10.0.2.1", patroni.RolePrimary, 6, 1))
	b.history = ancestry(patroni.HistoryEntry{Timeline: 5, LSN: 1000})

	c = standbyC(w)
	c.do(func(f *fakeCluster) {
		f.m("c1").Probe.Xlog.ReceivedLocation = i64(received)
		f.history = []patroni.HistoryEntry{}
	})

	return b, c
}

// oldPrimaryC makes dc_c a primary on timeline 5 that wrote up to location, with the
// history it keeps current as a primary.
func oldPrimaryC(c *fakeCluster, location int64) {
	c.do(func(f *fakeCluster) {
		f.setRole("c1", patroni.RolePrimary)
		f.m("c1").Probe.Xlog.ReceivedLocation = nil
		f.m("c1").Probe.Xlog.Location = i64(location)
		f.history = ancestry()
	})
}

// pointAt points the standby_cluster of c at the hosts.
func pointAt(c *fakeCluster, hosts string) {
	c.do(func(f *fakeCluster) {
		f.config["standby_cluster"] = map[string]any{"host": hosts, "port": float64(5432)}
	})
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

// The line of a standby is the history of the cluster it streams from: dc_b itself, or
// dc_a, a primary on timeline 5 still. When that cluster is not read the history of dc_b
// alone tells, unverified.
func TestDivergence(t *testing.T) {
	tests := []struct {
		name     string
		received int64
		// upstream is the standby_cluster of dc_c; empty for none.
		upstream string
		want     patroni.Divergence
	}{
		{name: "behind_the_switch", received: 1000, want: patroni.Divergence{Unverified: true}},
		{name: "past_the_switch", received: 1500, want: patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 500, Unverified: true}},
		{name: "follows_the_primary", received: 1500, upstream: "10.0.2.1", want: patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 500}},
		{name: "follows_the_old_primary", received: 1500, upstream: "10.0.1.1", want: patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 500}},
		{name: "upstream_not_configured", received: 1500, upstream: "10.0.9.9", want: patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 500, Unverified: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, tt.received)
			a := w.cluster(dcA, member("a1", "10.0.1.1", patroni.RolePrimary, 5, 1))
			a.history = ancestry()

			if tt.upstream != "" {
				pointAt(c, tt.upstream)
			}

			d, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
				[]pgctl.Cluster{{Datacenter: dcA, Patroni: a}}, 6)
			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
		})
	}
}

// A standby on the timeline of the primary and pointed at it follows it; one whose
// upstream is not read tells nothing on that timeline.
func TestDivergence_sameTimeline(t *testing.T) {
	w := newWorld(t)
	b, c := promotedB(w, 1500)
	c.do(func(f *fakeCluster) { f.setTimeline("c1", 6) })

	_, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b}, nil, 6)
	require.Error(t, err)

	pointAt(c, "10.0.2.1")

	d, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b}, nil, 6)
	require.NoError(t, err)
	assert.Equal(t, patroni.Divergence{}, d)
}

// dc_b lost dc_a at 1000 on timeline 5 and was promoted to 6, then failed over to 7 at
// 3000. dc_c kept streaming from dc_a, followed its local failover 5 -> 6 at 1500 and
// received 2500 on that timeline 6, which dc_b never had. dc_a is gone: timeline 6 is
// not below 6, the one dc_b took on promotion, so the history of dc_b alone does not
// tell, rather than tell no divergence. On timeline 5 it still tells; with the promoted
// timeline not known, never.
func TestDivergence_unverifiedOnTheLostUpstreamTimeline(t *testing.T) {
	tests := []struct {
		name     string
		timeline int64
		shared   int64
		fails    bool
	}{
		{name: "timeline_only_the_upstream_had", timeline: 6, shared: 6, fails: true},
		{name: "timeline_before_the_failover", timeline: 5, shared: 6},
		{name: "promoted_timeline_not_known", timeline: 5, fails: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			b, c := promotedB(w, 2500)
			b.do(func(f *fakeCluster) {
				f.setTimeline("b1", 7)
				f.history = ancestry(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 3000})
			})
			c.do(func(f *fakeCluster) { f.setTimeline("c1", tt.timeline) })

			a := w.cluster(dcA, member("a1", "10.0.1.1", patroni.RolePrimary, 6, 1))
			a.do(func(f *fakeCluster) { f.membersErr = errDown })
			pointAt(c, "10.0.1.1")

			d, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
				[]pgctl.Cluster{{Datacenter: dcA, Patroni: a}}, tt.shared)

			if !tt.fails {
				require.NoError(t, err)
				assert.Equal(t, patroni.Divergence{Diverged: true, LSN: 1000, Bytes: 1500, Unverified: true}, d)

				return
			}

			require.Error(t, err)

			if tt.shared > 0 {
				assert.Contains(t, err.Error(), "rebuild dc_c by hand")
			}
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
			detail: "does not tell",
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

			_, err := w.ctl.Divergence(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b}, nil, 6)
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
			oldPrimaryC(c, tt.location)

			d, err := w.ctl.DivergenceOf(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
		})
	}
}

// dc_b promoted from 5 at 1000 to 6, then failed over 6 -> 7 at 3000; dc_c, the old
// primary, failed over locally 5 -> 6 at 1500 and wrote up to 2500 on its own timeline
// 6. The histories disagree below timeline 6, so the WAL demoting dc_c discards is not
// told, rather than told too small.
func TestDivergenceOf_bothFailedOver(t *testing.T) {
	w := newWorld(t)
	b, c := promotedB(w, 0)
	b.do(func(f *fakeCluster) {
		f.setTimeline("b1", 7)
		f.history = ancestry(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 3000})
	})
	c.do(func(f *fakeCluster) {
		f.setRole("c1", patroni.RolePrimary)
		f.setTimeline("c1", 6)
		f.m("c1").Probe.Xlog.ReceivedLocation = nil
		f.m("c1").Probe.Xlog.Location = i64(2500)
		f.history = ancestry(patroni.HistoryEntry{Timeline: 5, LSN: 1500})
	})

	_, err := w.ctl.DivergenceOf(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rebuild dc_c by hand")
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
			oldPrimaryC(c, 1800)
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

// The standby streamed from the primary, so its own GET /history, empty as it never was
// primary, is not read: the history of the primary alone tells.
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
			a.history, a.historyErr = ancestry(patroni.HistoryEntry{Timeline: 5, LSN: 1000}), tt.historyErr
			a.do(func(f *fakeCluster) { f.m("a1").Probe.Xlog.Location = i64(1500) })

			b := w.cluster(dcB, member("b1", "10.0.2.1", patroni.RoleStandbyLeader, 5, 1))
			b.history = []patroni.HistoryEntry{}
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
