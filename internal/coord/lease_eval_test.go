package coord_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// window is T + acquire_after of the defaults.
const window = 60 * time.Second

func effectiveOf(gen int64, dc string) *coord.Record {
	return &coord.Record{State: kv.State{Generation: gen, ActiveDatacenter: dc}}
}

// leaseObserver feeds reads to EvaluateLease the way an instance does on
// every collection cycle. It runs in a synctest bubble, whose clock it
// moves.
type leaseObserver struct {
	t         *testing.T
	start     time.Time
	tl        coord.LeaseTimeline
	effective *coord.Record
}

func newLeaseObserver(t *testing.T) *leaseObserver {
	t.Helper()

	tl, err := coord.NewLeaseTimeline(30*time.Second, 30*time.Second)
	require.NoError(t, err)

	return &leaseObserver{t: t, start: time.Now(), tl: tl, effective: effectiveOf(8, "dc_a")}
}

// at evaluates voters at offset after the start of the observer.
func (o *leaseObserver) at(offset time.Duration, voters ...coord.VoterLease) coord.LeaseStatus {
	o.t.Helper()

	time.Sleep(time.Until(o.start.Add(offset)))

	var st coord.LeaseStatus

	st, o.tl = coord.EvaluateLease(o.tl, voters, o.effective)

	return st
}

func (o *leaseObserver) requireSince(offset time.Duration, st coord.LeaseStatus) {
	o.t.Helper()

	want := o.start.Add(offset)
	assert.True(o.t, want.Equal(st.Since), "since %s, want %s", st.Since, want)
}

func held(t *testing.T, name string, kind coord.Kind, l kv.Lease) coord.VoterLease {
	t.Helper()

	return coord.PresentLease(name, kind, leaseJSON(t, l), 10)
}

func leaseDown(name string, kind coord.Kind) coord.VoterLease {
	return coord.VoterLease{Name: name, Kind: kind, Status: coord.StatusUnreachable, Err: unreachable()}
}

func leaseA(counter int64) kv.Lease {
	return kv.Lease{Holder: "dc_a", Generation: 8, Counter: counter}
}

// all3 returns the reads of dc_a, dc_b and the arbiter holding the leases.
func all3(t *testing.T, a, b, arb kv.Lease) []coord.VoterLease {
	t.Helper()

	return []coord.VoterLease{
		held(t, "dc_a", coord.KindKV, a),
		held(t, "dc_b", coord.KindKV, b),
		held(t, coord.ArbiterName, coord.KindArbiter, arb),
	}
}

func same3(t *testing.T, l kv.Lease) []coord.VoterLease {
	t.Helper()

	return all3(t, l, l, l)
}

func assertLease(t *testing.T, state coord.LeaseState, reason coord.LeaseReason, got coord.LeaseStatus) {
	t.Helper()

	assert.Equal(t, state, got.State)
	assert.Equal(t, reason, got.Reason)
}

func TestNewLeaseTimelineValidates(t *testing.T) {
	for _, d := range [][2]time.Duration{{0, time.Second}, {time.Second, 0}, {-time.Second, time.Second}} {
		_, err := coord.NewLeaseTimeline(d[0], d[1])
		require.Error(t, err, "%v", d)
	}
}

func TestEvaluateLeaseHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)

		got := o.at(10*time.Second, same3(t, leaseA(6))...)

		assertLease(t, coord.LeaseHeld, coord.LeaseReasonRenewed, got)
		assert.Equal(t, leaseA(6), got.Lease)
		o.requireSince(10*time.Second, got)
	})
}

func TestEvaluateLeaseFirstSight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)

		got := o.at(0, same3(t, leaseA(5))...)
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)
		assert.Equal(t, leaseA(5), got.Lease)
		o.requireSince(0, got)

		got = o.at(window, same3(t, leaseA(5))...)
		assert.Equal(t, coord.LeaseUnknown, got.State)

		got = o.at(window+time.Nanosecond, same3(t, leaseA(5))...)
		assertLease(t, coord.LeaseLost, coord.LeaseReasonCounterStale, got)
	})
}

func TestEvaluateLeaseCounterStaleBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)

		// The change is seen at 10s; it happened after the read at 0.
		changed := 10 * time.Second
		o.at(changed, same3(t, leaseA(6))...)

		got := o.at(window, same3(t, leaseA(6))...)
		assertLease(t, coord.LeaseHeld, coord.LeaseReasonRenewed, got)

		got = o.at(window+time.Nanosecond, same3(t, leaseA(6))...)
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)

		got = o.at(changed+window, same3(t, leaseA(6))...)
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)

		got = o.at(changed+window+time.Nanosecond, same3(t, leaseA(6))...)
		assertLease(t, coord.LeaseLost, coord.LeaseReasonCounterStale, got)
		assert.Equal(t, leaseA(6), got.Lease)
		o.requireSince(changed, got)
	})
}

func TestEvaluateLeaseMismatchBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.effective = effectiveOf(9, "dc_b")

		o.at(0, same3(t, leaseA(5))...)

		got := o.at(10*time.Second, same3(t, leaseA(6))...)
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)

		got = o.at(window, same3(t, leaseA(7))...)
		assert.Equal(t, coord.LeaseUnknown, got.State)

		got = o.at(window+time.Nanosecond, same3(t, leaseA(8))...)
		assertLease(t, coord.LeaseLost, coord.LeaseReasonHolderMismatch, got)
		assert.Equal(t, leaseA(8), got.Lease)
	})
}

func TestEvaluateLeaseMismatchGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.effective = effectiveOf(9, "dc_a")

		o.at(0, same3(t, leaseA(5))...)

		got := o.at(window+time.Nanosecond, same3(t, leaseA(9))...)
		assertLease(t, coord.LeaseLost, coord.LeaseReasonHolderMismatch, got)
	})
}

func TestEvaluateLeaseNoQuorum(t *testing.T) {
	t.Run("no_effective_state", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := newLeaseObserver(t)
			o.effective = nil

			o.at(0, same3(t, leaseA(5))...)
			got := o.at(2*window, same3(t, leaseA(5))...)

			assertLease(t, coord.LeaseUnknown, coord.LeaseReasonStateUndetermined, got)
			assert.Equal(t, leaseA(5), got.Lease)
		})
	})

	t.Run("reads_on_a_minority", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := newLeaseObserver(t)
			o.at(0, same3(t, leaseA(5))...)

			got := o.at(2*window,
				held(t, "dc_a", coord.KindKV, leaseA(5)),
				leaseDown("dc_b", coord.KindKV),
				leaseDown(coord.ArbiterName, coord.KindArbiter))

			assertLease(t, coord.LeaseUnknown, coord.LeaseReasonNoMajority, got)
		})
	})

	t.Run("uninitialized_arbiter_does_not_vote", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := newLeaseObserver(t)
			uninitialized := coord.VoterLease{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUninitialized}
			o.at(0, held(t, "dc_a", coord.KindKV, leaseA(5)), leaseDown("dc_b", coord.KindKV), uninitialized)

			got := o.at(10*time.Second,
				held(t, "dc_a", coord.KindKV, leaseA(6)), leaseDown("dc_b", coord.KindKV), uninitialized)

			assertLease(t, coord.LeaseUnknown, coord.LeaseReasonNoMajority, got)
		})
	})
}

func TestEvaluateLeaseTwoVoters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, held(t, "dc_a", coord.KindKV, leaseA(5)), held(t, "dc_b", coord.KindKV, leaseA(5)))

		got := o.at(10*time.Second, held(t, "dc_a", coord.KindKV, leaseA(6)), held(t, "dc_b", coord.KindKV, leaseA(6)))
		assertLease(t, coord.LeaseHeld, coord.LeaseReasonRenewed, got)

		// dc_a is gone with its KV; dc_b sees its own KV stop.
		got = o.at(30*time.Second, leaseDown("dc_a", coord.KindKV), held(t, "dc_b", coord.KindKV, leaseA(6)))
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonNoMajority, got)

		got = o.at(10*time.Second+window+time.Nanosecond,
			leaseDown("dc_a", coord.KindKV), held(t, "dc_b", coord.KindKV, leaseA(6)))
		assertLease(t, coord.LeaseLost, coord.LeaseReasonCounterStale, got)
	})
}

