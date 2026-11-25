package pgctl_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// remoteGuard is a guard that asks a remote store and takes d to answer.
func remoteGuard(d time.Duration) pgctl.Guard {
	return func(ctx context.Context) error {
		t := time.NewTimer(d)
		defer t.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}

func TestGuard(t *testing.T) {
	tests := []struct {
		name    string
		guard   pgctl.Guard
		want    error
		notWant error
	}{
		{name: "refuses", guard: func(context.Context) error { return errLost }, want: pgctl.ErrGuard, notWant: pgctl.ErrTimeout},
		{name: "cut by the deadline", guard: remoteGuard(time.Hour), want: pgctl.ErrTimeout, notWant: pgctl.ErrGuard},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				a, b := warmStandby(w)

				res, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
					pgctl.Options{Guard: tt.guard})
				require.ErrorIs(t, err, tt.want)
				require.NotErrorIs(t, err, tt.notWant)
				assert.Equal(t, pgctl.RequestNone, res.Request)
				assert.Empty(t, a.patchCalls())
			})
		})
	}
}

func TestNoGuard(t *testing.T) {
	w := newWorld(t)
	a, b := warmStandby(w)

	_, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{})
	require.Error(t, err)
	assert.Empty(t, a.patchCalls())
	assert.Empty(t, b.patchCalls())
}

// A PATCH that fails before it reaches Patroni is sent again, guarded again each time.
func TestPatchResent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := warmStandby(w)
		a.onPatch = demotesAfter(time.Second, "a1")
		a.patchErr = errDown

		go func() {
			time.Sleep(25 * time.Second)
			a.do(func(f *fakeCluster) { f.patchErr = nil })
		}()

		var guards atomic.Int32

		guard := func(context.Context) error {
			guards.Add(1)

			return nil
		}

		res, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: guard})
		require.NoError(t, err)
		assert.Equal(t, pgctl.RequestApplied, res.Request)

		calls := a.patchCalls()
		assert.Len(t, calls, 4, "sent at 0s, 10s, 20s and 30s, the last one applied")
		assert.Equal(t, int32(len(calls)), guards.Load())
	})
}

// A PATCH answered with an error after Patroni applied it ends as done: the condition
// is what counts.
func TestPatchUnknownButApplied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		a, b := warmStandby(w)
		a.onPatch = demotesAfter(time.Second, "a1")
		a.patchErr, a.patchApplies = errDown, true

		res, err := w.ctl.Demote(t.Context(), pgctl.Cluster{Datacenter: dcA, Patroni: a}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: allow})
		require.NoError(t, err)
		assert.Equal(t, pgctl.RequestUnknown, res.Request)
		assert.Len(t, a.patchCalls(), 1)
	})
}

func TestPatchUnknownTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		_, b := warmStandby(w)
		b.patchErr = errDown

		start := time.Now()

		res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrOutcomeUnknown)
		require.ErrorIs(t, err, pgctl.ErrTimeout)
		assert.Equal(t, timeouts.Promote, time.Since(start))
		assert.Equal(t, pgctl.RequestUnknown, res.Request)
	})
}

func TestPatchRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		_, b := warmStandby(w)
		b.patchErr = refusal(http.StatusConflict)

		res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrRefused)
		assert.Equal(t, pgctl.RequestRefused, res.Request)
		assert.Len(t, b.patchCalls(), 1)
	})
}

// The parent context ends the primitive with its own cause.
func TestCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		_, b := warmStandby(w)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		_, err := w.ctl.Promote(ctx, pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotErrorIs(t, err, pgctl.ErrTimeout)
	})
}

// Once an attempt had an unknown outcome, a refusal of a later one may be the conflict
// with that attempt applied: the outcome stays unknown and the condition is polled on.
func TestRefusalAfterUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		_, b := warmStandby(w)
		b.patchErr = errDown

		go func() {
			time.Sleep(5 * time.Second)
			b.do(func(f *fakeCluster) { f.patchErr = refusal(http.StatusConflict) })
		}()

		start := time.Now()

		res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrOutcomeUnknown)
		require.NotErrorIs(t, err, pgctl.ErrRefused)
		assert.Equal(t, timeouts.Promote, time.Since(start))
		assert.Equal(t, pgctl.RequestUnknown, res.Request)
		assert.Greater(t, len(b.patchCalls()), 2)
	})
}

// A refused request whose condition is met already ends as done.
func TestRefusedButDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		_, b := warmStandby(w)
		b.onPatch = promotesAfter(0, "b1", 6)
		b.patchErr, b.patchApplies = refusal(http.StatusConflict), true

		res, err := w.ctl.Promote(t.Context(), pgctl.Cluster{Datacenter: dcB, Patroni: b}, pgctl.Options{Guard: allow})
		require.NoError(t, err)
		assert.Equal(t, pgctl.RequestRefused, res.Request)
		assert.Len(t, b.patchCalls(), 1)
	})
}

// The check before a resend refuses a cluster that changed meanwhile.
func TestResendPrecondition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		b, c := promoted(w)
		c.patchErr = errDown
		c.after(5*time.Second, func(f *fakeCluster) { f.setRole("c1", patroni.RolePrimary) })

		res, err := w.ctl.Repoint(t.Context(), pgctl.Cluster{Datacenter: dcC, Patroni: c}, pgctl.Cluster{Datacenter: dcB, Patroni: b},
			pgctl.Options{Guard: allow})
		require.ErrorIs(t, err, pgctl.ErrPrecondition)
		require.ErrorIs(t, err, pgctl.ErrOutcomeUnknown)
		assert.Equal(t, pgctl.RequestUnknown, res.Request)
		assert.Len(t, c.patchCalls(), 1)
	})
}
