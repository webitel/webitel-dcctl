package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func opWithSteps(st dcctlv1.Operation_State, steps ...*dcctlv1.Operation_Step) *dcctlv1.Operation {
	op := operation("op-2", st, dcB, dcB)
	op.Steps = steps

	return op
}

func step(t dcctlv1.Operation_Step_Type, st dcctlv1.Operation_Step_State, pointOfNoReturn bool) *dcctlv1.Operation_Step {
	return &dcctlv1.Operation_Step{Type: t, State: st, PointOfNoReturn: pointOfNoReturn}
}

func TestOpInterrupted(t *testing.T) {
	p := Params{Operation: "op-2"}

	runCases(t, (*Input).opInterrupted, []caseOf{
		{name: "interrupted", snap: withOp(operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB), nil), action: Resume, params: p, want: check.Pass},
		{name: "running", snap: withOp(operation("op-2", dcctlv1.Operation_RUNNING, dcB, dcB), nil), action: Resume, params: p, want: check.Fail},
		{name: "finished_in_one_copy", snap: func(t *testing.T) *collect.Snapshot {
			t.Helper()

			s := withOp(operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB), nil)(t)
			b, err := kv.EncodeProto(operation("op-2", dcctlv1.Operation_ABANDONED, dcB, dcB))
			require.NoError(t, err)

			s.Voters[0].KV.Data.Op.Value = b

			return s
		}, action: Abandon, params: p, want: check.Fail},
		{name: "other_operation", snap: withOp(operation("op-3", dcctlv1.Operation_INTERRUPTED, dcB, dcB), nil), action: Resume, params: p, want: check.Fail},
		{name: "no_kv_answers", snap: withOp(operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB), func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV = down[collect.KV]()
			}
		}), action: Resume, params: p, want: check.Fail},
	})
}

func TestPointOfNoReturn(t *testing.T) {
	p := Params{Operation: "op-2"}
	before := opWithSteps(dcctlv1.Operation_INTERRUPTED,
		step(dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_SUCCEEDED, false),
		step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_PENDING, true))
	after := opWithSteps(dcctlv1.Operation_INTERRUPTED,
		step(dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_SUCCEEDED, false),
		step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_RUNNING, true))

	runCases(t, (*Input).pointOfNoReturn, []caseOf{
		{name: "before", snap: withOp(before, nil), action: Abandon, params: p, want: check.Pass},
		{name: "maybe_passed", snap: withOp(after, nil), action: Abandon, params: p, want: check.Warn},
		{name: "not_found", snap: snap(nil), action: Abandon, params: p, want: check.Warn},
		{
			name: "failed_step", snap: withOp(opWithSteps(dcctlv1.Operation_INTERRUPTED,
				step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_FAILED, true)), nil),
			action: Abandon, params: p, want: check.Warn,
		},
		{
			name: "unknown_step_state", snap: withOp(opWithSteps(dcctlv1.Operation_INTERRUPTED,
				step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_STATE_UNSPECIFIED, true)), nil),
			action: Abandon, params: p, want: check.Warn,
		},
		{
			name: "skipped_step", snap: withOp(opWithSteps(dcctlv1.Operation_INTERRUPTED,
				step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_SKIPPED, true)), nil),
			action: Abandon, params: p, want: check.Pass,
		},
	})
}

func TestResolve(t *testing.T) {
	runCases(t, (*Input).resolveConflict, []caseOf{
		{name: "conflict", snap: snap(conflict), action: Resolve, want: check.Pass},
		{name: "no_conflict", snap: snap(nil), action: Resolve, want: check.Fail},
		{name: "no_voter_answers", snap: snap(func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV = down[collect.KV]()
			}

			s.Arbiter = new(down[collect.Arbiter]())
		}), action: Resolve, want: check.Fail},
	})

	runCases(t, (*Input).resolveWinner, []caseOf{
		{name: "winner_of_a_record", snap: snap(conflict), action: Resolve, want: check.Pass},
		{name: "winner_of_no_record", snap: snap(func(s *collect.Snapshot) {
			conflict(s)
			withC(s, 4900)
		}), action: Resolve, params: Params{Datacenter: dcC}, want: check.Fail},
	})

	c := newInput(snap(conflict)(t), Resolve, Params{Datacenter: dcB}).resolveWinner()
	assert.Equal(t, map[string]any{dcA: "primary", dcB: "standby_leader"}, c.Facts["roles"])
}

