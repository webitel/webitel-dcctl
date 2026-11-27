package patroni_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// upTo is the history of a cluster that left timelines 1 to 4 at 100 × t, then the
// timelines of more.
func upTo(more ...patroni.HistoryEntry) []patroni.HistoryEntry {
	return slices.Concat([]patroni.HistoryEntry{{Timeline: 1, LSN: 100}, {Timeline: 2, LSN: 200}, {Timeline: 3, LSN: 300}, {Timeline: 4, LSN: 400}},
		more)
}

func TestDiverge(t *testing.T) {
	pos := func(v int64) *int64 { return &v }
	// The active cluster left timeline 5 at 1000 and 6 at 2000.
	active := upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 2000})

	tests := []struct {
		name   string
		ta, td int64
		ha, hd []patroni.HistoryEntry
		pos    *int64
		want   patroni.Divergence
		ok     bool
	}{
		{name: "first_timeline", ta: 1, td: 1, ok: true},
		{name: "position_unknown", ta: 7, td: 6, hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000})},
		{name: "older_timeline_behind_switch", ta: 7, td: 6, hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}), pos: pos(2000), ok: true},
		{
			name: "older_timeline_past_switch", ta: 7, td: 6, hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}), pos: pos(2500), ok: true,
			want: patroni.Divergence{Diverged: true, LSN: 2000, Bytes: 500},
		},
		{name: "older_timeline_not_in_history", ta: 7, td: 4, ha: upTo()[:2], hd: upTo()[:3], pos: pos(10)},
		{
			name: "same_timeline_same_ancestry", ta: 7, td: 7, pos: pos(9000), ok: true,
			hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 2000}),
		},
		{
			// Promoted from timeline 6 at another point, as after failovers on both sides.
			name: "same_timeline_other_switch", ta: 7, td: 7, pos: pos(3000),
			hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 1800}),
		},
		{name: "same_timeline_history_unknown", ta: 7, td: 7, pos: pos(3000)},
		{
			name: "newer_timeline", ta: 6, td: 7, pos: pos(2600), ok: true,
			ha:   upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}),
			hd:   upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 2400}),
			want: patroni.Divergence{Diverged: true, LSN: 2400, Bytes: 200},
		},
		{
			// Promoted from 5 at 1000 to 6; the other one failed over locally 5 -> 6 at
			// 1500 and 6 -> 7 at 2000: its timeline 6 is another branch.
			name: "newer_number_after_two_local_failovers", ta: 6, td: 7, pos: pos(2500),
			ha: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}),
			hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1500}, patroni.HistoryEntry{Timeline: 6, LSN: 2000}),
		},
		{
			// Promoted from 5 at 1000 to 6, then failed over 6 -> 7 at 3000; the other one
			// failed over locally 5 -> 6 at 1500.
			name: "older_number_both_failed_over", ta: 7, td: 6, pos: pos(2500),
			ha: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}, patroni.HistoryEntry{Timeline: 6, LSN: 3000}),
			hd: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1500}),
		},
		{
			// Both skipped timeline 3.
			name: "skipped_timeline_on_both", ta: 5, td: 5, pos: pos(500), ok: true,
			ha: []patroni.HistoryEntry{{Timeline: 1, LSN: 100}, {Timeline: 2, LSN: 200}, {Timeline: 4, LSN: 400}},
			hd: []patroni.HistoryEntry{{Timeline: 1, LSN: 100}, {Timeline: 2, LSN: 200}, {Timeline: 4, LSN: 400}},
		},
		{
			name: "ancestry_differs_below", ta: 6, td: 6, pos: pos(2500),
			ha: upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000}),
			hd: append(upTo()[:3], patroni.HistoryEntry{Timeline: 4, LSN: 450}, patroni.HistoryEntry{Timeline: 5, LSN: 1000}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ha := tt.ha
			if ha == nil {
				ha = active
			}

			got, ok := patroni.Diverge(tt.ta, ha, tt.td, tt.hd, tt.pos)

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A standby that never was primary answers GET /history with nothing: only the history
// of the primary tells, on the assumption that the standby streamed from its line.
func TestDivergeByPrimary(t *testing.T) {
	pos := func(v int64) *int64 { return &v }
	// The primary left timeline 5 at 5000 and is on timeline 6.
	primary := upTo(patroni.HistoryEntry{Timeline: 5, LSN: 5000})

	tests := []struct {
		name string
		td   int64
		pos  *int64
		// shared is the first timeline the clusters may not share: 6 when unset, 0 when
		// below 0.
		shared int64
		want   patroni.Divergence
		ok     bool
	}{
		// A standby repointed after a failover, its upstream lost: behind the switch.
		{name: "behind_the_switch", td: 5, pos: pos(4900), want: patroni.Divergence{Unverified: true}, ok: true},
		{name: "past_the_switch", td: 5, pos: pos(5300), want: patroni.Divergence{Diverged: true, LSN: 5000, Bytes: 300, Unverified: true}, ok: true},
		{name: "same_timeline", td: 6, pos: pos(9000)},
		{name: "newer_timeline", td: 7, pos: pos(9000)},
		{name: "position_unknown", td: 5},
		{name: "timeline_not_in_history", td: 9, pos: pos(10)},
		// The primary took timeline 6 on promotion; 5 is still shared. One that took 5
		// on promotion may share only 4 and below: 5 may be one its upstream alone had.
		{name: "on_the_promoted_timeline", td: 5, pos: pos(4900), shared: 5},
		{name: "shared_not_known", td: 5, pos: pos(4900), shared: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shared := tt.shared
			if shared == 0 {
				shared = 6
			} else if shared < 0 {
				shared = 0
			}

			got, ok := patroni.DivergeByPrimary(6, primary, tt.td, tt.pos, shared)

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A timeline number neither history was on agrees, as a promotion skipped it; one only
// one of them left does not.
func TestSharedAncestry(t *testing.T) {
	skip := []patroni.HistoryEntry{{Timeline: 1, LSN: 100}, {Timeline: 2, LSN: 200}, {Timeline: 4, LSN: 400}}

	assert.True(t, patroni.SharedAncestry(skip, skip, 5), "1 -> 2 -> 4 against 1 -> 2 -> 4")
	assert.False(t, patroni.SharedAncestry(skip, upTo(), 5), "3 is left by one of them only")
	assert.False(t, patroni.SharedAncestry(upTo(), nil, 5), "an empty history agrees with none")
	assert.True(t, patroni.SharedAncestry(nil, nil, 1))
}

func TestDemoteLoss(t *testing.T) {
	// The primary left timeline 5 at 1000; the standby stayed on it and received 1200.
	history := upTo(patroni.HistoryEntry{Timeline: 5, LSN: 1000})

	tests := []struct {
		name     string
		ta       int64
		ha       []patroni.HistoryEntry
		loc      int64
		received int64
		want     int64
		ok       bool
	}{
		{name: "same_timeline", ta: 5, loc: 1500, received: 1200, want: 300, ok: true},
		// A failover from a primary that answers: the standby has no history of its own.
		{name: "failover_from_primary_that_answers", ta: 5, ha: upTo(), loc: 5200, received: 5000, want: 200, ok: true},
		{name: "same_timeline_received_all", ta: 5, loc: 1200, received: 1200, ok: true},
		{name: "newer_timeline", ta: 6, ha: history, loc: 1500, received: 1200, want: 500, ok: true},
		{name: "newer_timeline_behind_received", ta: 6, ha: history, loc: 1100, received: 1200, want: 100, ok: true},
		{name: "newer_timeline_received_less", ta: 6, ha: history, loc: 1500, received: 800, want: 700, ok: true},
		{name: "history_not_read", ta: 6, loc: 1500, received: 1200},
		{name: "timeline_not_in_history", ta: 6, ha: history[:4], loc: 1500, received: 1200},
		{name: "standby_on_newer_timeline", ta: 4, ha: history, loc: 1500, received: 1200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := patroni.DemoteLoss(tt.ta, tt.ha, tt.loc, 5, tt.received)

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
