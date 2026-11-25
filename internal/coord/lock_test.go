package coord_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

const testTTL = 15 * time.Second

// testLock are the default timings of coordination.
var testLock = coord.LockOptions{TTL: testTTL, LockDelay: 15 * time.Second, Timeout: 3 * time.Second}

func quietLog() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

func newLocker(t *testing.T, kvs ...coord.KV) *coord.Locker {
	t.Helper()

	l, err := coord.NewLocker(kvs, testLock, quietLog())
	require.NoError(t, err)

	return l
}

func lockHolder(instance, op string) kv.Lock {
	return kv.Lock{Instance: instance, Datacenter: "dc_a", Operation: op}
}

func twoDCs() (a, b *fakeConsul, kvs []coord.KV) {
	a, b = newFakeConsul(), newFakeConsul()

	// Not in name order: the locker sorts them.
	return a, b, []coord.KV{{Name: "dc_b", Client: b}, {Name: "dc_a", Client: a}}
}

// holdLock takes kv.LockKey in f as another instance would.
func holdLock(t *testing.T, f *fakeConsul, h kv.Lock) string {
	t.Helper()

	sid, err := f.CreateSession(t.Context(), "other")
	require.NoError(t, err)

	value, err := json.Marshal(h)
	require.NoError(t, err)

	ok, err := f.Acquire(t.Context(), kv.LockKey, value, sid)
	require.NoError(t, err)
	require.True(t, ok)

	return sid
}

func errorInfo(t *testing.T, err error) (*status.Status, *errdetails.ErrorInfo) {
	t.Helper()

	st, ok := status.FromError(err)
	require.True(t, ok, "%v is not a gRPC status", err)

	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return st, info
		}
	}

	require.Fail(t, "no ErrorInfo", "%v", st.Details())

	return st, nil
}

func outcomes(rs []coord.Released) map[string]coord.ReleaseOutcome {
	out := map[string]coord.ReleaseOutcome{}
	for _, r := range rs {
		out[r.Name] = r.Outcome
	}

	return out
}

func TestNewLockerValidates(t *testing.T) {
	f := newFakeConsul()
	one := []coord.KV{{Name: "dc_a", Client: f}}

	tests := []struct {
		name string
		kvs  []coord.KV
		opts func(o *coord.LockOptions)
	}{
		{name: "no_kvs"},
		{name: "no_name", kvs: []coord.KV{{Client: f}}},
		{name: "no_client", kvs: []coord.KV{{Name: "dc_a"}}},
		{name: "duplicate", kvs: []coord.KV{{Name: "dc_a", Client: f}, {Name: "dc_a", Client: f}}},
		{name: "zero_ttl", kvs: one, opts: func(o *coord.LockOptions) { o.TTL = 0 }},
		{name: "zero_timeout", kvs: one, opts: func(o *coord.LockOptions) { o.Timeout = 0 }},
		{name: "timeout_not_within_renewal", kvs: one, opts: func(o *coord.LockOptions) { o.Timeout = testTTL / 3 }},
		{name: "lock_delay_within_renewal", kvs: one, opts: func(o *coord.LockOptions) { o.LockDelay = testTTL/3 + o.Timeout }},
		{name: "zero_lock_delay", kvs: one, opts: func(o *coord.LockOptions) { o.LockDelay = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testLock
			if tt.opts != nil {
				tt.opts(&opts)
			}

			_, err := coord.NewLocker(tt.kvs, opts, quietLog())
			require.Error(t, err)
		})
	}

	_, err := coord.NewLocker(one, testLock, quietLog())
	require.NoError(t, err, "the default timings")
}

func TestNewKVs(t *testing.T) {
	servers := []config.ConsulServer{
		{Address: "127.0.0.1:1", Datacenter: "dc_b"},
		{Address: "127.0.0.1:2", Datacenter: "dc_a"},
	}

	newClients := func(profile config.Profile, stale bool) *consul.Clients {
		c, err := consul.New(consul.Options{
			Profile: profile, NodeMetaKey: "dc", Servers: servers,
			SessionTTL: testTTL, Timeout: testTimeout, AllowStale: stale,
		})
		require.NoError(t, err)

		return c
	}

	names := func(kvs []coord.KV) []string {
		out := make([]string, 0, len(kvs))
		for _, s := range kvs {
			out = append(out, s.Name)
		}

		return out
	}

	kvs, err := coord.NewKVs(config.ProfileWarmStandby, newClients(config.ProfileWarmStandby, false))
	require.NoError(t, err)
	assert.Equal(t, []string{"dc_a", "dc_b"}, names(kvs))

	kvs, err = coord.NewKVs(config.ProfileStretch, newClients(config.ProfileStretch, false))
	require.NoError(t, err)
	assert.Equal(t, []string{coord.StretchName}, names(kvs), "one lock and one record in the global consul")

	_, err = coord.NewKVs(config.ProfileWarmStandby, newClients(config.ProfileWarmStandby, true))
	require.Error(t, err, "stale reads are refused")
}

