package preflight

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// precondition is a change of a healthy snapshot that breaks the check id of an action;
// without mutate, the datacenter of the action is one that is not configured.
type precondition struct {
	id     check.ID
	mutate func(t *testing.T, s *collect.Snapshot)
}

func onSnap(fn func(s *collect.Snapshot)) func(t *testing.T, s *collect.Snapshot) {
	return func(_ *testing.T, s *collect.Snapshot) { fn(s) }
}

func runningOp(t *testing.T, s *collect.Snapshot) {
	t.Helper()
	setOp(t, s, operation("op-2", dcctlv1.Operation_RUNNING, dcB, dcB))
}

func lockHeld(s *collect.Snapshot) { s.Voters[1].KV.Data.Lock = lockEntry("s-1") }

func emptyArbiter(s *collect.Snapshot) { s.Arbiter = new(up(collect.Arbiter{})) }

// Every precondition of apps sync, resolve, init and the arbiter initialization refuses
// the action on its own, through the set of the action and the decision on it; the
// healthy installation is allowed.
func TestPreconditions_installation(t *testing.T) {
	tests := []struct {
		name    string
		action  Action
		params  Params
		healthy func(s *collect.Snapshot)
		given   check.Confirmations
		pre     []precondition
	}{
		{
			name: "apps_sync", action: AppsSync,
			healthy: func(s *collect.Snapshot) { s.Observer = collect.Instance{Datacenter: dcA, Node: "dcctl-" + dcA} },
			pre: []precondition{
				{check.CoordStatePresent, onSnap(uninitialized)},
				{check.CoordNoOperation, runningOp},
				{check.CoordLockFree, onSnap(lockHeld)},
				{check.CoordLeaderReachable, onSnap(func(s *collect.Snapshot) {
					d, _ := s.Datacenter(dcA)
					d.Services.Data.Dcctl[0].Status = "critical"
				})},
				{check.CoordNoConflict, onSnap(conflict)},
				{check.CoordNoUncommitted, onSnap(partial)},
				{check.CoordNoSplit, onSnap(func(s *collect.Snapshot) {
					d, _ := s.Datacenter(dcB)
					d.Nomad = up(runningNomad())
				})},
				{check.PatroniActivePrimary, onSnap(func(s *collect.Snapshot) { cutPatroni(s, dcA) })},
				{check.NomadReachable, onSnap(func(s *collect.Snapshot) { cutNomad(s, dcA) })},
				{check.NomadStagesValid, onSnap(func(s *collect.Snapshot) { invalidStage(s, dcA) })},
				{check.NomadActivePlan, onSnap(func(s *collect.Snapshot) { cutNomad(s, dcA) })},
			},
		},
		{
			name: "resolve", action: Resolve, params: Params{Datacenter: dcB}, healthy: conflict,
			given: check.Confirmations{Isolation: true},
			pre: []precondition{
				{check.ResolveConflict, onSnap(func(s *collect.Snapshot) {
					s.Voters[0] = kvVoter(dcA, state(6, dcB))
					s.Voters[1] = kvVoter(dcB, state(6, dcB))
				})},
				{check.ResolveWinner, nil},
				{check.CoordNoOperation, runningOp},
				{check.CoordLockFree, onSnap(lockHeld)},
				{check.CoordLeaderReachable, onSnap(func(s *collect.Snapshot) {
					d, _ := s.Datacenter(dcB)
					d.Services.Data.Dcctl = nil
				})},
			},
		},
		{
			name: "init", action: Init, params: Params{Datacenter: dcA}, healthy: uninitialized,
			pre: []precondition{
				{check.InitStateAbsent, onSnap(func(s *collect.Snapshot) { s.Voters[1] = kvVoter(dcB, state(2, dcA)) })},
				{check.InitAllReachable, onSnap(func(s *collect.Snapshot) { s.Voters[1].KV = down[collect.KV]() })},
				{check.InitActiveValid, nil},
				{check.InitActiveHasPrimary, onSnap(func(s *collect.Snapshot) { cutPatroni(s, dcB) })},
			},
		},
		{
			name: "arbiter_init", action: ArbiterInit, healthy: emptyArbiter,
			pre: []precondition{
				{check.CoordNoOperation, runningOp},
				{check.InitAllReachable, onSnap(func(s *collect.Snapshot) { s.Voters[1].KV = down[collect.KV]() })},
				{check.ArbiterReachable, onSnap(func(s *collect.Snapshot) { s.Arbiter = new(down[collect.Arbiter]()) })},
				{check.ArbiterEmpty, onSnap(func(s *collect.Snapshot) { s.Arbiter = new(up(collect.Arbiter{Initialized: true})) })},
				{check.ArbiterStatesEqual, onSnap(func(s *collect.Snapshot) { s.Voters[1] = kvVoter(dcB, state(5, dcA, dcB)) })},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthy := func() *collect.Snapshot {
				s := warm()
				tt.healthy(s)

				return s
			}

			v := verdict(t, newInput(healthy(), tt.action, tt.params), tt.given)
			require.Equal(t, check.Allowed, v.Outcome, "failed: %v, unconfirmed: %v", v.Failed, v.Unconfirmed)

			ids := make([]check.ID, 0, len(v.Checks))
			for _, c := range v.Checks {
				ids = append(ids, c.ID)
			}

			for _, p := range tt.pre {
				require.Contains(t, ids, p.id, "%s is in the set of %s", p.id, tt.action)

				t.Run(string(p.id), func(t *testing.T) {
					s, params := healthy(), tt.params
					if p.mutate != nil {
						p.mutate(t, s)
					} else {
						params.Datacenter = "dc_x"
					}

					v := verdict(t, newInput(s, tt.action, params), tt.given)
					assert.NotEqual(t, check.Allowed, v.Outcome)
					assert.True(t, slices.ContainsFunc(v.Failed, func(c check.Check) bool { return c.ID == p.id }),
						"%s fails: %v", p.id, v.Failed)
				})
			}
		})
	}
}

