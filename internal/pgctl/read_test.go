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
