package patroni_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func TestDiverge(t *testing.T) {
	pos := func(v int64) *int64 { return &v }
	// The active cluster left timeline 5 at 1000 and 6 at 2000.
	active := []patroni.HistoryEntry{{Timeline: 5, LSN: 1000}, {Timeline: 6, LSN: 2000}}

	tests := []struct {
		name   string
		ta, td int64
		hd     []patroni.HistoryEntry
		pos    *int64
		want   patroni.Divergence
		ok     bool
	}{
		{name: "first_timeline", ta: 1, td: 1, ok: true},
		{name: "position_unknown", ta: 7, td: 6},
		{name: "older_timeline_behind_switch", ta: 7, td: 6, pos: pos(2000), ok: true},
		{
			name: "older_timeline_past_switch", ta: 7, td: 6, pos: pos(2500), ok: true,
			want: patroni.Divergence{Diverged: true, LSN: 2000, Bytes: 500},
		},
		{name: "older_timeline_not_in_history", ta: 7, td: 4, pos: pos(10)},
		{
			name: "same_timeline_same_switch", ta: 7, td: 7, pos: pos(9000), ok: true,
			hd: []patroni.HistoryEntry{{Timeline: 6, LSN: 2000}},
		},
		{
			name: "same_timeline_other_switch", ta: 7, td: 7, pos: pos(3000), ok: true,
			hd:   []patroni.HistoryEntry{{Timeline: 6, LSN: 1800}},
			want: patroni.Divergence{Diverged: true, LSN: 1800, Bytes: 1200},
		},
		{name: "same_timeline_history_unknown", ta: 7, td: 7, pos: pos(3000)},
		{
			name: "newer_timeline", ta: 6, td: 7, pos: pos(2600), ok: true,
			hd:   []patroni.HistoryEntry{{Timeline: 6, LSN: 2400}},
			want: patroni.Divergence{Diverged: true, LSN: 2400, Bytes: 200},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := patroni.Diverge(tt.ta, active, tt.td, tt.hd, tt.pos)

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDemoteLoss(t *testing.T) {
	// The primary left timeline 5 at 1000; the standby stayed on it and received 1200.
	history := []patroni.HistoryEntry{{Timeline: 4, LSN: 400}, {Timeline: 5, LSN: 1000}}

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
		{name: "same_timeline_received_all", ta: 5, loc: 1200, received: 1200, ok: true},
		{name: "newer_timeline", ta: 6, ha: history, loc: 1500, received: 1200, want: 500, ok: true},
		{name: "newer_timeline_behind_received", ta: 6, ha: history, loc: 1100, received: 1200, want: 100, ok: true},
		{name: "newer_timeline_received_less", ta: 6, ha: history, loc: 1500, received: 800, want: 700, ok: true},
		{name: "history_not_read", ta: 6, loc: 1500, received: 1200},
		{name: "timeline_not_in_history", ta: 6, ha: history[:1], loc: 1500, received: 1200},
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