// Resolve needs the isolation confirmed, whatever the mode.
func TestPreconditions_resolveNeedsIsolation(t *testing.T) {
	s := warm()
	conflict(s)

	v := verdict(t, newInput(s, Resolve, Params{Datacenter: dcB}), check.Confirmations{})
	require.Equal(t, check.NeedsConfirmation, v.Outcome)
	require.Len(t, v.Unconfirmed, 1)
	assert.Equal(t, check.CoordIsolation, v.Unconfirmed[0].ID)
}

// A resolve that did not write its record resumes over the conflict, which then does not
// refuse it, and confirms the isolation anew.
func TestResume_resolve(t *testing.T) {
	op := operation("op-r", dcctlv1.Operation_INTERRUPTED, dcB, dcB)
	op.Type = dcctlv1.Operation_RESOLVE_CONFLICT
	op.PreviousGeneration, op.NewGeneration = 6, 7
	op.Steps = []*dcctlv1.Operation_Step{{Type: dcctlv1.Operation_Step_GENERATION, State: dcctlv1.Operation_Step_FAILED}}

	s := warm()
	conflict(s)
	setOp(t, s, op)

	in := newInput(s, Resume, Params{Operation: "op-r"})

	assert.Equal(t, check.Skip, in.noConflict().Result)

	v := verdict(t, in, check.Confirmations{})
	require.Equal(t, check.NeedsConfirmation, v.Outcome, "failed: %v", v.Failed)
	assert.Equal(t, check.CoordIsolation, v.Unconfirmed[0].ID)

	v = verdict(t, in, check.Confirmations{Isolation: true})
	assert.Equal(t, check.Allowed, v.Outcome, "failed: %v", v.Failed)
}