func TestAcquireConcurrent(t *testing.T) {
	ctx := t.Context()
	a, b, kvs := twoDCs()

	var (
		wg      sync.WaitGroup
		lockers = [2]*coord.Locker{newLocker(t, kvs...), newLocker(t, kvs...)}
		locks   [2]*coord.Lock
		errs    [2]error
	)

	for i, l := range lockers {
		wg.Go(func() {
			locks[i], errs[i] = l.Acquire(ctx, lockHolder(fmt.Sprintf("node-%d", i), fmt.Sprintf("op-%d", i)))
		})
	}

	wg.Wait()

	winner := slices.IndexFunc(errs[:], func(err error) bool { return err == nil })
	require.NotEqual(t, -1, winner, "one instance gets the lock: %v", errs)

	loser := 1 - winner

	var busy *coord.LockBusyError
	require.ErrorAs(t, errs[loser], &busy)
	assert.Nil(t, locks[loser])
	assert.Equal(t, "dc_a", busy.Name, "the first datacenter in name order is where they meet")
	assert.Equal(t, lockHolder(fmt.Sprintf("node-%d", winner), fmt.Sprintf("op-%d", winner)), busy.Holder)

	st, info := errorInfo(t, errs[loser])
	assert.Equal(t, codes.Aborted, st.Code())
	assert.Equal(t, "LOCK_BUSY", info.GetReason())
	assert.Equal(t, fmt.Sprintf("node-%d", winner), info.GetMetadata()["holder"])
	assert.Equal(t, fmt.Sprintf("operations/op-%d", winner), info.GetMetadata()["operation"])
	assert.Equal(t, "dc_a", info.GetMetadata()["datacenter"], "the datacenter of the holder")

	lk := locks[winner]
	assert.Equal(t, []string{"dc_a", "dc_b"}, lk.Held())
	assert.Empty(t, lk.Unreachable())
	assert.Len(t, a.live(), 1, "the session of the loser is destroyed")
	assert.Len(t, b.live(), 1)

	got := lk.Release(ctx)
	assert.Equal(t, map[string]coord.ReleaseOutcome{"dc_a": coord.ReleaseDone, "dc_b": coord.ReleaseDone}, outcomes(got))
	assert.Empty(t, a.holder(kv.LockKey))
	assert.Empty(t, b.holder(kv.LockKey))
	assert.Empty(t, a.live())
	assert.Empty(t, b.live())
}

func TestAcquireBusySecondReleasesFirst(t *testing.T) {
	a, b, kvs := twoDCs()
	other := lockHolder("node-b", "op-b")
	holdLock(t, b, other)

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.Nil(t, lk)

	var busy *coord.LockBusyError
	require.ErrorAs(t, err, &busy)
	assert.Equal(t, "dc_b", busy.Name)
	assert.Equal(t, other, busy.Holder)

	assert.Empty(t, a.holder(kv.LockKey), "the lock taken in dc_a is released")
	assert.Empty(t, a.live(), "and its session destroyed")
	assert.Len(t, b.live(), 1, "only the holder's session is left in dc_b")
}

func TestAcquireBusyUnknownHolder(t *testing.T) {
	a := newFakeConsul()
	holdLock(t, a, kv.Lock{})
	a.with(func(f *fakeConsul) { f.keys[kv.LockKey].value = []byte("not json") })

	_, err := newLocker(t, coord.KV{Name: "dc_a", Client: a}).Acquire(t.Context(), lockHolder("node-a", "op-a"))

	var busy *coord.LockBusyError
	require.ErrorAs(t, err, &busy)
	assert.Equal(t, kv.Lock{}, busy.Holder)

	_, info := errorInfo(t, err)
	assert.NotContains(t, info.GetMetadata(), "holder")
}

