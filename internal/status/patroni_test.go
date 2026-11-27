package status_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func patroniOf(t *testing.T, s *collect.Snapshot, dc string) status.Patroni {
	t.Helper()

	return datacenter(t, status.Build(s, testConfig(), now), dc).Patroni
}

func TestBuild_patroniWarm(t *testing.T) {
	s := warm()

	a := patroniOf(t, s, dcA)
	assert.Equal(t, "pg-a1", a.Leader)
	assert.Equal(t, status.RolePrimary, a.Role)
	assert.Equal(t, i64(3), a.Timeline)
	assert.False(t, a.Paused)
	assert.Nil(t, a.LagBytes, "the active datacenter does not lag behind itself")
	assert.Equal(t, status.Divergence{Known: true}, a.Divergence)
	require.Len(t, a.Members, 2)
	assert.Equal(t, status.Member{
		Name: "pg-a1", Host: "10.0.0.1", APIURL: "https://10.0.0.1:8008/patroni", Datacenter: dcA,
		Reachable: true, Role: status.RolePrimary, PatroniState: "running", Timeline: i64(3),
		Xlog: status.Xlog{Location: i64(5000)}, Version: "4.1.0", FailoverPriority: 1, Usable: true,
	}, a.Members[0])
	assert.Equal(t, i64(0), a.Members[1].LagBytes)
	assert.Equal(t, "streaming", a.Members[1].ReplicationState)

	b := patroniOf(t, s, dcB)
	assert.Equal(t, "pg-b1", b.Leader)
	assert.Equal(t, status.RoleStandbyLeader, b.Role)
	assert.Equal(t, "10.0.0.1", b.StandbyClusterHost)
	assert.Equal(t, dcA, b.UpstreamDatacenter)
	assert.Equal(t, i64(100), b.LagBytes)
	assert.Equal(t, status.Divergence{Known: true}, b.Divergence)
}

func TestBuild_patroniUnreachableMember(t *testing.T) {
	s := warm()
	editMember("pg-a2", func(m *collect.Member) { *m = member(m.Name, m.Host, m.Datacenter, m.Role, m.State, nil) })(s)

	m := patroniOf(t, s, dcA).Members[1]
	assert.False(t, m.Reachable)
	assert.Equal(t, source.ReasonTimeout, m.Reason)
	assert.False(t, m.Usable)
	assert.Equal(t, patroni.UnusableUnreachable, m.UnusableReason)
	assert.Equal(t, "streaming", m.PatroniState, "the state of /cluster stays")
}

func TestBuild_patroniLag(t *testing.T) {
	t.Run("standby_ahead_of_an_older_primary_read", func(t *testing.T) {
		s := warm()
		editProbe("pg-b1", func(p *patroni.State) { p.Xlog.ReceivedLocation = i64(5100) })(s)

		assert.Equal(t, i64(0), patroniOf(t, s, dcB).LagBytes)
	})

	t.Run("not_from_last_known_data", func(t *testing.T) {
		s := warm()
		s.Clusters[0].Patroni = unreachable(s.Clusters[0].Patroni)

		assert.Nil(t, patroniOf(t, s, dcB).LagBytes)
	})

	t.Run("multiple_upstream_hosts", func(t *testing.T) {
		s := warm()
		setStandbyHost("10.0.0.1, 10.0.0.2")(s)

		assert.Equal(t, dcA, patroniOf(t, s, dcB).UpstreamDatacenter)
	})

	t.Run("upstream_hosts_in_two_datacenters", func(t *testing.T) {
		s := warm()
		setStandbyHost("10.0.0.1,10.0.1.2")(s)

		assert.Empty(t, patroniOf(t, s, dcB).UpstreamDatacenter)
	})
}

// setPatroniTimeline makes every voter hold generation 5 of dc_a with patroni_timeline
// tl, the timeline dc_a took on promotion.
func setPatroniTimeline(s *collect.Snapshot, tl int64) {
	st := kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "op-1", PatroniTimeline: tl, UpdatedAt: lost}

	for i := range s.Voters {
		s.Voters[i] = kvVoter(s.Voters[i].Datacenter, entry(kv.StateKey, st))
	}

	s.Arbiter = new(up(collect.Arbiter{Initialized: true, State: arbiterRecord(st), Lease: arbiterRecord(kv.Lease{Holder: dcA, Generation: 5, Counter: 6})}))
}

