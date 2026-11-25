package status_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// with returns a snapshot built by base and changed by fn.
func with(base func() *collect.Snapshot, fn func(s *collect.Snapshot)) func() *collect.Snapshot {
	return func() *collect.Snapshot {
		s := base()
		fn(s)

		return s
	}
}

func setVoterState(s *collect.Snapshot, dc string, state *consul.KVEntry) {
	for i := range s.Voters {
		if s.Voters[i].Datacenter == dc {
			s.Voters[i].KV.Data.State = state
		}
	}
}

func voterDown(s *collect.Snapshot, dc string) {
	for i := range s.Voters {
		if s.Voters[i].Datacenter == dc {
			s.Voters[i].KV = unreachable(s.Voters[i].KV)
		}
	}
}

func dcOf(s *collect.Snapshot, name string) *collect.Datacenter {
	d, ok := s.Datacenter(name)
	if !ok {
		panic("no datacenter " + name)
	}

	return d
}

func allSourcesDown(s *collect.Snapshot, dc string) {
	d := dcOf(s, dc)
	d.Services = unreachable(d.Services)
	d.Nomad = unreachable(d.Nomad)

	cl, _ := s.ClusterOf(dc)
	cl.Patroni = unreachable(cl.Patroni)
}

func startEngine(s *collect.Snapshot, dc string) {
	d := dcOf(s, dc)
	n := d.Nomad.Data
	n.Managed = append([]nomad.Job(nil), n.Managed...)
	n.Managed[0].Stop = false
	n.Allocs = map[string][]nomad.Alloc{"engine": {healthyAlloc("engine", "n1")}}
	d.Nomad.Data = n
}

func promote(s *collect.Snapshot, dc, name string) {
	editPatroni(s, dc, func(p *collect.Patroni) {
		m := memberNamed(p, name)
		m.Role = patroni.ClusterRoleLeader
		m.State = "running"
		m.Probe = probe(patroni.RolePrimary, "running", "", patroni.Xlog{Location: i64(4950)})
	})
}

func uninitialized(s *collect.Snapshot) {
	for i := range s.Voters {
		s.Voters[i].KV.Data.State = nil
		s.Voters[i].KV.Data.Lease = nil
	}

	s.Arbiter.Data = collect.Arbiter{}
}