func TestAcquireLockDelay(t *testing.T) {
	a := newFakeConsul()
	a.lockDelay = true
	a.expire(holdLock(t, a, lockHolder("node-b", "op-b")))

	_, err := newLocker(t, coord.KV{Name: "dc_a", Client: a}).Acquire(t.Context(), lockHolder("node-a", "op-a"))

	var busy *coord.LockBusyError
	require.ErrorAs(t, err, &busy)
	assert.True(t, busy.LockDelay)
	assert.Equal(t, kv.Lock{}, busy.Holder, "the last holder no longer holds it")
	assert.Contains(t, err.Error(), "lock delay")

	_, info := errorInfo(t, err)
	assert.NotContains(t, info.GetMetadata(), "holder")
}

func TestAcquireNothingHeld(t *testing.T) {
	tests := []struct {
		name string
		kvs  func() []coord.KV
	}{
		{name: "warm_standby", kvs: func() []coord.KV {
			a, b, kvs := twoDCs()
			a.createErr, b.createErr = unreachable(), unreachable()

			return kvs
		}},
		{name: "stretch_minority", kvs: func() []coord.KV {
			g := newFakeConsul()
			g.createErr = unreachable()

			return []coord.KV{{Name: coord.StretchName, Client: g}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kvs := tt.kvs()

			lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
			require.Nil(t, lk, "no operation runs without a lock")

			var none *coord.LockUnavailableError
			require.ErrorAs(t, err, &none)
			assert.Len(t, none.Unreachable, len(kvs))
			assert.Equal(t, codes.Unavailable, status.Code(err))
		})
	}
}

func TestAcquireStretchOneLock(t *testing.T) {
	g := newFakeConsul()

	lk, err := newLocker(t, coord.KV{Name: coord.StretchName, Client: g}).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	assert.Equal(t, []string{coord.StretchName}, lk.Held())
	assert.Len(t, g.live(), 1, "one session")
	assert.NotEmpty(t, g.holder(kv.LockKey))

	lk.Release(t.Context())
}

func TestAcquireSkipsUnreachable(t *testing.T) {
	a, b, kvs := twoDCs()
	b.createErr = unreachable()

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	assert.Equal(t, []string{"dc_a"}, lk.Held())
	require.Len(t, lk.Unreachable(), 1)
	assert.Equal(t, "dc_b", lk.Unreachable()[0].Name)
	assert.Equal(t, source.ReasonRefused, lk.Unreachable()[0].Reason)
	assert.NotEmpty(t, a.holder(kv.LockKey))

	lk.Release(t.Context())
}

func TestAcquireErrorDestroysSession(t *testing.T) {
	_, b, kvs := twoDCs()
	b.acquireErr = unreachable()
	b.acquireApplied = true

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	assert.Equal(t, []string{"dc_a"}, lk.Held())
	require.Len(t, lk.Unreachable(), 1)
	assert.Equal(t, "dc_b", lk.Unreachable()[0].Name)
	assert.Len(t, b.destroyed, 1, "the session that may hold the lock is destroyed")
	assert.Empty(t, b.holder(kv.LockKey))

	lk.Release(t.Context())
}

func TestAcquireSessionLost(t *testing.T) {
	a, b, kvs := twoDCs()
	b.forgetSessions = true

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.Nil(t, lk)
	require.ErrorIs(t, err, coord.ErrLockLost)
	require.ErrorIs(t, err, consul.ErrSessionNotFound)

	assert.Empty(t, a.holder(kv.LockKey), "the lock taken in dc_a is released")
	assert.Empty(t, a.live())
}

func TestAcquireCanceledRollsBack(t *testing.T) {
	a, _, kvs := twoDCs()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	a.onAcquire = cancel

	lk, err := newLocker(t, kvs...).Acquire(ctx, lockHolder("node-a", "op-a"))
	require.Nil(t, lk)
	require.ErrorIs(t, err, context.Canceled)

	assert.Empty(t, a.holder(kv.LockKey), "the lock is released although ctx ended")
	assert.Empty(t, a.live())
}

func TestAcquireLostWhileAcquiring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, kvs := twoDCs()

		// dc_b answers slowly, and the session taken in dc_a ends meanwhile.
		b.onCreate = func() {
			a.expire(a.live()[0])
			time.Sleep(testTTL / 2)
		}

		lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
		require.Nil(t, lk)
		require.ErrorIs(t, err, coord.ErrLockLost, "renewal starts with the first lock taken")

		assert.Empty(t, b.holder(kv.LockKey), "the lock taken in dc_b is released")
		assert.Empty(t, b.live())
	})
}

