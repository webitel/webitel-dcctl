package patroni

import "slices"

// Divergence is where a cluster left the history of another one.
type Divergence struct {
	Diverged bool
	// LSN is where the histories part, Bytes how much WAL the cluster holds past it.
	LSN, Bytes int64
}

// Diverge tells whether a cluster on timeline td that holds WAL up to pos left the
// history of a cluster on timeline ta; ha and hd are the GET /history of each. A
// cluster on an older timeline diverged when it holds WAL past the point the other one
// switched from that timeline; one on a newer timeline branched off at the point its
// own history gives. Two clusters on the same timeline diverged when they switched to
// it at different points, as after two promotions from one timeline. ok is false when
// the histories or pos do not tell.
func Diverge(ta int64, ha []HistoryEntry, td int64, hd []HistoryEntry, pos *int64) (d Divergence, ok bool) {
	if ta == td && ta <= 1 {
		return Divergence{}, true
	}

	if pos == nil {
		return Divergence{}, false
	}

	switch {
	case ta == td:
		la, okA := SwitchPoint(ha, ta-1)
		ld, okD := SwitchPoint(hd, td-1)

		switch {
		case !okA || !okD:
			return Divergence{}, false
		case la == ld:
			return Divergence{}, true
		}

		lsn := min(la, ld)

		return Divergence{Diverged: true, LSN: lsn, Bytes: max(0, *pos-lsn)}, true
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

// SwitchPoint returns where the cluster of history left timeline.
func SwitchPoint(history []HistoryEntry, timeline int64) (int64, bool) {
	i := slices.IndexFunc(history, func(e HistoryEntry) bool { return e.Timeline == timeline })
	if i < 0 {
		return 0, false
	}

	return history[i].LSN, true
}
