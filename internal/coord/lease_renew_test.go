package coord_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

const testLeasePeriod = 30 * time.Second

// renewing runs RenewLease for holderA on the voters and returns the
// recorder of its log, the function that stops it and the channel of its
// result.
func renewing(t *testing.T, voters ...*leaseFake) (rec *logtest.Recorder, stop context.CancelFunc, done <-chan error) {
	t.Helper()

	vs := make([]coord.Voter, 0, len(voters))
	for _, v := range voters {
		vs = append(vs, v)
	}

	rec = logtest.NewRecorder()

	c, err := coord.New(vs, time.Second, logging.New(logging.WithProvider(rec)))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan error, 1)

	go func() { out <- c.RenewLease(ctx, holderA, testLeasePeriod) }()

	return rec, cancel, out
}

func logged(rec *logtest.Recorder, body string) int {
	n := 0

	for _, r := range rec.Records() {
		if r.Body().AsString() == body {
			n++
		}
	}

	return n
}

func counterOf(t *testing.T, f *leaseFake) int64 {
	t.Helper()

	l, ok := f.lease(t)
	require.True(t, ok, f.name)

	return l.Counter
}

func requireRunning(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		require.FailNow(t, "renewal returned", "%v", err)
	default:
	}
}

func TestRenewLeaseEveryThirdOfPeriod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		_, stop, done := renewing(t, a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		for _, f := range []*leaseFake{a, b, arb} {
			assert.Equal(t, int64(8), counterOf(t, f), f.name)
		}

		stop()
		require.NoError(t, <-done)
	})
}

func TestRenewLeaseSurvivesMajorityDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		b.set(func(f *leaseFake) { f.leaseDown = true })
		arb.set(func(f *leaseFake) { f.leaseDown = true })

		rec, stop, done := renewing(t, a, b, arb)

		time.Sleep(15 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, 2, logged(rec, "lease not renewed"))

		b.set(func(f *leaseFake) { f.leaseDown = false })
		arb.set(func(f *leaseFake) { f.leaseDown = false })

		time.Sleep(10 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, int64(8), counterOf(t, b))
		assert.Equal(t, int64(8), counterOf(t, arb))
		assert.Equal(t, 1, logged(rec, "lease renewed again"))

		stop()
		require.NoError(t, <-done)
	})
}

func TestRenewLeaseOneVoterDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		arb.set(func(f *leaseFake) { f.down = true })

		rec, stop, done := renewing(t, a, b, arb)

		time.Sleep(15 * time.Second)
		synctest.Wait()

		assert.Equal(t, int64(7), counterOf(t, b))
		assert.Zero(t, logged(rec, "lease not renewed"))

		stop()
		require.NoError(t, <-done)
	})
}

func TestRenewLeaseHangingVoter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		b.set(func(f *leaseFake) { f.hang = true })

		rec, stop, done := renewing(t, a, b, arb)

		time.Sleep(25 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, int64(8), counterOf(t, a))
		assert.Equal(t, int64(8), counterOf(t, arb))
		assert.Zero(t, logged(rec, "lease not renewed"))

		stop()
		require.NoError(t, <-done)
	})
}

func TestRenewLeaseStopsWhenNotHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		_, stop, done := renewing(t, a, b, arb)

		defer stop()

		time.Sleep(5 * time.Second)
		synctest.Wait()

		for _, f := range []*leaseFake{a, b, arb} {
			f.put(coord.KeyState, raw(t, 9, "dc_b", false))
		}

		time.Sleep(10 * time.Second)
		synctest.Wait()

		select {
		case err := <-done:
			require.ErrorIs(t, err, coord.ErrNotLeaseHolder)
		default:
			require.FailNow(t, "renewal did not stop")
		}

		casBefore := a.casCount()

		time.Sleep(30 * time.Second)
		synctest.Wait()

		assert.Equal(t, casBefore, a.casCount())
		assert.Equal(t, int64(6), counterOf(t, a))
	})
}

func TestRenewLeaseSkipsUndeterminedState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		b.set(func(f *leaseFake) { f.down = true })
		arb.set(func(f *leaseFake) { f.down = true })

		_, stop, done := renewing(t, a, b, arb)

		time.Sleep(5 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Zero(t, a.casCount())

		b.set(func(f *leaseFake) { f.down = false })
		arb.set(func(f *leaseFake) { f.down = false })

		time.Sleep(10 * time.Second)
		synctest.Wait()

		requireRunning(t, done)
		assert.Equal(t, int64(6), counterOf(t, b))

		stop()
		require.NoError(t, <-done)
	})
}

func TestRenewLeaseStopsOnContextEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
		_, stop, done := renewing(t, a, b, arb)

		synctest.Wait()
		stop()

		require.NoError(t, <-done)
	})
}

func TestRenewLeaseValidates(t *testing.T) {
	a, b, arb := threeVoters(t, raw(t, 8, "dc_a", false), kv.Lease{Holder: "dc_a", Generation: 8, Counter: 5})
	c := leaseCoordinator(t, a, b, arb)

	tests := []struct {
		name   string
		holder coord.LeaseHolder
		period time.Duration
	}{
		{name: "interval_not_above_timeout", holder: holderA, period: 3 * time.Second},
		{name: "zero_period", holder: holderA},
		{name: "no_datacenter", holder: coord.LeaseHolder{Generation: 8}, period: testLeasePeriod},
		{name: "zero_generation", holder: coord.LeaseHolder{Datacenter: "dc_a"}, period: testLeasePeriod},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, c.RenewLease(t.Context(), tt.holder, tt.period))
		})
	}

	assert.Zero(t, a.casCount())
}
