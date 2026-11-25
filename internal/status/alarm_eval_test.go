package status_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func heldLease() coord.LeaseStatus {
	return coord.LeaseStatus{State: coord.LeaseHeld, Reason: coord.LeaseReasonRenewed}
}

func evaluate(s *collect.Snapshot, lease coord.LeaseStatus) []status.Finding {
	return status.EvaluateAlarms(status.AlarmInput{Snapshot: s, Status: status.Build(s, testConfig(), now), Lease: lease})
}

func findingOf(t *testing.T, fs []status.Finding, key status.AlarmKey) status.Finding {
	t.Helper()

	for _, f := range fs {
		if f.Key == key {
			return f
		}
	}

	require.Failf(t, "no finding", "%+v", key)

	return status.Finding{}
}

func key(id status.AlarmID, dc string) status.AlarmKey {
	return status.AlarmKey{ID: id, Datacenter: dc}
}

func voterKey(id status.AlarmID, dc, voter string) status.AlarmKey {
	return status.AlarmKey{ID: id, Datacenter: dc, Voter: voter}
}

// forcedState is a record written in forced mode.
func forcedState(generation int64, active string) *consul.KVEntry {
	return entry(kv.StateKey, kv.State{Generation: generation, ActiveDatacenter: active, Forced: true})
}

// roleUndetermined leaves warm without a majority of voters that answer.
func roleUndetermined(s *collect.Snapshot) {
	voterDown(s, dcB)
	s.Arbiter = new(unreachable(*s.Arbiter))
}

func patroniDown(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		cl, _ := s.ClusterOf(dc)
		cl.Patroni = unreachable(cl.Patroni)
	}
}

func nomadDown(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		d := dcOf(s, dc)
		d.Nomad = unreachable(d.Nomad)
	}
}

func voterDownFn(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) { voterDown(s, dc) }
}

func startEngineIn(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) { startEngine(s, dc) }
}

func promoteIn(dc, name string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) { promote(s, dc, name) }
}

func allSourcesDownIn(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) { allSourcesDown(s, dc) }
}

func arbiterDown(s *collect.Snapshot) {
	s.Arbiter = new(unreachable(*s.Arbiter))
}

func tagPrimary(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		cl, _ := s.ClusterOf(dc)
		in := cl.Scope.Data
		in.Nodes = append([]consul.ServiceNode(nil), in.Nodes...)

		for i := range in.Nodes {
			in.Nodes[i].Tags = []string{"primary"}
		}

		cl.Scope.Data = in
	}
}

// leaderIn moves the leader of the stretch cluster to pg-3 of dc_b.
func leaderIn(s *collect.Snapshot) {
	editPatroni(s, dcA, func(p *collect.Patroni) {
		memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
		memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
		p.Leader = "pg-3"
	})
}

func unhealthyEngine(dc string) func(s *collect.Snapshot) {
	return editNomad(dc, func(n *collect.Nomad) { n.Allocs = map[string][]nomad.Alloc{"freeswitch": n.Allocs["freeswitch"]} })
}

func memberDown(name string) func(s *collect.Snapshot) {
	return editMember(name, func(m *collect.Member) {
		m.Probe, m.Err, m.Usable = nil, errDown, false
	})
}

func needsRecovery(dc string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		setVoterState(s, dcA, stateEntry(5, dcA, dc))
		setVoterState(s, dcB, stateEntry(5, dcA, dc))
		s.Arbiter.Data.State = arbiterRecord(kv.State{Generation: 5, ActiveDatacenter: dcA, NeedsRecovery: []string{dc}})
	}
}

func operationIn(t *testing.T, st dcctlv1.Operation_State) func(s *collect.Snapshot) {
	t.Helper()

	return func(s *collect.Snapshot) {
		for i := range s.Voters {
			s.Voters[i].KV.Data.Op = opEntry(t, "operations/op-1", st, observed)
		}
	}
}

