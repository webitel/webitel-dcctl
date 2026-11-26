package preflight

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestOpStepsKnown(t *testing.T) {
	p := Params{Operation: "op-2"}
	known := opWithSteps(dcctlv1.Operation_INTERRUPTED,
		step(dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_SUCCEEDED, false),
		step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_PENDING, true))
	unknown := opWithSteps(dcctlv1.Operation_INTERRUPTED,
		step(dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_SUCCEEDED, false),
		step(99, dcctlv1.Operation_Step_PENDING, true))
	unspecified := opWithSteps(dcctlv1.Operation_INTERRUPTED,
		step(dcctlv1.Operation_Step_TYPE_UNSPECIFIED, dcctlv1.Operation_Step_PENDING, false))

	runCases(t, (*Input).opStepsKnown, []caseOf{
		{name: "known", snap: withOp(known, nil), action: Resume, params: p, want: check.Pass},
		{name: "unknown_number", snap: withOp(unknown, nil), action: Resume, params: p, want: check.Fail},
		{name: "unspecified", snap: withOp(unspecified, nil), action: Resume, params: p, want: check.Fail},
		{name: "not_found", snap: snap(nil), action: Resume, params: p, want: check.Skip},
	})

	c := StepsKnown(unknown)
	assert.Contains(t, c.Detail, "step 1")
	assert.Contains(t, c.Detail, "dev")
	assert.Equal(t, []any{map[string]any{"index": 1, "type": int32(99)}}, c.Facts["steps"])
}

func TestOpNotRolledBack(t *testing.T) {
	p := Params{Operation: "op-2"}
	rolledBack := func(detail string) *dcctlv1.Operation_Step {
		st := step(dcctlv1.Operation_Step_APPS_STOP, dcctlv1.Operation_Step_SUCCEEDED, false)
		st.Detail = detail

		return st
	}
	failed := step(dcctlv1.Operation_Step_PATRONI_CATCHUP, dcctlv1.Operation_Step_FAILED, false)

	runCases(t, (*Input).opNotRolledBack, []caseOf{
		{
			name:   "not_rolled_back",
			snap:   withOp(opWithSteps(dcctlv1.Operation_INTERRUPTED, rolledBack("stopped"), failed), nil),
			action: Resume, params: p, want: check.Pass,
		},
		{
			name:   "rolled_back",
			snap:   withOp(opWithSteps(dcctlv1.Operation_INTERRUPTED, rolledBack(kv.RollbackDetail("", nil)), failed), nil),
			action: Resume, params: p, want: check.Fail,
		},
		{
			name: "rollback_failed",
			snap: withOp(opWithSteps(dcctlv1.Operation_INTERRUPTED,
				rolledBack(kv.RollbackDetail("stopped", errors.New("start refused"))), failed), nil),
			action: Resume, params: p, want: check.Fail,
		},
		{name: "not_found", snap: snap(nil), action: Resume, params: p, want: check.Skip},
	})

	c := NotRolledBack(opWithSteps(dcctlv1.Operation_INTERRUPTED, rolledBack(kv.RollbackDetail("", nil)), failed))
	assert.Contains(t, c.Detail, "abandon")
}

func TestOpGenerationDetermined(t *testing.T) {
	p := Params{Operation: "op-2"}
	op := operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB)
	op.PreviousGeneration, op.NewGeneration = 5, 6

	taken := state(6, dcB)
	taken.OpID = "op-2"

	runCases(t, (*Input).opGenerationDetermined, []caseOf{
		{name: "not_written", snap: withOp(op, nil), action: Abandon, params: p, want: check.Pass},
		{name: "uncommitted", snap: withOp(op, func(s *collect.Snapshot) {
			s.Voters[1].KV.Data.State = entry(kv.StateKey, taken)
		}), action: Abandon, params: p, want: check.Pass},
		{name: "undetermined", snap: withOp(op, func(s *collect.Snapshot) {
			s.Voters[1].KV.Data.State = entry(kv.StateKey, taken)
			s.Voters[0].KV = down[collect.KV]()
		}), action: Abandon, params: p, want: check.Fail},
		{
			name: "no_generation", snap: withOp(operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB), nil),
			action: Abandon, params: p, want: check.Skip,
		},
	})

	c := newInput(withOp(op, func(s *collect.Snapshot) {
		s.Voters[1].KV.Data.State = entry(kv.StateKey, taken)
		s.Voters[0].KV = down[collect.KV]()
	})(t), Abandon, p).opGenerationDetermined()
	assert.Equal(t, check.Confirm, c.Effect)
	assert.Equal(t, check.ConfirmIsolation, c.Confirmation)
	assert.Equal(t, check.Allowed, check.Decide([]check.Check{c}, check.Confirmations{Isolation: true}).Outcome)
}

func TestBaseUnchanged(t *testing.T) {
	tests := []struct {
		name       string
		generation int64
		active     string
		want       check.Result
	}{
		{name: "unchanged", generation: 5, active: dcA, want: check.Pass},
		{name: "generation_only", generation: 5, want: check.Pass},
		{name: "generation_changed", generation: 4, active: dcA, want: check.Fail},
		{name: "active_changed", generation: 5, active: dcB, want: check.Fail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newInput(warm(), Switch, Params{Datacenter: dcB})
			c := BaseUnchanged(in.Status.View, false, tt.generation, tt.active)

			assert.Equal(t, tt.want, c.Result, c.Detail)
			assert.Equal(t, check.Block, c.Effect)
			requireStruct(t, c)
		})
	}
}