func uninitialized(s *collect.Snapshot) {
	for i := range s.Voters {
		s.Voters[i].KV.Data.State = nil
	}

	s.Arbiter = new(up(collect.Arbiter{}))
}

func TestInit(t *testing.T) {
	a := Params{Datacenter: dcA}

	runCases(t, (*Input).initStateAbsent, []caseOf{
		{name: "absent", snap: snap(uninitialized), action: Init, params: a, want: check.Pass},
		{name: "present", snap: snap(nil), action: Init, params: a, want: check.Fail},
		{name: "kv_unreachable", snap: snap(func(s *collect.Snapshot) {
			uninitialized(s)
			s.Voters[1].KV = down[collect.KV]()
		}), action: Init, params: a, want: check.Fail},
	})

	runCases(t, (*Input).initAllReachable, []caseOf{
		{name: "all", snap: snap(uninitialized), action: Init, params: a, want: check.Pass},
		{name: "one_down", snap: snap(func(s *collect.Snapshot) { s.Voters[1].KV = down[collect.KV]() }), action: Init, params: a, want: check.Fail},
		{name: "without_leader", snap: snap(func(s *collect.Snapshot) { s.Voters[1].KV.Data.Meta.KnownLeader = false }), action: Init, params: a, want: check.Fail},
	})

	runCases(t, (*Input).initActiveValid, []caseOf{
		{name: "configured", snap: snap(uninitialized), action: Init, params: a, want: check.Pass},
		{name: "unknown", snap: snap(uninitialized), action: Init, params: Params{Datacenter: "dc_x"}, want: check.Fail},
	})

	runCases(t, (*Input).initActiveHasPrimary, []caseOf{
		{name: "primary_there", snap: snap(uninitialized), action: Init, params: a, want: check.Pass},
		{name: "primary_elsewhere", snap: snap(uninitialized), action: Init, params: Params{Datacenter: dcB}, want: check.Fail},
		{name: "a_cluster_does_not_answer", snap: snap(func(s *collect.Snapshot) {
			uninitialized(s)
			cutPatroni(s, dcB)
		}), action: Init, params: a, want: check.Fail},
	})
}

func TestArbiter(t *testing.T) {
	empty := func(s *collect.Snapshot) { s.Arbiter = new(up(collect.Arbiter{})) }

	runCases(t, (*Input).arbiterReachable, []caseOf{
		{name: "answers", snap: snap(nil), action: ArbiterInit, want: check.Pass},
		{name: "down", snap: snap(func(s *collect.Snapshot) { s.Arbiter = new(down[collect.Arbiter]()) }), action: ArbiterInit, want: check.Fail},
		{name: "not_configured", snap: snap(func(s *collect.Snapshot) { s.Arbiter = nil }), action: ArbiterInit, want: check.Fail},
	})

	runCases(t, (*Input).arbiterEmpty, []caseOf{
		{name: "empty", snap: snap(empty), action: ArbiterInit, want: check.Pass},
		{name: "initialized", snap: snap(nil), action: ArbiterInit, want: check.Fail},
		{name: "down", snap: snap(func(s *collect.Snapshot) { s.Arbiter = new(down[collect.Arbiter]()) }), action: ArbiterInit, want: check.Fail},
	})

	runCases(t, (*Input).arbiterStatesEqual, []caseOf{
		{name: "equal", snap: snap(empty), action: ArbiterInit, want: check.Pass},
		{name: "different_generation", snap: snap(func(s *collect.Snapshot) {
			empty(s)
			s.Voters[1] = kvVoter(dcB, state(4, dcA))
		}), action: ArbiterInit, want: check.Fail},
		{name: "different_needs_recovery", snap: snap(func(s *collect.Snapshot) {
			empty(s)
			s.Voters[1] = kvVoter(dcB, state(5, dcA, dcB))
		}), action: ArbiterInit, want: check.Fail},
		{name: "kv_unreachable", snap: snap(func(s *collect.Snapshot) {
			empty(s)
			s.Voters[1].KV = down[collect.KV]()
		}), action: ArbiterInit, want: check.Fail},
		{name: "absent", snap: snap(func(s *collect.Snapshot) {
			empty(s)
			uninitialized(s)
		}), action: ArbiterInit, want: check.Fail},
	})
}