func TestBuild_state(t *testing.T) {
	tests := []struct {
		name    string
		snap    func() *collect.Snapshot
		dc      string
		want    status.State
		skipped []status.State
	}{
		{name: "active", snap: warm, dc: dcA, want: status.StateActive},
		{name: "standby", snap: warm, dc: dcB, want: status.StateStandby},
		{
			name: "unreachable",
			snap: with(warm, func(s *collect.Snapshot) { allSourcesDown(s, dcB) }),
			dc:   dcB, want: status.StateUnreachable,
		},
		{
			name: "unreachable_before_active",
			snap: with(warm, func(s *collect.Snapshot) { allSourcesDown(s, dcA) }),
			dc:   dcA, want: status.StateUnreachable,
		},
		{
			name: "one_source_left_is_reachable",
			snap: with(warm, func(s *collect.Snapshot) {
				allSourcesDown(s, dcB)
				dcOf(s, dcB).Nomad.Reachable = true
			}),
			dc: dcB, want: status.StateStandby, skipped: []status.State{status.StateDBSplit},
		},
		{
			name: "uninitialized",
			snap: with(warm, uninitialized),
			dc:   dcA, want: status.StateUninitialized,
		},
		{
			name: "uninitialized_unknown_with_a_voter_unreachable",
			snap: with(warm, func(s *collect.Snapshot) {
				uninitialized(s)
				voterDown(s, dcB)
			}),
			dc: dcA, want: status.StateUndetermined, skipped: []status.State{status.StateUninitialized},
		},
		{
			name: "split_brain",
			snap: with(warm, func(s *collect.Snapshot) { startEngine(s, dcB) }),
			dc:   dcB, want: status.StateSplitBrain,
		},
		{
			name: "split_brain_skipped_without_nomad",
			snap: with(warm, func(s *collect.Snapshot) {
				startEngine(s, dcB)
				d := dcOf(s, dcB)
				d.Nomad = unreachable(d.Nomad)
			}),
			dc: dcB, want: status.StateStandby, skipped: []status.State{status.StateSplitBrain},
		},
		{
			name: "split_brain_while_stopping",
			snap: with(warm, func(s *collect.Snapshot) {
				d := dcOf(s, dcB)
				n := d.Nomad.Data
				n.Allocs = map[string][]nomad.Alloc{"engine": {healthyAlloc("engine", "n1")}}
				d.Nomad.Data = n
			}),
			dc: dcB, want: status.StateSplitBrain,
		},
		{
			name: "db_split",
			snap: with(warm, func(s *collect.Snapshot) { promote(s, dcB, "pg-b1") }),
			dc:   dcB, want: status.StateDBSplit,
		},
		{
			name: "db_split_skipped_without_patroni",
			snap: with(warm, func(s *collect.Snapshot) {
				promote(s, dcB, "pg-b1")
				cl, _ := s.ClusterOf(dcB)
				cl.Patroni = unreachable(cl.Patroni)
			}),
			dc: dcB, want: status.StateStandby, skipped: []status.State{status.StateDBSplit},
		},
		{
			name: "no_db_split_in_stretch",
			snap: with(stretch, func(s *collect.Snapshot) {
				editPatroni(s, dcB, func(p *collect.Patroni) {
					memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
					memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
					p.Leader = "pg-3"
				})
			}),
			dc: dcB, want: status.StateStandby,
		},
		{
			name: "stale",
			snap: with(warm, func(s *collect.Snapshot) { setVoterState(s, dcB, stateEntry(4, dcA)) }),
			dc:   dcB, want: status.StateStale,
		},
		{
			name: "stale_skipped_without_the_voter",
			snap: with(warm, func(s *collect.Snapshot) {
				setVoterState(s, dcB, stateEntry(4, dcA))
				voterDown(s, dcB)
			}),
			dc: dcB, want: status.StateStandby, skipped: []status.State{status.StateStale},
		},
		{
			name: "missing_key_is_stale",
			snap: with(warm, func(s *collect.Snapshot) { setVoterState(s, dcB, nil) }),
			dc:   dcB, want: status.StateStale,
		},
		{
			name: "needs_recovery",
			snap: with(warm, func(s *collect.Snapshot) {
				setVoterState(s, dcA, stateEntry(5, dcA, dcB))
				setVoterState(s, dcB, stateEntry(5, dcA, dcB))
			}),
			dc: dcB, want: status.StateNeedsRecovery,
		},
		{
			name: "conflict_is_undetermined",
			snap: with(warm, func(s *collect.Snapshot) {
				forced := func(active string) *consul.KVEntry {
					e := stateEntry(6, active)
					e.Value = []byte(`{"generation":6,"active_datacenter":"` + active + `","forced":true}`)

					return e
				}
				setVoterState(s, dcA, forced(dcA))
				setVoterState(s, dcB, forced(dcB))
			}),
			dc: dcA, want: status.StateUndetermined,
		},
		{
			name: "record_without_majority_is_undetermined",
			snap: with(warm, func(s *collect.Snapshot) {
				setVoterState(s, dcA, stateEntry(6, dcB))
				voterDown(s, dcB)
			}),
			dc: dcB, want: status.StateUndetermined,
		},
		{
			name: "stretch_active",
			snap: stretch,
			dc:   dcA, want: status.StateActive,
		},
		{
			name: "stretch_standby",
			snap: stretch,
			dc:   dcB, want: status.StateStandby,
		},
		{
			name: "stretch_consul_without_leader_is_undetermined",
			snap: with(stretch, func(s *collect.Snapshot) { s.Voters[0].KV.Data.Meta.KnownLeader = false }),
			dc:   dcA, want: status.StateUndetermined,
		},
		{
			name: "forced_below_an_undetermined_other_active_is_undetermined",
			snap: forcedBelowUndetermined(dcA),
			dc:   dcA, want: status.StateUndetermined,
		},
		{
			name: "forced_below_an_undetermined_other_active_is_undetermined_for_the_forced",
			snap: forcedBelowUndetermined(dcA),
			dc:   dcB, want: status.StateUndetermined,
		},
		{
			name: "forced_below_an_undetermined_same_active",
			snap: forcedBelowUndetermined(dcB),
			dc:   dcB, want: status.StateActive,
		},
		{
			name: "forced_seen_alone_is_undetermined_for_its_active",
			snap: forcedSeenAlone,
			dc:   dcA, want: status.StateUndetermined,
		},
		{
			name: "forced_seen_alone_is_undetermined_for_the_other",
			snap: forcedSeenAlone,
			dc:   dcB, want: status.StateUndetermined,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := status.Build(tt.snap(), testConfig(), now)

			d := datacenter(t, st, tt.dc)
			assert.Equal(t, tt.want, d.State)
			assert.Equal(t, tt.skipped, d.Skipped)
		})
	}
}

