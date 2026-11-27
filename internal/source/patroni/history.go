package patroni

import "slices"

// Divergence is where a cluster left the history of another one.
type Divergence struct {
	Diverged bool
	// LSN is where the histories part, Bytes how much WAL the cluster holds past it.
	LSN, Bytes int64
	// Unverified is set when the line of the cluster was not read, and the history of
	// the other one alone told the divergence, see [DivergeByPrimary].
	Unverified bool
}

// Diverge tells whether a cluster on timeline td that holds WAL up to pos left the
// history of a cluster on timeline ta; ha and hd are the lines of each, every timeline
// before the current one. Patroni keeps GET /history current on a primary only: the
// line of a standby is the history of the primary it streams from. A timeline number names the same branch in two
// clusters only while their histories agree: both must have left every timeline below
// the lower current one at the same point, or ok is false, as after local failovers on
// both sides of a split, where Patroni would not rewind either. Then a cluster on an
// older timeline diverged when it holds WAL past the point the other one switched from
// that timeline, and one on a newer timeline branched off at the point its own history
// gives; two on the same timeline did not diverge. ok is false too when pos is not known.
//
// Promotions apart at the same point to the same timeline number leave histories that
// agree: such clusters read as not diverged, or with the divergence of the later
// promotion of one of them, and the WAL written past the shared point is not told.
func Diverge(ta int64, ha []HistoryEntry, td int64, hd []HistoryEntry, pos *int64) (d Divergence, ok bool) {
	if ta == td && ta <= 1 {
		return Divergence{}, true
	}

	if pos == nil || !SharedAncestry(ha, hd, min(ta, td)) {
		return Divergence{}, false
	}

	switch {
	case ta == td:
		return Divergence{}, true
	case td < ta:
		lsn, ok := SwitchPoint(ha, td)
		if !ok {
			return Divergence{}, false
		}

		if *pos <= lsn {
			return Divergence{}, true
		}

		return Divergence{Diverged: true, LSN: lsn, Bytes: *pos - lsn}, true
	}

	lsn, ok := SwitchPoint(hd, ta)
	if !ok {
		return Divergence{}, false
	}

	return Divergence{Diverged: true, LSN: lsn, Bytes: max(0, *pos-lsn)}, true
}

// DivergeByPrimary is [Diverge] for a cluster whose own line is not known, as a standby
// whose upstream is not read: only ha, the history of the primary, tells. shared is the
// first timeline the two may not share, the one the primary took when it was promoted
// over the cluster the other one streamed from: below it the line of the other one is
// that of the primary. A cluster on an older timeline than shared is judged as Diverge
// judges it; on shared or newer it may be on a timeline only its lost upstream had, and
// ok is false, as on the timeline of the primary or newer, or with shared 0, not known.
// The result is Unverified.
func DivergeByPrimary(ta int64, ha []HistoryEntry, td int64, pos *int64, shared int64) (d Divergence, ok bool) {
	switch {
	case ta == td && ta <= 1:
		return Divergence{Unverified: true}, true
	case pos == nil || td >= ta || td >= shared:
		return Divergence{}, false
	}

	lsn, ok := SwitchPoint(ha, td)
	if !ok {
		return Divergence{}, false
	}

	if *pos <= lsn {
		return Divergence{Unverified: true}, true
	}

	return Divergence{Diverged: true, LSN: lsn, Bytes: *pos - lsn, Unverified: true}, true
}

// DemoteLoss returns the WAL a primary on timeline ta that wrote up to loc holds beyond
// what a standby on timeline tb received up to recv: demoting the primary to follow the
// standby discards it. The standby streamed from the primary, so below timeline tb its
// line is that of ha, the history of the primary. On another timeline than the standby
// the primary shares WAL with it only up to where it left timeline tb, by ha. ok is
// false when ha does not tell that point, or when the standby is on a newer timeline than
// the primary.
func DemoteLoss(ta int64, ha []HistoryEntry, loc, tb, recv int64) (loss int64, ok bool) {
	if tb > ta {
		return 0, false
	}

	shared := recv

	if ta != tb {
		x, ok := SwitchPoint(ha, tb)
		if !ok {
			return 0, false
		}

		shared = min(recv, x)
	}

	return max(0, loc-shared), true
}

// SharedAncestry reports whether histories a and b left every timeline below timeline
// at the same point. A timeline neither of them was on, as a number a promotion skipped,
// agrees; one only one of them left does not.
func SharedAncestry(a, b []HistoryEntry, timeline int64) bool {
	for t := int64(1); t < timeline; t++ {
		la, okA := SwitchPoint(a, t)
		lb, okB := SwitchPoint(b, t)

		if okA != okB || la != lb {
			return false
		}
	}

	return true
}

// SwitchPoint returns where the cluster of history left timeline.
func SwitchPoint(history []HistoryEntry, timeline int64) (int64, bool) {
	i := slices.IndexFunc(history, func(e HistoryEntry) bool { return e.Timeline == timeline })
	if i < 0 {
		return 0, false
	}

	return history[i].LSN, true
}