func TestVerifyPatroni(t *testing.T) {
	eval := func(dc string) func(in *Input) check.Check {
		return func(in *Input) check.Check { return find(t, in.verifyPatroni(), check.VerifyPatroni, dc) }
	}

	runCases(t, eval(dcA), []caseOf{
		{name: "active_primary", snap: snap(nil), action: Verify, want: check.Pass},
		{name: "active_not_primary", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-a1").Role = patroni.ClusterRoleStandbyLeader })
		}), action: Verify, want: check.Fail},
	})

	runCases(t, eval(dcB), []caseOf{
		{name: "standby_follows", snap: snap(nil), action: Verify, want: check.Pass},
		{name: "standby_not_streaming", snap: snap(func(s *collect.Snapshot) {
			editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) { p.ReplicationState = "stopped" })
		}), action: Verify, want: check.Fail},
		{name: "standby_unreachable_warns", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), action: Verify, want: check.Warn},
	})

	runCases(t, eval(""), []caseOf{
		{name: "stretch_leader_in_active", snap: stretchSnap(nil), action: Verify, want: check.Pass},
		{name: "active_not_known", snap: snap(conflict), action: Verify, want: check.Fail},
	})
}

func TestVerifyJobs(t *testing.T) {
	eval := func(dc string) func(in *Input) check.Check {
		return func(in *Input) check.Check { return find(t, in.verifyJobs(), check.VerifyJobs, dc) }
	}

	runCases(t, eval(dcA), []caseOf{
		{name: "active_healthy", snap: snap(nil), action: Verify, want: check.Pass},
		{name: "active_stopped", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcA)
			d.Nomad = up(stoppedNomad())
		}), action: Verify, want: check.Fail},
	})

	runCases(t, eval(dcB), []caseOf{
		{name: "standby_stopped", snap: snap(nil), action: Verify, want: check.Pass},
		{name: "standby_running", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Nomad = up(runningNomad())
		}), action: Verify, want: check.Fail},
		{name: "standby_unreachable_warns", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcB) }), action: Verify, want: check.Warn},
	})
}

func TestVerifyState(t *testing.T) {
	runCases(t, (*Input).verifyState, []caseOf{
		{name: "everywhere", snap: snap(nil), action: Verify, want: check.Pass},
		{name: "voter_behind", snap: snap(func(s *collect.Snapshot) { s.Voters[1] = kvVoter(dcB, state(4, dcA)) }), action: Verify, want: check.Fail},
		{name: "unreachable_voter_is_not_required", snap: snap(func(s *collect.Snapshot) { s.Voters[1].KV = down[collect.KV]() }), action: Verify, want: check.Pass},
		{name: "forced_without_majority", snap: snap(func(s *collect.Snapshot) {
			f := state(6, dcB)
			f.Forced = true
			s.Voters[1] = kvVoter(dcB, f)
			s.Voters[0].KV = down[collect.KV]()
			s.Arbiter = new(down[collect.Arbiter]())
		}), action: Verify, want: check.Fail},
		{name: "no_effective_state", snap: snap(conflict), action: Verify, want: check.Fail},
	})
}
