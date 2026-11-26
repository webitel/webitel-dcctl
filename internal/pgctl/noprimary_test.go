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

// standbyC adds dc_c as a standby cluster.
func standbyC(w *world) *fakeCluster {
	return w.cluster(dcC, member("c1", "10.0.3.1", patroni.RoleStandbyLeader, 5, 1))
}

func clusters(dcs map[string]*fakeCluster, names ...string) []pgctl.Cluster {
	out := make([]pgctl.Cluster, 0, len(names))
	for _, n := range names {
		out = append(out, pgctl.Cluster{Datacenter: n, Patroni: dcs[n]})
	}

	return out
}

func TestNoPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := demoted(w)
		c := standbyC(w)
		dcs := map[string]*fakeCluster{dcA: a, dcB: b, dcC: c}

		res, err := w.ctl.NoPrimary(t.Context(), clusters(dcs, dcA, dcB), clusters(dcs, dcC), pgctl.Options{})
		require.NoError(t, err)
		assert.Equal(t, pgctl.ActionNoPrimary, res.Action)
		assert.Equal(t, dcA+","+dcB, res.Datacenter)
		assert.Empty(t, res.Unread)
	})
}

func TestNoPrimary_failsAtOnce(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world, dcs map[string]*fakeCluster)
		dc    string
	}{
		{name: "member_primary", dc: dcA, setup: func(_ *world, dcs map[string]*fakeCluster) {
			dcs[dcA].do(func(f *fakeCluster) { f.setRole("a1", patroni.RolePrimary) })
		}},
		{name: "consul_primary", dc: dcB, setup: func(w *world, _ map[string]*fakeCluster) {
			w.catalog.setPrimary(dcB, "10.0.2.9")
		}},
		{name: "other_primary", dc: dcC, setup: func(_ *world, dcs map[string]*fakeCluster) {
			dcs[dcC].do(func(f *fakeCluster) { f.setRole("c1", patroni.RolePrimary) })
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := demoted(w)
				dcs := map[string]*fakeCluster{dcA: a, dcB: b, dcC: standbyC(w)}
				tt.setup(w, dcs)

				start := time.Now()

				_, err := w.ctl.NoPrimary(t.Context(), clusters(dcs, dcA, dcB), clusters(dcs, dcC), pgctl.Options{})
				require.ErrorIs(t, err, pgctl.ErrPrecondition)

				var pe *pgctl.PrimaryError
				require.ErrorAs(t, err, &pe)
				assert.Equal(t, tt.dc, pe.Datacenter)
				assert.Zero(t, time.Since(start))
			})
		})
	}
}

func TestNoPrimary_unreadClusterTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := demoted(w)
		b.do(func(f *fakeCluster) { f.membersErr = errDown })
		dcs := map[string]*fakeCluster{dcA: a, dcB: b}

		start := time.Now()

		_, err := w.ctl.NoPrimary(t.Context(), clusters(dcs, dcA, dcB), nil, pgctl.Options{})
		require.ErrorIs(t, err, pgctl.ErrTimeout)
		assert.NotErrorIs(t, err, pgctl.ErrOutcomeUnknown)
		assert.Equal(t, timeouts.Catchup, time.Since(start))
	})
}

func TestNoPrimary_waitsForRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := demoted(w)
		b.do(func(f *fakeCluster) { f.membersErr = errDown })
		b.after(10*time.Second, func(f *fakeCluster) { f.membersErr = nil })
		dcs := map[string]*fakeCluster{dcA: a, dcB: b}

		start := time.Now()

		_, err := w.ctl.NoPrimary(t.Context(), clusters(dcs, dcA, dcB), nil, pgctl.Options{})
		require.NoError(t, err)
		assert.Less(t, time.Since(start), timeouts.Catchup)
	})
}

// A cluster the primitive does not need is reported, not waited for.
func TestNoPrimary_otherNotRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := demoted(w)
		c := standbyC(w)
		c.do(func(f *fakeCluster) { f.membersErr = errDown })
		dcs := map[string]*fakeCluster{dcA: a, dcB: b, dcC: c}

		start := time.Now()

		res, err := w.ctl.NoPrimary(t.Context(), clusters(dcs, dcA, dcB), clusters(dcs, dcC), pgctl.Options{})
		require.NoError(t, err)
		assert.Equal(t, []string{dcC}, res.Unread)
		assert.Zero(t, time.Since(start))
	})
}