func TestBuild_divergence(t *testing.T) {
	tests := []struct {
		name string
		edit func(s *collect.Snapshot)
		want status.Divergence
	}{
		{
			name: "older_timeline_written_past_the_switch",
			edit: editProbe("pg-b1", func(p *patroni.State) {
				p.Timeline = i64(2)
				p.Xlog.ReceivedLocation = i64(3500)
			}),
			want: status.Divergence{Known: true, Diverged: true, LSN: 3000, Bytes: 500},
		},
		{
			name: "older_timeline_behind_the_switch",
			edit: editProbe("pg-b1", func(p *patroni.State) {
				p.Timeline = i64(2)
				p.Xlog.ReceivedLocation = i64(2500)
			}),
			want: status.Divergence{Known: true},
		},
		{
			// A standby answers GET /history with what it last wrote as a primary, if ever:
			// its line is the history of dc_a, which it streams from.
			name: "standby_by_the_line_of_its_upstream",
			edit: func(s *collect.Snapshot) {
				editPatroni(s, dcB, func(p *collect.Patroni) {
					p.History = history(
						patroni.HistoryEntry{Timeline: 1, LSN: 1000},
						patroni.HistoryEntry{Timeline: 2, LSN: 2800},
					)
				})
			},
			want: status.Divergence{Known: true},
		},
		{
			name: "standby_without_a_history",
			edit: func(s *collect.Snapshot) {
				editPatroni(s, dcB, func(p *collect.Patroni) { p.History = unreachable(p.History) })
			},
			want: status.Divergence{Known: true},
		},
		{
			// The standby points at no known datacenter: the history of dc_a alone tells.
			name: "standby_upstream_not_known",
			edit: all(
				editProbe("pg-b1", func(p *patroni.State) {
					p.Timeline = i64(2)
					p.Xlog.ReceivedLocation = i64(3500)
				}),
				func(s *collect.Snapshot) {
					editPatroni(s, dcB, func(p *collect.Patroni) {
						p.Config = collect.Source[map[string]any]{Reachable: true, Data: map[string]any{
							"standby_cluster": map[string]any{"host": "10.9.9.9", "port": float64(5432)},
						}}
					})
				},
			),
			want: status.Divergence{Known: true, Diverged: true, LSN: 3000, Bytes: 500, Unverified: true},
		},
		{
			// On the timeline below the active one, at or above the one it took on
			// promotion, as recorded: it may follow a line only its lost upstream had.
			name: "standby_upstream_not_known_on_the_promoted_timeline",
			edit: all(
				editProbe("pg-b1", func(p *patroni.State) {
					p.Timeline = i64(2)
					p.Xlog.ReceivedLocation = i64(3500)
				}),
				func(s *collect.Snapshot) {
					editPatroni(s, dcB, func(p *collect.Patroni) {
						p.Config = collect.Source[map[string]any]{Reachable: true, Data: map[string]any{
							"standby_cluster": map[string]any{"host": "10.9.9.9", "port": float64(5432)},
						}}
					})
					setPatroniTimeline(s, 2)
				},
			),
			want: status.Divergence{Rebuild: true},
		},
		{
			// An old primary on the timeline of the active one: two promotions apart are
			// not told from each other.
			name: "primary_on_the_active_timeline",
			edit: func(s *collect.Snapshot) { promote(s, dcB, "pg-b1") },
			want: status.Divergence{},
		},
		{
			name: "newer_timeline_of_a_promoted_datacenter",
			edit: all(
				func(s *collect.Snapshot) { promote(s, dcB, "pg-b1") },
				editProbe("pg-b1", func(p *patroni.State) {
					p.Timeline = i64(4)
					p.Xlog.Location = i64(4500)
				}),
				func(s *collect.Snapshot) {
					editPatroni(s, dcB, func(p *collect.Patroni) {
						p.History = history(
							patroni.HistoryEntry{Timeline: 1, LSN: 1000},
							patroni.HistoryEntry{Timeline: 2, LSN: 3000},
							patroni.HistoryEntry{Timeline: 3, LSN: 4000},
						)
					})
				},
			),
			want: status.Divergence{Known: true, Diverged: true, LSN: 4000, Bytes: 500},
		},
		{
			name: "newer_timeline_without_its_history",
			edit: all(
				editProbe("pg-b1", func(p *patroni.State) { p.Timeline = i64(4) }),
				func(s *collect.Snapshot) {
					editPatroni(s, dcB, func(p *collect.Patroni) { p.History = unreachable(p.History) })
				},
			),
			want: status.Divergence{},
		},
		{
			name: "active_history_unreachable",
			edit: all(
				editProbe("pg-b1", func(p *patroni.State) { p.Timeline = i64(2) }),
				func(s *collect.Snapshot) {
					editPatroni(s, dcA, func(p *collect.Patroni) { p.History = unreachable(p.History) })
				},
			),
			want: status.Divergence{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := warm()
			tt.edit(s)

			assert.Equal(t, tt.want, patroniOf(t, s, dcB).Divergence)
		})
	}
}

func TestBuild_patroniStretch(t *testing.T) {
	s := stretch()

	a := patroniOf(t, s, dcA)
	assert.Equal(t, "pg-1", a.Leader)
	assert.Equal(t, status.RolePrimary, a.Role)
	assert.Nil(t, a.LagBytes)
	assert.Len(t, a.Members, 2)

	b := patroniOf(t, s, dcB)
	assert.Empty(t, b.Leader)
	assert.Equal(t, status.RoleUnknown, b.Role)
	assert.Equal(t, i64(3), b.Timeline)
	assert.Equal(t, i64(10), b.LagBytes, "the least lag of a member of the datacenter")
	assert.Len(t, b.Members, 2)

	editMember("pg-4", func(m *collect.Member) { m.Datacenter = "" })(s)
	assert.Len(t, patroniOf(t, s, dcB).Members, 1, "a member of no known datacenter is in none")
}

func TestBuild_detectedActive(t *testing.T) {
	tests := []struct {
		name string
		snap func() *collect.Snapshot
		want string
	}{
		{"warm", warm, dcA},
		{"two_primaries", with(warm, func(s *collect.Snapshot) { promote(s, dcB, "pg-b1") }), ""},
		{"primary_unreachable", with(warm, func(s *collect.Snapshot) {
			s.Clusters[0].Patroni = unreachable(s.Clusters[0].Patroni)
		}), ""},
		{"other_cluster_unreachable", with(warm, func(s *collect.Snapshot) {
			promote(s, dcB, "pg-b1")
			s.Clusters[0].Patroni = unreachable(s.Clusters[0].Patroni)
		}), ""},
		{"stretch", stretch, dcA},
		{"stretch_leader_without_datacenter", with(stretch, editMember("pg-1", func(m *collect.Member) {
			m.Datacenter = ""
		})), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, status.Build(tt.snap(), testConfig(), now).DetectedActiveDatacenter)
		})
	}
}