func TestEvaluateAlarms(t *testing.T) {
	lost := coord.LeaseStatus{State: coord.LeaseLost, Reason: coord.LeaseReasonCounterStale}
	unknown := coord.LeaseStatus{State: coord.LeaseUnknown, Reason: coord.LeaseReasonUnconfirmed}

	tests := []struct {
		name  string
		snap  func() *collect.Snapshot
		lease *coord.LeaseStatus
		key   status.AlarmKey
		want  status.Signal
	}{
		{
			name: "conflict_raise",
			snap: with(warm, func(s *collect.Snapshot) { setVoterState(s, dcB, forcedState(5, dcB)) }),
			key:  key(status.AlarmConflict, ""), want: status.SignalRaise,
		},
		{name: "conflict_clear", snap: warm, key: key(status.AlarmConflict, ""), want: status.SignalClear},
		{
			name: "conflict_unknown_voter_down",
			snap: with(warm, voterDownFn(dcB)),
			key:  key(status.AlarmConflict, ""), want: status.SignalUnknown,
		},
		{
			name: "uncommitted_raise",
			snap: with(warm, func(s *collect.Snapshot) { setVoterState(s, dcB, stateEntry(6, dcB)) }),
			key:  key(status.AlarmUncommitted, ""), want: status.SignalRaise,
		},
		{name: "uncommitted_clear", snap: warm, key: key(status.AlarmUncommitted, ""), want: status.SignalClear},
		{
			name: "uncommitted_unknown_undetermined",
			snap: with(warm, func(s *collect.Snapshot) {
				setVoterState(s, dcB, stateEntry(6, dcB))
				arbiterDown(s)
			}),
			key: key(status.AlarmUncommitted, ""), want: status.SignalUnknown,
		},
		{
			name: "stale_raise_behind",
			snap: with(warm, func(s *collect.Snapshot) { setVoterState(s, dcB, stateEntry(4, dcA)) }),
			key:  key(status.AlarmStale, dcB), want: status.SignalRaise,
		},
		{
			name: "stale_raise_key_missing",
			snap: with(warm, func(s *collect.Snapshot) { setVoterState(s, dcB, nil) }),
			key:  key(status.AlarmStale, dcB), want: status.SignalRaise,
		},
		{name: "stale_clear", snap: warm, key: key(status.AlarmStale, dcB), want: status.SignalClear},
		{name: "stale_unknown_voter_down", snap: with(warm, voterDownFn(dcB)), key: key(status.AlarmStale, dcB), want: status.SignalUnknown},
		{
			// dc_a holds a forced generation 6; dc_b and the arbiter, unreachable, may
			// hold a committed 7 that dc_a misses.
			name: "stale_unknown_effective_undecided",
			snap: with(warm, func(s *collect.Snapshot) {
				setVoterState(s, dcA, forcedState(6, dcA))
				setVoterState(s, dcB, stateEntry(7, dcB))
				s.Arbiter.Data.State = arbiterRecord(kv.State{Generation: 7, ActiveDatacenter: dcB})
				voterDown(s, dcB)
				arbiterDown(s)
			}),
			key: key(status.AlarmStale, dcA), want: status.SignalUnknown,
		},
		{name: "split_brain_raise", snap: with(warm, startEngineIn(dcB)), key: key(status.AlarmSplitBrain, dcB), want: status.SignalRaise},
		{name: "split_brain_clear", snap: warm, key: key(status.AlarmSplitBrain, dcB), want: status.SignalClear},
		{name: "split_brain_clear_active", snap: warm, key: key(status.AlarmSplitBrain, dcA), want: status.SignalClear},
		{
			name: "split_brain_unknown_nomad_down",
			snap: with(warm, all(startEngineIn(dcB), nomadDown(dcB))),
			key:  key(status.AlarmSplitBrain, dcB), want: status.SignalUnknown,
		},
		{
			name: "split_brain_unknown_role_undetermined",
			snap: with(warm, all(startEngineIn(dcB), roleUndetermined)),
			key:  key(status.AlarmSplitBrain, dcB), want: status.SignalUnknown,
		},
		{
			name: "split_brain_global_raise_role_undetermined",
			snap: with(warm, all(startEngineIn(dcB), roleUndetermined)),
			key:  key(status.AlarmSplitBrain, ""), want: status.SignalRaise,
		},
		{
			name: "split_brain_global_unknown_one_nomad_down",
			snap: with(warm, all(roleUndetermined, nomadDown(dcB))),
			key:  key(status.AlarmSplitBrain, ""), want: status.SignalUnknown,
		},
		{
			name: "split_brain_global_clear_one_running",
			snap: with(warm, roleUndetermined),
			key:  key(status.AlarmSplitBrain, ""), want: status.SignalClear,
		},
		{name: "split_brain_global_clear_role_known", snap: warm, key: key(status.AlarmSplitBrain, ""), want: status.SignalClear},
		{name: "db_split_raise", snap: with(warm, promoteIn(dcB, "pg-b1")), key: key(status.AlarmDBSplit, dcB), want: status.SignalRaise},
		{name: "db_split_clear", snap: warm, key: key(status.AlarmDBSplit, dcB), want: status.SignalClear},
		{
			name: "db_split_unknown_patroni_down",
			snap: with(warm, patroniDown(dcB)),
			key:  key(status.AlarmDBSplit, dcB), want: status.SignalUnknown,
		},
		{
			name: "db_split_unknown_role_undetermined",
			snap: with(warm, all(promoteIn(dcB, "pg-b1"), roleUndetermined)),
			key:  key(status.AlarmDBSplit, dcB), want: status.SignalUnknown,
		},
		{
			name: "multiple_primaries_raise_patroni",
			snap: with(warm, promoteIn(dcB, "pg-b1")),
			key:  key(status.AlarmMultiplePrimaries, ""), want: status.SignalRaise,
		},
		{
			name: "multiple_primaries_raise_role_undetermined",
			snap: with(warm, all(promoteIn(dcB, "pg-b1"), roleUndetermined)),
			key:  key(status.AlarmMultiplePrimaries, ""), want: status.SignalRaise,
		},
		{
			name: "multiple_primaries_raise_consul",
			snap: with(warm, tagPrimary(dcA)),
			key:  key(status.AlarmMultiplePrimaries, ""), want: status.SignalRaise,
		},
		{name: "multiple_primaries_clear", snap: warm, key: key(status.AlarmMultiplePrimaries, ""), want: status.SignalClear},
		{name: "multiple_primaries_clear_stretch", snap: stretch, key: key(status.AlarmMultiplePrimaries, ""), want: status.SignalClear},
		{
			name: "multiple_primaries_unknown_member_down",
			snap: with(warm, all(promoteIn(dcB, "pg-b1"), memberDown("pg-b1"))),
			key:  key(status.AlarmMultiplePrimaries, ""), want: status.SignalUnknown,
		},
		{
			name: "multiple_primaries_unknown_patroni_down",
			snap: with(warm, patroniDown(dcB)),
			key:  key(status.AlarmMultiplePrimaries, ""), want: status.SignalUnknown,
		},
		{
			name: "db_primary_missing_raise",
			snap: with(warm, dropLeader(dcA, "pg-a1")),
			key:  key(status.AlarmDBPrimaryMissing, dcA), want: status.SignalRaise,
		},
		{
			name: "db_primary_missing_raise_stretch",
			snap: with(stretch, dropLeader(dcA, "pg-1")),
			key:  key(status.AlarmDBPrimaryMissing, dcA), want: status.SignalRaise,
		},
		{name: "db_primary_missing_clear", snap: warm, key: key(status.AlarmDBPrimaryMissing, dcA), want: status.SignalClear},
		{name: "db_primary_missing_clear_inactive", snap: warm, key: key(status.AlarmDBPrimaryMissing, dcB), want: status.SignalClear},
		{
			name: "db_primary_missing_unknown_patroni_down",
			snap: with(warm, patroniDown(dcA)),
			key:  key(status.AlarmDBPrimaryMissing, dcA), want: status.SignalUnknown,
		},
		{
			name: "db_primary_missing_unknown_role_undetermined",
			snap: with(warm, all(dropLeader(dcA, "pg-a1"), roleUndetermined)),
			key:  key(status.AlarmDBPrimaryMissing, dcA), want: status.SignalUnknown,
		},
		{
			name: "datacenter_unreachable_raise",
			snap: with(warm, allSourcesDownIn(dcB)),
			key:  key(status.AlarmDatacenterUnreachable, dcB), want: status.SignalRaise,
		},
		{name: "datacenter_unreachable_clear", snap: warm, key: key(status.AlarmDatacenterUnreachable, dcB), want: status.SignalClear},
		{
			name: "datacenter_unreachable_clear_one_source",
			snap: with(warm, all(nomadDown(dcB), patroniDown(dcB))),
			key:  key(status.AlarmDatacenterUnreachable, dcB), want: status.SignalClear,
		},
		{name: "db_leader_mismatch_raise", snap: with(stretch, leaderIn), key: key(status.AlarmDBLeaderMismatch, ""), want: status.SignalRaise},
		{name: "db_leader_mismatch_clear", snap: stretch, key: key(status.AlarmDBLeaderMismatch, ""), want: status.SignalClear},
		{
			name: "db_leader_mismatch_unknown_patroni_down",
			snap: with(stretch, all(leaderIn, patroniDown(dcA))),
			key:  key(status.AlarmDBLeaderMismatch, ""), want: status.SignalUnknown,
		},
		{
			name: "db_leader_mismatch_unknown_no_leader",
			snap: with(stretch, dropLeader(dcA, "pg-1")),
			key:  key(status.AlarmDBLeaderMismatch, ""), want: status.SignalUnknown,
		},
		{name: "lease_lost_raise", snap: warm, lease: &lost, key: key(status.AlarmLeaseLost, ""), want: status.SignalRaise},
		{name: "lease_lost_clear", snap: warm, key: key(status.AlarmLeaseLost, ""), want: status.SignalClear},
		{name: "lease_lost_unknown", snap: warm, lease: &unknown, key: key(status.AlarmLeaseLost, ""), want: status.SignalUnknown},
		{name: "apps_degraded_raise", snap: with(warm, unhealthyEngine(dcA)), key: key(status.AlarmAppsDegraded, dcA), want: status.SignalRaise},
		{name: "apps_degraded_clear", snap: warm, key: key(status.AlarmAppsDegraded, dcA), want: status.SignalClear},
		{name: "apps_degraded_clear_inactive", snap: warm, key: key(status.AlarmAppsDegraded, dcB), want: status.SignalClear},
		{
			name: "apps_degraded_unknown_nomad_down",
			snap: with(warm, all(unhealthyEngine(dcA), nomadDown(dcA))),
			key:  key(status.AlarmAppsDegraded, dcA), want: status.SignalUnknown,
		},
		{
			name: "apps_degraded_unknown_role_undetermined",
			snap: with(warm, all(unhealthyEngine(dcA), roleUndetermined)),
			key:  key(status.AlarmAppsDegraded, dcA), want: status.SignalUnknown,
		},
		{name: "db_degraded_raise_member", snap: with(warm, memberDown("pg-b2")), key: key(status.AlarmDBDegraded, dcB), want: status.SignalRaise},
		{
			name: "db_degraded_raise_paused",
			snap: with(warm, editProbe("pg-a2", func(p *patroni.State) { p.Pause = true })),
			key:  key(status.AlarmDBDegraded, dcA), want: status.SignalRaise,
		},
		{
			name: "db_degraded_raise_not_running",
			snap: with(warm, editProbe("pg-b2", func(p *patroni.State) { p.State = "starting" })),
			key:  key(status.AlarmDBDegraded, dcB), want: status.SignalRaise,
		},
		{name: "db_degraded_raise_patroni_down", snap: with(warm, patroniDown(dcB)), key: key(status.AlarmDBDegraded, dcB), want: status.SignalRaise},
		{name: "db_degraded_clear", snap: warm, key: key(status.AlarmDBDegraded, dcB), want: status.SignalClear},
		{
			name: "db_degraded_unknown_member_datacenter",
			snap: with(stretch, editMember("pg-4", func(m *collect.Member) { m.Datacenter = "" })),
			key:  key(status.AlarmDBDegraded, dcB), want: status.SignalUnknown,
		},
		{
			name: "standby_not_following_raise_not_streaming",
			snap: with(warm, editProbe("pg-b1", func(p *patroni.State) { p.ReplicationState = "stopped" })),
			key:  key(status.AlarmStandbyNotFollowing, dcB), want: status.SignalRaise,
		},
		{
			name: "standby_not_following_raise_other_upstream",
			snap: with(warm, setStandbyHost("10.0.1.2")),
			key:  key(status.AlarmStandbyNotFollowing, dcB), want: status.SignalRaise,
		},
		{name: "standby_not_following_clear", snap: warm, key: key(status.AlarmStandbyNotFollowing, dcB), want: status.SignalClear},
		{
			name: "standby_not_following_clear_needs_recovery",
			snap: with(warm, all(needsRecovery(dcB), editProbe("pg-b1", func(p *patroni.State) { p.ReplicationState = "stopped" }))),
			key:  key(status.AlarmStandbyNotFollowing, dcB), want: status.SignalClear,
		},
		{
			name: "standby_not_following_unknown_active_down",
			snap: with(warm, patroniDown(dcA)),
			key:  key(status.AlarmStandbyNotFollowing, dcB), want: status.SignalUnknown,
		},
		{
			name: "standby_not_following_unknown_patroni_down",
			snap: with(warm, patroniDown(dcB)),
			key:  key(status.AlarmStandbyNotFollowing, dcB), want: status.SignalUnknown,
		},
		{
			name: "voter_unreachable_raise",
			snap: with(warm, voterDownFn(dcB)),
			key:  voterKey(status.AlarmVoterUnreachable, dcB, dcB), want: status.SignalRaise,
		},
		{
			name: "voter_unreachable_raise_no_leader",
			snap: with(warm, func(s *collect.Snapshot) { s.Voters[1].KV.Data.Meta.KnownLeader = false }),
			key:  voterKey(status.AlarmVoterUnreachable, dcB, dcB), want: status.SignalRaise,
		},
		{
			name: "voter_unreachable_raise_arbiter",
			snap: with(warm, arbiterDown),
			key:  voterKey(status.AlarmVoterUnreachable, "", coord.ArbiterName), want: status.SignalRaise,
		},
		{name: "voter_unreachable_clear", snap: warm, key: voterKey(status.AlarmVoterUnreachable, dcB, dcB), want: status.SignalClear},
		{name: "quorum_lost_raise", snap: with(warm, roleUndetermined), key: key(status.AlarmQuorumLost, ""), want: status.SignalRaise},
		{name: "quorum_lost_clear", snap: warm, key: key(status.AlarmQuorumLost, ""), want: status.SignalClear},
		{
			name: "arbiter_uninitialized_raise",
			snap: with(warm, func(s *collect.Snapshot) { s.Arbiter.Data = collect.Arbiter{} }),
			key:  voterKey(status.AlarmArbiterUninitialized, "", coord.ArbiterName), want: status.SignalRaise,
		},
		{
			name: "arbiter_uninitialized_clear",
			snap: warm,
			key:  voterKey(status.AlarmArbiterUninitialized, "", coord.ArbiterName), want: status.SignalClear,
		},
		{
			name: "arbiter_uninitialized_unknown",
			snap: with(warm, func(s *collect.Snapshot) {
				s.Arbiter.Data = collect.Arbiter{}
				arbiterDown(s)
			}),
			key: voterKey(status.AlarmArbiterUninitialized, "", coord.ArbiterName), want: status.SignalUnknown,
		},
		{
			name: "operation_interrupted_raise",
			snap: with(warm, operationIn(t, dcctlv1.Operation_INTERRUPTED)),
			key:  key(status.AlarmOperationInterrupted, ""), want: status.SignalRaise,
		},
		{
			name: "operation_interrupted_clear_running",
			snap: with(warm, operationIn(t, dcctlv1.Operation_RUNNING)),
			key:  key(status.AlarmOperationInterrupted, ""), want: status.SignalClear,
		},
		{name: "operation_interrupted_clear", snap: warm, key: key(status.AlarmOperationInterrupted, ""), want: status.SignalClear},
		{
			name: "operation_interrupted_unknown_invalid_copy",
			snap: with(warm, func(s *collect.Snapshot) {
				s.Voters[1].KV.Data.Op = &consul.KVEntry{Key: kv.OpKey, Value: []byte("{")}
			}),
			key: key(status.AlarmOperationInterrupted, ""), want: status.SignalUnknown,
		},
		{
			name: "operation_interrupted_unknown_no_leader",
			snap: with(warm, func(s *collect.Snapshot) { s.Voters[1].KV.Data.Meta.KnownLeader = false }),
			key:  key(status.AlarmOperationInterrupted, ""), want: status.SignalUnknown,
		},
		{
			name: "operation_interrupted_unknown_voter_down",
			snap: with(warm, voterDownFn(dcB)),
			key:  key(status.AlarmOperationInterrupted, ""), want: status.SignalUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lease := heldLease()
			if tt.lease != nil {
				lease = *tt.lease
			}

			f := findingOf(t, evaluate(tt.snap(), lease), tt.key)
			assert.Equal(t, tt.want, f.Signal, f.Detail)

			if tt.want == status.SignalRaise {
				assert.NotEmpty(t, f.Detail)
				assert.Equal(t, alarmSeverity(tt.key.ID), f.Severity)
			}
		})
	}
}