func TestEvaluateLeaseOneVoter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, held(t, coord.StretchName, coord.KindKV, leaseA(5)))

		got := o.at(10*time.Second, held(t, coord.StretchName, coord.KindKV, leaseA(6)))
		assertLease(t, coord.LeaseHeld, coord.LeaseReasonRenewed, got)

		got = o.at(10*time.Second+window+time.Nanosecond, held(t, coord.StretchName, coord.KindKV, leaseA(6)))
		assertLease(t, coord.LeaseLost, coord.LeaseReasonCounterStale, got)

		got = o.at(2*window, leaseDown(coord.StretchName, coord.KindKV))
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonNoMajority, got)
	})
}

func TestEvaluateLeaseCounterChangeClearsLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)

		got := o.at(window+time.Nanosecond, same3(t, leaseA(5))...)
		require.Equal(t, coord.LeaseLost, got.State)

		got = o.at(window+time.Second, same3(t, leaseA(6))...)
		assertLease(t, coord.LeaseHeld, coord.LeaseReasonRenewed, got)
	})
}

func TestEvaluateLeaseUnreachableNeverHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)
		o.at(10*time.Second, all3(t, leaseA(6), leaseA(5), leaseA(6))...)

		// The arbiter was last seen renewed; unreachable now, that proves
		// nothing.
		got := o.at(20*time.Second,
			held(t, "dc_a", coord.KindKV, leaseA(7)),
			held(t, "dc_b", coord.KindKV, leaseA(5)),
			leaseDown(coord.ArbiterName, coord.KindArbiter))

		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)
	})
}

func TestEvaluateLeaseVoterBackAfterGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)

		for offset := 10 * time.Second; offset < 5*time.Minute; offset += 10 * time.Second {
			o.at(offset,
				leaseDown("dc_a", coord.KindKV),
				leaseDown("dc_b", coord.KindKV),
				held(t, coord.ArbiterName, coord.KindArbiter, leaseA(5)))
		}

		// Both KVs answer again with what the holder wrote before it died;
		// when that was is unknown.
		back := 5 * time.Minute
		got := o.at(back, all3(t, leaseA(40), leaseA(40), leaseA(5))...)
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)

		got = o.at(back+window, all3(t, leaseA(40), leaseA(40), leaseA(5))...)
		assert.Equal(t, coord.LeaseUnknown, got.State)

		got = o.at(back+window+time.Nanosecond, all3(t, leaseA(40), leaseA(40), leaseA(5))...)
		assertLease(t, coord.LeaseLost, coord.LeaseReasonCounterStale, got)
	})
}

func TestEvaluateLeaseVoterBackAfterGapRenewing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)
		o.at(5*time.Minute, leaseDown("dc_a", coord.KindKV), leaseDown("dc_b", coord.KindKV),
			leaseDown(coord.ArbiterName, coord.KindArbiter))

		got := o.at(5*time.Minute+10*time.Second, all3(t, leaseA(40), leaseA(40), leaseA(40))...)
		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)

		got = o.at(5*time.Minute+20*time.Second, all3(t, leaseA(41), leaseA(41), leaseA(41))...)
		assertLease(t, coord.LeaseHeld, coord.LeaseReasonRenewed, got)
	})
}

func TestEvaluateLeaseMinorityRenewalIsLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)

		got := o.at(30*time.Second, all3(t, leaseA(6), leaseA(5), leaseA(5))...)
		assert.Equal(t, coord.LeaseUnknown, got.State)

		got = o.at(window+time.Nanosecond, all3(t, leaseA(7), leaseA(5), leaseA(5))...)
		assertLease(t, coord.LeaseLost, coord.LeaseReasonCounterStale, got)
		assert.Equal(t, leaseA(7), got.Lease)
	})
}

func TestEvaluateLeasePartialViewIsUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)

		var got coord.LeaseStatus

		for i := range int64(10) {
			got = o.at(time.Duration(i+1)*10*time.Second,
				leaseDown("dc_a", coord.KindKV),
				held(t, "dc_b", coord.KindKV, leaseA(5)),
				held(t, coord.ArbiterName, coord.KindArbiter, leaseA(6+i)))
		}

		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)
		assert.Equal(t, leaseA(15), got.Lease)
	})
}