func TestAcquireHolderRequired(t *testing.T) {
	l := newLocker(t, coord.KV{Name: "dc_a", Client: newFakeConsul()})

	_, err := l.Acquire(t.Context(), kv.Lock{Operation: "op-a"})
	require.Error(t, err)

	_, err = l.Acquire(t.Context(), kv.Lock{Instance: "node-a"})
	require.Error(t, err)
}

func TestLockLostOnRenew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, kvs := twoDCs()

		lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
		require.NoError(t, err)

		time.Sleep(3 * testTTL)
		synctest.Wait()

		select {
		case <-lk.Lost():
			require.Fail(t, "renewed sessions are not lost", "%v", lk.Err())
		default:
		}

		require.NoError(t, lk.Err())

		a.expire(a.live()[0])

		time.Sleep(testTTL / 2)
		synctest.Wait()

		select {
		case <-lk.Lost():
		default:
			require.Fail(t, "an expired session is reported")
		}

		require.ErrorIs(t, lk.Err(), coord.ErrLockLost)
		require.ErrorIs(t, lk.Err(), consul.ErrSessionNotFound)
		assert.Contains(t, lk.Err().Error(), "dc_a")

		got := lk.Release(t.Context())
		assert.Equal(t, map[string]coord.ReleaseOutcome{"dc_a": coord.ReleaseNotHeld, "dc_b": coord.ReleaseDone}, outcomes(got))
		assert.Empty(t, b.live())
	})
}

func TestLockUnconfirmed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, b, kvs := twoDCs()

		lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
		require.NoError(t, err)

		b.with(func(f *fakeConsul) { f.renewErr = unreachable() })

		time.Sleep(testTTL / 2)
		synctest.Wait()
		require.NoError(t, lk.Err(), "one failed renewal is not a lost lock")

		time.Sleep(testTTL)
		synctest.Wait()

		require.ErrorIs(t, lk.Err(), coord.ErrLockLost, "a session not renewed for its ttl may be gone")
		assert.Contains(t, lk.Err().Error(), "dc_b")

		b.with(func(f *fakeConsul) { f.renewErr = nil })
		lk.Release(t.Context())
	})
}

func TestLockNotLostAfterRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, _, kvs := twoDCs()

		lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
		require.NoError(t, err)

		lk.Release(t.Context())

		time.Sleep(3 * testTTL)
		synctest.Wait()

		select {
		case <-lk.Lost():
			require.Fail(t, "a released lock is not lost", "%v", lk.Err())
		default:
		}
	})
}

func TestReleaseCanceledContext(t *testing.T) {
	a, b, kvs := twoDCs()

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got := lk.Release(ctx)
	assert.Equal(t, map[string]coord.ReleaseOutcome{"dc_a": coord.ReleaseDone, "dc_b": coord.ReleaseDone}, outcomes(got))
	assert.Empty(t, a.holder(kv.LockKey))
	assert.Empty(t, b.holder(kv.LockKey))
}

func TestReleaseAmbiguous(t *testing.T) {
	_, b, kvs := twoDCs()

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	b.with(func(f *fakeConsul) { f.releaseErr = outcomeUnknown() })

	got := lk.Release(t.Context())
	assert.Equal(t, map[string]coord.ReleaseOutcome{"dc_a": coord.ReleaseDone, "dc_b": coord.ReleaseUnknown}, outcomes(got))

	i := slices.IndexFunc(got, func(r coord.Released) bool { return r.Name == "dc_b" })
	require.ErrorIs(t, got[i].Err, consul.ErrOutcomeUnknown)
	require.ErrorIs(t, got[i].Err, coord.ErrOutcomeUnknown)
	assert.True(t, got[i].SessionDestroyed, "destroying the session frees the key after the lock delay")
	assert.Empty(t, b.holder(kv.LockKey))

	assert.Equal(t, got, lk.Release(t.Context()), "a second release reports the first")
}

func TestReleaseNotHeld(t *testing.T) {
	a, _, kvs := twoDCs()

	lk, err := newLocker(t, kvs...).Acquire(t.Context(), lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	a.expire(a.live()[0])

	got := lk.Release(t.Context())
	assert.Equal(t, map[string]coord.ReleaseOutcome{"dc_a": coord.ReleaseNotHeld, "dc_b": coord.ReleaseDone}, outcomes(got))
}