func TestConflictUnchanged(t *testing.T) {
	a, b := state(6, dcA), state(6, dcB)
	a.Forced, b.Forced = true, true

	view := coord.Evaluate([]coord.VoterState{
		coord.PresentState(dcA, coord.KindKV, entry(kv.StateKey, a).Value, 3),
		coord.PresentState(dcB, coord.KindKV, entry(kv.StateKey, b).Value, 3),
	})
	require.Len(t, view.Conflict, 2)

	assert.Equal(t, check.Pass, ConflictUnchanged(view, 6, dcB).Result)
	assert.Equal(t, check.Fail, ConflictUnchanged(view, 5, dcB).Result, "another generation")
	assert.Equal(t, check.Fail, ConflictUnchanged(view, 6, dcC).Result, "no record of the winner")
	assert.Equal(t, check.Fail, ConflictUnchanged(coord.View{}, 6, dcB).Result, "no conflict")
}

// The record of a resolve or an initialization is never reverted by an abandon: the
// abandon is refused while voters hold it, unless an initialization committed it.
func TestAbandonable(t *testing.T) {
	present := func(name string, s kv.State) coord.VoterState {
		return coord.PresentState(name, coord.KindKV, entry(kv.StateKey, s).Value, 42)
	}
	absent := func(name string) coord.VoterState {
		return coord.VoterState{Name: name, Kind: coord.KindKV, Status: coord.StatusAbsent}
	}
	down := func(name string) coord.VoterState {
		return coord.VoterState{Name: name, Kind: coord.KindKV, Status: coord.StatusUnreachable, Err: errDown}
	}

	a, b := state(6, dcA), state(6, dcB)
	a.Forced, b.Forced = true, true
	resolved := kv.State{Generation: 7, ActiveDatacenter: dcB, OpID: "op-r"}
	first := kv.State{Generation: 1, ActiveDatacenter: dcA, OpID: "op-i"}

	resolve := &dcctlv1.Operation{
		Name: "operations/op-r", Type: dcctlv1.Operation_RESOLVE_CONFLICT, Target: "datacenters/" + dcB,
		PreviousGeneration: 6, NewGeneration: 7,
	}
	initialize := &dcctlv1.Operation{
		Name: "operations/op-i", Type: dcctlv1.Operation_INITIALIZE, Target: "datacenters/" + dcA,
		NewGeneration: 1,
	}

	tests := []struct {
		name   string
		op     *dcctlv1.Operation
		voters []coord.VoterState
		want   check.Result
	}{
		{name: "resolve not written", op: resolve, voters: []coord.VoterState{present(dcA, a), present(dcB, b)}, want: check.Pass},
		{name: "resolve written in part", op: resolve, voters: []coord.VoterState{present(dcA, a), present(dcB, resolved)}, want: check.Fail},
		{name: "init not written", op: initialize, voters: []coord.VoterState{absent(dcA), absent(dcB)}, want: check.Pass},
		{name: "init written in part", op: initialize, voters: []coord.VoterState{present(dcA, first), absent(dcB)}, want: check.Fail},
		{name: "init committed", op: initialize, voters: []coord.VoterState{present(dcA, first), present(dcB, first)}, want: check.Pass},
		{name: "resolve may be held where not read", op: resolve, voters: []coord.VoterState{present(dcA, a), present(dcB, b), down(dcC)}, want: check.Fail},
		{name: "resolve may be held where the key is lost", op: resolve, voters: []coord.VoterState{present(dcA, a), present(dcB, b), absent(dcC)}, want: check.Fail},
		{name: "init may be held where not read", op: initialize, voters: []coord.VoterState{absent(dcA), down(dcB)}, want: check.Fail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := GenerationDetermined(tt.op, coord.Evaluate(tt.voters))
			assert.Equal(t, tt.want, c.Result, c.Detail)
			assert.Equal(t, check.Block, c.Effect, "no confirmation reverts it")
		})
	}

	c := GenerationDetermined(initialize, coord.Evaluate([]coord.VoterState{present(dcA, first), absent(dcB)}))
	assert.Contains(t, c.Detail, "consul kv delete -cas")
	assert.Equal(t, map[string]any{dcA: uint64(42)}, c.Facts["modify_index"])

	c = GenerationDetermined(resolve, coord.Evaluate([]coord.VoterState{present(dcA, a), present(dcB, b), down(dcC)}))
	assert.Contains(t, c.Detail, "abandon once every voter answers")
}