// alarmSeverity is the severity of the catalog, which only datacenter_unreachable
// changes with the role of the datacenter.
func alarmSeverity(id status.AlarmID) status.Severity {
	critical := []status.AlarmID{
		status.AlarmConflict, status.AlarmUncommitted, status.AlarmSplitBrain, status.AlarmDBSplit,
		status.AlarmMultiplePrimaries, status.AlarmDBPrimaryMissing,
	}
	if slices.Contains(critical, id) {
		return status.SeverityCritical
	}

	return status.SeverityWarning
}

func TestEvaluateAlarms_healthyClearsEverything(t *testing.T) {
	for name, snap := range map[string]func() *collect.Snapshot{"warm": warm, "stretch": stretch} {
		t.Run(name, func(t *testing.T) {
			fs := evaluate(snap(), heldLease())
			require.NotEmpty(t, fs)

			for _, f := range fs {
				assert.Equal(t, status.SignalClear, f.Signal, "%+v: %s", f.Key, f.Detail)
			}
		})
	}
}

// Every alarm is evaluated for each of its instances, so the tracker sees each one
// on every cycle.
func TestEvaluateAlarms_instances(t *testing.T) {
	ids := make(map[status.AlarmID]bool)

	for _, f := range evaluate(warm(), heldLease()) {
		ids[f.Key.ID] = true
	}

	for _, f := range evaluate(stretch(), heldLease()) {
		ids[f.Key.ID] = true
	}

	for _, id := range status.AlarmIDs() {
		assert.True(t, ids[id], id)
	}
}

