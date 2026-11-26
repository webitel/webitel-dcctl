package coord_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// keeping runs KeepLease for dc on the voters and returns the function that
// stops it and the channel of its result.
func keeping(t *testing.T, dc string, voters ...*leaseFake) (stop context.CancelFunc, done <-chan error) {
	t.Helper()

	vs := make([]coord.Voter, 0, len(voters))
	for _, v := range voters {
		vs = append(vs, v)
	}

	c, err := coord.New(vs, time.Second, quietLog())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan error, 1)

	go func() { out <- c.KeepLease(ctx, dc, testLeasePeriod, false) }()

	return cancel, out
}

func TestKeepLeaseRenewsWhileActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		assert.Equal(t, int64(8), counterOf(t, a))

		stop()
		require.NoError(t, <-done)
	})
}

func TestKeepLeaseWaitsWhileAnotherIsActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_b", false), kv.Lease{Holder: "dc_b", Generation: 8, Counter: 5})
		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		assert.Equal(t, 0, a.casCount(), "the lease of another datacenter is not written")

		stop()
		require.NoError(t, <-done)
	})
}

func TestKeepLeaseFollowsNewGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(5 * time.Second)
		synctest.Wait()

		// An operation made dc_a active again at generation 10.
		for _, f := range []*leaseFake{a, b, arb} {
			f.put(coord.KeyState, raw(t, 10, "dc_a", false))
		}

		time.Sleep(25 * time.Second)
		synctest.Wait()

		l, ok := a.lease(t)
		require.True(t, ok)
		assert.Equal(t, int64(10), l.Generation)

		stop()
		require.NoError(t, <-done)
	})
}

func TestKeepLeaseRenewsOnMinority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		b.set(func(f *leaseFake) { f.leaseDown = true })
		arb.set(func(f *leaseFake) { f.leaseDown = true })

		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, int64(8), counterOf(t, a), "renewed every round on the voter that takes it")

		b.set(func(f *leaseFake) { f.leaseDown = false })
		time.Sleep(10 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, counterOf(t, a), counterOf(t, b))

		stop()
		require.NoError(t, <-done)
	})
}

func TestKeepLeaseWaitsForVoters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		for _, f := range []*leaseFake{a, b, arb} {
			f.set(func(f *leaseFake) { f.down = true })
		}

		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		requireRunning(t, done)

		for _, f := range []*leaseFake{a, b, arb} {
			assert.Equal(t, 0, f.casCount(), f.name)
			f.set(func(f *leaseFake) { f.down = false })
		}

		time.Sleep(15 * time.Second)
		synctest.Wait()

		assert.Greater(t, counterOf(t, a), int64(5), "renewal starts once the voters answer")

		stop()
		require.NoError(t, <-done)
	})
}

// A forced record seen on one voter while the others do not answer does not
// tell the active datacenter: they may hold a newer one.
func TestKeepLeaseNeedsResolvedState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", true), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		b.set(func(f *leaseFake) { f.down = true })
		arb.set(func(f *leaseFake) { f.down = true })

		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, 0, a.casCount())

		stop()
		require.NoError(t, <-done)
	})
}

// The effective record stops telling the active datacenter within a renewal
// period: the rounds stop writing until it does again.
func TestKeepLeasePausesWhileStateNotResolved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", true), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		stop, done := keeping(t, "dc_a", a, b, arb)

		time.Sleep(5 * time.Second)
		synctest.Wait()

		require.Positive(t, a.casCount())

		b.set(func(f *leaseFake) { f.down = true })
		arb.set(func(f *leaseFake) { f.down = true })

		written := a.casCount()

		time.Sleep(25 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, written, a.casCount(), "no lease written while the record is not resolved")

		b.set(func(f *leaseFake) { f.down = false })
		arb.set(func(f *leaseFake) { f.down = false })

		time.Sleep(15 * time.Second)
		synctest.Wait()

		assert.Greater(t, a.casCount(), written)

		stop()
		require.NoError(t, <-done)
	})
}
