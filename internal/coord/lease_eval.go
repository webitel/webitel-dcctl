package coord

import (
	"fmt"
	"time"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

// LeaseState is whether the lease of the active datacenter is renewed.
type LeaseState string

const (
	// LeaseHeld means a majority of voters were seen renewed within
	// T + acquire_after.
	LeaseHeld LeaseState = "held"
	// LeaseLost means the voters that may still be renewed are fewer than a
	// majority: every other voter answered with a counter unchanged, or a
	// holder or generation other than the effective state, for longer than
	// T + acquire_after.
	LeaseLost LeaseState = "lost"
	// LeaseUnknown means the reads prove neither.
	LeaseUnknown LeaseState = "unknown"
)

// LeaseReason tells why the lease is in its [LeaseState].
type LeaseReason string

const (
	LeaseReasonRenewed LeaseReason = "renewed"
	// LeaseReasonCounterStale is a lost lease whose counter stopped.
	LeaseReasonCounterStale LeaseReason = "counter_stale"
	// LeaseReasonHolderMismatch is a lost lease that names another holder
	// or generation than the effective state, or is missing.
	LeaseReasonHolderMismatch LeaseReason = "holder_mismatch"
	// LeaseReasonStateUndetermined is an unknown lease because there is no
	// effective state to compare it with.
	LeaseReasonStateUndetermined LeaseReason = "state_undetermined"
	// LeaseReasonNoMajority is an unknown lease because fewer than a
	// majority of voters answered with a lease or without the key, and those
	// that did not may be renewed.
	LeaseReasonNoMajority LeaseReason = "no_majority"
	// LeaseReasonUnconfirmed is an unknown lease that is not proven renewed
	// on a majority: an observer that has not yet seen the counter change,
	// a voter that answered again after a gap, or voters that did not answer
	// and may hold the renewals.
	LeaseReasonUnconfirmed LeaseReason = "unconfirmed"
)

// LeaseStatus is the lease as one instance sees it.
type LeaseStatus struct {
	State  LeaseState
	Reason LeaseReason
	// Lease is the lease of the highest generation and counter among the
	// voters that answered; Holder is empty when none holds one.
	Lease kv.Lease
	// Since is when this instance first saw Lease on a voter.
	Since time.Time
}

// LeaseTimeline is what an instance remembers of the leases it read: when
// each voter was last read, when its lease was first seen and since when it
// differs from the effective state. It lives in the memory of one instance,
// on its monotonic clock, and is advanced only by [EvaluateLease].
type LeaseTimeline struct {
	window time.Duration
	marks  map[string]leaseMark
}

type leaseMark struct {
	present bool
	lease   kv.Lease
	// seenAt is when lease was first read: the latest it may have been
	// written.
	seenAt time.Time
	// readAt is when the voter was last read.
	readAt time.Time
	// changedAfter is when the voter was read before lease was seen: the
	// earliest it may have been written. It is zero when lease is the first
	// one read.
	changedAfter time.Time

	mismatched    bool
	mismatchSince time.Time
	// mismatchWith is the effective state the mismatch is measured against.
	mismatchWith RecordKey
}

// NewLeaseTimeline returns the empty timeline of a lease with the period T
// and acquireAfter.
func NewLeaseTimeline(period, acquireAfter time.Duration) (LeaseTimeline, error) {
	if period <= 0 || acquireAfter <= 0 {
		return LeaseTimeline{}, fmt.Errorf("lease period %s and acquire after %s must be positive", period, acquireAfter)
	}

	return LeaseTimeline{window: period + acquireAfter}, nil
}

// EvaluateLease tells whether the lease is held from the reads of dcctl/lease
// on voters, which must be every configured voter, and the effective record
// of dcctl/state (View.Effective). It takes the time from the monotonic clock
// of this instance, so it is to be called right after the reads. It returns
// the timeline to pass on the next call; tl is not changed. A read that may
// not be current is passed as stale, see [VoterLease].
//
// A change of a voter's lease happened between the read before it was seen
// and the read that saw it. A voter is renewed when its lease names the
// effective holder and generation and the earliest that change may have
// happened is within T + acquire_after. A voter is stale when the latest the
// change may have happened is longer ago than that, or when its lease has
// not matched the effective state for longer. A KV without the key counts as
// a lease that never matches; an arbiter without the record or without state
// does not vote.
//
// The lease is [LeaseHeld] when a majority of voters are renewed. It is
// [LeaseLost] when the voters that are not stale, including every voter that
// did not answer, are fewer than a majority: the holder cannot be renewing a
// majority, whatever this instance cannot reach. Anything else is
// [LeaseUnknown]. What an unreachable voter held when last read is kept for
// when it answers again; a voter that answers with a new lease after a gap
// is renewed only once it is seen changing again. An instance that has just
// started has no history, so the lease stays unknown until it sees the
// counter change or until T + acquire_after passes without a change.
func EvaluateLease(tl LeaseTimeline, voters []VoterLease, effective *Record) (LeaseStatus, LeaseTimeline) {
	now := time.Now()
	next := LeaseTimeline{window: tl.window, marks: make(map[string]leaseMark, len(voters))}

	var eff *RecordKey
	if effective != nil {
		eff = new(KeyOf(effective.State))
	}

	var (
		st                               LeaseStatus
		renewed, pending, stale, unknown int
		staleMismatch                    bool
		majority                         = len(voters)/2 + 1
	)

	for _, v := range voters {
		prev, seen := tl.marks[v.Name]

		if !answered(v) {
			if seen {
				next.marks[v.Name] = prev
			}

			if v.Status.Unknown() {
				unknown++
			}

			continue
		}

		m := observeLease(prev, seen, v, eff, now)
		next.marks[v.Name] = m

		if m.present {
			st.Lease, st.Since = leadingLease(st.Lease, st.Since, m)
		}

		staleCounter := now.Sub(m.seenAt) > tl.window
		staleHolder := m.mismatched && now.Sub(m.mismatchSince) > tl.window

		switch {
		case staleCounter || staleHolder:
			stale++
			staleMismatch = staleMismatch || staleHolder
		case m.present && !m.mismatched && !m.changedAfter.IsZero() && now.Sub(m.changedAfter) <= tl.window:
			renewed++
		default:
			pending++
		}
	}

	switch {
	case eff == nil:
		st.State, st.Reason = LeaseUnknown, LeaseReasonStateUndetermined
	case renewed >= majority:
		st.State, st.Reason = LeaseHeld, LeaseReasonRenewed
	case renewed+pending+unknown < majority:
		st.State, st.Reason = LeaseLost, LeaseReasonCounterStale
		if staleMismatch {
			st.Reason = LeaseReasonHolderMismatch
		}
	case renewed+pending+stale < majority:
		st.State, st.Reason = LeaseUnknown, LeaseReasonNoMajority
	default:
		st.State, st.Reason = LeaseUnknown, LeaseReasonUnconfirmed
	}

	return st, next
}

// answered reports whether v tells what lease the voter holds: a lease, or
// a KV without the key. An arbiter takes no lease it does not hold.
func answered(v VoterLease) bool {
	return v.Status == StatusPresent || v.Status == StatusAbsent && v.Kind == KindKV
}

// observeLease returns the mark of v read at now after prev. Without an
// effective state the mismatch is left as it was.
func observeLease(prev leaseMark, seen bool, v VoterLease, eff *RecordKey, now time.Time) leaseMark {
	m := prev
	present := v.Status == StatusPresent

	if !seen || prev.present != present || prev.lease != v.Lease {
		m.present, m.lease, m.seenAt, m.changedAfter = present, v.Lease, now, prev.readAt
	}

	m.readAt = now

	if eff == nil {
		return m
	}

	switch {
	case present && v.Lease.Holder == eff.ActiveDatacenter && v.Lease.Generation == eff.Generation:
		m.mismatched, m.mismatchSince, m.mismatchWith = false, time.Time{}, RecordKey{}
	case !m.mismatched || m.mismatchWith != *eff:
		m.mismatched, m.mismatchSince, m.mismatchWith = true, now, *eff
	}

	return m
}

// leadingLease returns the lease of the highest generation and counter of
// lease and m, with the earliest time it was seen.
func leadingLease(lease kv.Lease, since time.Time, m leaseMark) (kv.Lease, time.Time) {
	l := m.lease

	switch {
	case lease.Holder == "",
		l.Generation > lease.Generation,
		l.Generation == lease.Generation && l.Counter > lease.Counter:
		return l, m.seenAt
	case l == lease && m.seenAt.Before(since):
		return l, m.seenAt
	}

	return lease, since
}