func TestEvaluateAlarms_datacenterUnreachableSeverity(t *testing.T) {
	tests := []struct {
		name       string
		snap       func() *collect.Snapshot
		dc         string
		lastActive string
		want       status.Severity
	}{
		{name: "inactive", snap: with(warm, allSourcesDownIn(dcB)), dc: dcB, want: status.SeverityWarning},
		{name: "active", snap: with(warm, allSourcesDownIn(dcA)), dc: dcA, want: status.SeverityCritical},
		{
			name: "last_active_when_role_undetermined",
			snap: with(warm, all(allSourcesDownIn(dcA), roleUndetermined)),
			dc:   dcA, lastActive: dcA, want: status.SeverityCritical,
		},
		{
			name: "inactive_last_active_when_role_undetermined",
			snap: with(warm, all(allSourcesDownIn(dcB), roleUndetermined)),
			dc:   dcB, lastActive: dcA, want: status.SeverityWarning,
		},
		{
			// Any datacenter may be the active one.
			name: "role_undetermined_never_known",
			snap: with(warm, all(allSourcesDownIn(dcB), roleUndetermined)),
			dc:   dcB, want: status.SeverityCritical,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.snap()
			fs := status.EvaluateAlarms(status.AlarmInput{
				Snapshot: s, Status: status.Build(s, testConfig(), now), Lease: heldLease(), LastActive: tt.lastActive,
			})

			f := findingOf(t, fs, key(status.AlarmDatacenterUnreachable, tt.dc))
			assert.Equal(t, status.SignalRaise, f.Signal)
			assert.Equal(t, tt.want, f.Severity)
		})
	}
}