// forcedBelowUndetermined: dc_b failed over in forced mode to generation 6, then a
// return to active wrote generation 7 to the arbiter, and the KV of dc_a, which may
// hold 7 as well, does not answer.
func forcedBelowUndetermined(active string) func() *collect.Snapshot {
	return with(warm, func(s *collect.Snapshot) {
		forced := stateEntry(6, dcB)
		forced.Value = []byte(`{"generation":6,"active_datacenter":"dc_b","forced":true}`)
		setVoterState(s, dcB, forced)
		voterDown(s, dcA)
		s.Arbiter.Data.State = arbiterRecord(kv.State{Generation: 7, ActiveDatacenter: active})
	})
}

// forcedSeenAlone: only the KV of dc_b answers, with a forced generation 5 that names
// dc_a; the KV of dc_a and the arbiter, a majority, may hold a newer one.
func forcedSeenAlone() *collect.Snapshot {
	return with(warm, func(s *collect.Snapshot) {
		forced := stateEntry(5, dcA)
		forced.Value = []byte(`{"generation":5,"active_datacenter":"dc_a","forced":true}`)
		setVoterState(s, dcB, forced)
		voterDown(s, dcA)
		s.Arbiter = new(unreachable(*s.Arbiter))
	})()
}

func TestBuild_needsRecoveryUnderHigherState(t *testing.T) {
	s := warm()
	setVoterState(s, dcA, stateEntry(5, dcA, dcB))
	setVoterState(s, dcB, stateEntry(5, dcA, dcB))
	startEngine(s, dcB)

	d := datacenter(t, status.Build(s, testConfig(), now), dcB)
	assert.Equal(t, status.StateSplitBrain, d.State)
	assert.True(t, d.NeedsRecovery)
}

// An effective state that cannot be told never names an active or standby
// datacenter, and nothing derived from a role is reported as fine.
func TestBuild_undeterminedNeverGuesses(t *testing.T) {
	snaps := map[string]*collect.Snapshot{
		"majority_unreachable": with(warm, func(s *collect.Snapshot) {
			voterDown(s, dcB)
			s.Arbiter = new(unreachable(*s.Arbiter))
		})(),
		"stretch_no_leader":         with(stretch, func(s *collect.Snapshot) { s.Voters[0].KV.Data.Meta.KnownLeader = false })(),
		"forced_below_undetermined": forcedBelowUndetermined(dcA)(),
		"forced_seen_alone":         forcedSeenAlone(),
	}

	for name, snap := range snaps {
		t.Run(name, func(t *testing.T) {
			st := status.Build(snap, testConfig(), now)

			assert.Nil(t, st.Effective)

			for _, d := range st.Datacenters {
				assert.Equal(t, status.StateUndetermined, d.State, d.Name)
				assert.Equal(t, status.HealthUnknown, d.Health, d.Name)
				assert.Contains(t, d.HealthReasons, status.ReasonRoleUndetermined, d.Name)
			}
		})
	}
}