func TestEvaluateLeaseMismatchRestartsOnNewState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		old := kv.Lease{Holder: "dc_a", Generation: 7}

		for i := range int64(5) {
			old.Counter = i
			o.at(time.Duration(i)*10*time.Second, same3(t, old)...)
		}

		o.effective = effectiveOf(9, "dc_b")
		old.Counter = 5
		o.at(50*time.Second, same3(t, old)...)

		old.Counter = 6
		got := o.at(window+time.Nanosecond, same3(t, old)...)

		assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)
	})
}

func TestEvaluateLeaseAbsentKey(t *testing.T) {
	absent := coord.VoterLease{Name: "dc_b", Kind: coord.KindKV, Status: coord.StatusAbsent}
	absentArbiter := coord.VoterLease{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusAbsent}

	t.Run("kv_without_key_is_stale", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := newLeaseObserver(t)
			arb := held(t, coord.ArbiterName, coord.KindArbiter, leaseA(5))
			o.at(0, held(t, "dc_a", coord.KindKV, leaseA(5)), absent, arb)

			got := o.at(window+time.Nanosecond, held(t, "dc_a", coord.KindKV, leaseA(6)), absent, arb)

			assertLease(t, coord.LeaseLost, coord.LeaseReasonHolderMismatch, got)
		})
	})

	t.Run("arbiter_without_record_does_not_vote", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := newLeaseObserver(t)
			o.at(0, held(t, "dc_a", coord.KindKV, leaseA(5)), leaseDown("dc_b", coord.KindKV), absentArbiter)

			got := o.at(10*time.Second,
				held(t, "dc_a", coord.KindKV, leaseA(6)), leaseDown("dc_b", coord.KindKV), absentArbiter)

			assertLease(t, coord.LeaseUnknown, coord.LeaseReasonNoMajority, got)
		})
	})

	t.Run("invalid_record_may_be_renewed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			o := newLeaseObserver(t)
			invalid := coord.PresentLease(coord.ArbiterName, coord.KindArbiter, []byte(`{`), 10)
			o.at(0, held(t, "dc_a", coord.KindKV, leaseA(5)), held(t, "dc_b", coord.KindKV, leaseA(5)), invalid)

			got := o.at(window+time.Nanosecond,
				held(t, "dc_a", coord.KindKV, leaseA(6)), held(t, "dc_b", coord.KindKV, leaseA(5)), invalid)

			assertLease(t, coord.LeaseUnknown, coord.LeaseReasonUnconfirmed, got)
		})
	})
}

func TestEvaluateLeaseSinceIsEarliest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)
		o.at(10*time.Second, all3(t, leaseA(6), leaseA(5), leaseA(5))...)

		got := o.at(20*time.Second, same3(t, leaseA(6))...)

		assert.Equal(t, leaseA(6), got.Lease)
		o.requireSince(10*time.Second, got)
	})
}

func TestEvaluateLeaseDoesNotMutateInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)
		time.Sleep(10 * time.Second)

		tl := o.tl

		first, _ := coord.EvaluateLease(tl, same3(t, leaseA(6)), o.effective)
		second, _ := coord.EvaluateLease(tl, same3(t, leaseA(6)), o.effective)

		assert.Equal(t, coord.LeaseHeld, first.State)
		assert.Equal(t, first, second)
	})
}

func TestEvaluateLeaseKeepsMarkOfUnreachable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := newLeaseObserver(t)
		o.at(0, same3(t, leaseA(5))...)
		o.at(30*time.Second,
			held(t, "dc_a", coord.KindKV, leaseA(5)),
			held(t, "dc_b", coord.KindKV, leaseA(5)),
			leaseDown(coord.ArbiterName, coord.KindArbiter))

		// The arbiter answers again with the counter it had at the start.
		got := o.at(window+time.Nanosecond,
			held(t, "dc_a", coord.KindKV, leaseA(5)),
			leaseDown("dc_b", coord.KindKV),
			held(t, coord.ArbiterName, coord.KindArbiter, leaseA(5)))

		assert.Equal(t, coord.LeaseLost, got.State)
		o.requireSince(0, got)
	})
}