// Without a known active datacenter nothing that depends on the role is raised.
func TestEvaluateAlarms_roleUndeterminedNeverGuesses(t *testing.T) {
	s := warm()
	startEngine(s, dcB)
	promote(s, dcB, "pg-b1")
	dropLeader(dcA, "pg-a1")(s)
	unhealthyEngine(dcA)(s)
	roleUndetermined(s)

	roleDependent := []status.AlarmID{
		status.AlarmDBSplit, status.AlarmDBPrimaryMissing, status.AlarmAppsDegraded, status.AlarmStandbyNotFollowing,
		status.AlarmDBLeaderMismatch,
	}

	for _, f := range evaluate(s, coord.LeaseStatus{State: coord.LeaseUnknown, Reason: coord.LeaseReasonStateUndetermined}) {
		if f.Key.ID == status.AlarmSplitBrain && f.Key.Datacenter != "" {
			assert.Equal(t, status.SignalUnknown, f.Signal, "%+v", f.Key)
		}

		for _, id := range roleDependent {
			if f.Key.ID == id {
				assert.Equal(t, status.SignalUnknown, f.Signal, "%+v", f.Key)
			}
		}
	}
}

func TestEvaluateAlarms_voterAlarm(t *testing.T) {
	s := warm()
	arbiterDown(s)

	f := findingOf(t, evaluate(s, heldLease()), voterKey(status.AlarmVoterUnreachable, "", coord.ArbiterName))
	assert.Equal(t, status.VoterArbiter, f.VoterType)
	assert.Equal(t, "refused", f.Facts["reason"])
}

// Every alarm is evaluated for what its scope says, which the metrics rely on for the
// points of alarms that are not raised.
func TestAlarmID_Scope(t *testing.T) {
	for _, id := range status.AlarmIDs() {
		assert.NotEmpty(t, id.Scope(), id)
	}

	for _, f := range evaluate(warm(), heldLease()) {
		k := f.Key

		switch k.ID.Scope() {
		case status.AlarmScopeInstallation:
			assert.Equal(t, status.AlarmKey{ID: k.ID}, k)
		case status.AlarmScopeDatacenter:
			if k.ID == status.AlarmSplitBrain && k.Datacenter == "" {
				continue
			}

			assert.NotEmpty(t, k.Datacenter, k)
			assert.Empty(t, k.Voter, k)
		case status.AlarmScopeVoter:
			assert.NotEmpty(t, k.Voter, k)
		case status.AlarmScopeArbiter:
			assert.Equal(t, status.AlarmKey{ID: k.ID, Voter: coord.ArbiterName}, k)
		}
	}
}
