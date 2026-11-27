package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
)

// recoveryOp is an interrupted operation of typ on dc_b whose steps are in the states
// given, in order.
func recoveryOp(typ dcctlv1.Operation_Type, steps ...*dcctlv1.Operation_Step) *dcctlv1.Operation {
	op := operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB)
	op.Type, op.Steps = typ, steps

	return op
}

func stepIn(typ dcctlv1.Operation_Step_Type, st dcctlv1.Operation_Step_State) *dcctlv1.Operation_Step {
	return &dcctlv1.Operation_Step{Type: typ, Datacenter: "datacenters/" + dcB, State: st}
}

// A resumed demote or repoint whose Patroni step did not succeed confirms the discard
// anew, on the divergence measured now; once the step succeeded nothing is asked.
func TestResumeRecoveryDiscard(t *testing.T) {
	const (
		demote  = dcctlv1.Operation_Step_PATRONI_DEMOTE
		repoint = dcctlv1.Operation_Step_PATRONI_REPOINT
		follow  = dcctlv1.Operation_Step_PATRONI_FOLLOW
		ok      = dcctlv1.Operation_Step_SUCCEEDED
	)

	tests := []struct {
		name string
		op   *dcctlv1.Operation
		// asked is set when patroni.divergence of dc_b fails in the set; took makes dc_b a
		// standby of dc_a, which skips it.
		asked, took bool
	}{
		{
			name:  "demote_failed",
			op:    recoveryOp(dcctlv1.Operation_DEMOTE, stepIn(demote, dcctlv1.Operation_Step_FAILED), stepIn(follow, dcctlv1.Operation_Step_PENDING)),
			asked: true,
		},
		{
			name: "demote_done",
			op:   recoveryOp(dcctlv1.Operation_DEMOTE, stepIn(demote, ok), stepIn(follow, dcctlv1.Operation_Step_FAILED)),
		},
		{
			name:  "repoint_failed",
			op:    recoveryOp(dcctlv1.Operation_REPOINT, stepIn(repoint, dcctlv1.Operation_Step_FAILED)),
			asked: true,
		},
		{
			name: "demote_took_before_the_crash",
			op:   recoveryOp(dcctlv1.Operation_DEMOTE, stepIn(demote, dcctlv1.Operation_Step_FAILED), stepIn(follow, dcctlv1.Operation_Step_PENDING)),
			took: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := warm()
			if !tt.took {
				diverged(s)
			}

			setOp(t, s, tt.op)

			cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
			require.NoError(t, err)
			requireStruct(t, cs...)

			if tt.took {
				assert.Equal(t, check.Skip, find(t, cs, check.PatroniDivergence, dcB).Result)

				return
			}

			found := false

			for _, c := range cs {
				if c.ID == check.PatroniDivergence {
					found = true

					assert.Equal(t, dcB, c.Datacenter)
					assert.Equal(t, check.Fail, c.Result)
					require.NotNil(t, c.DiscardBytes)
					assert.Equal(t, int64(400), *c.DiscardBytes)
				}
			}

			assert.Equal(t, tt.asked, found)

			if !tt.asked {
				return
			}

			v := check.Decide(cs, check.Confirmations{MaxDiscardBytes: 399})
			assert.Equal(t, check.NeedsConfirmation, v.Outcome)

			v = check.Decide(cs, check.Confirmations{MaxDiscardBytes: 400})
			assert.Equal(t, check.Allowed, v.Outcome)
			assert.Equal(t, int64(400), v.Confirmed.MaxDiscardBytes)
		})
	}
}

// A resumed demote or mark-recovered whose write did not succeed refuses a payload split.
func TestResumeMarkRecoveredPayloadSplit(t *testing.T) {
	demote := recoveryOp(dcctlv1.Operation_DEMOTE,
		stepIn(dcctlv1.Operation_Step_GENERATION, dcctlv1.Operation_Step_FAILED),
		stepIn(dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_SUCCEEDED),
		stepIn(dcctlv1.Operation_Step_PATRONI_FOLLOW, dcctlv1.Operation_Step_PENDING),
	)

	splitSnap := warm()
	payloadSplit(splitSnap)
	setOp(t, splitSnap, demote)

	demoteChecks, err := Checks(newInput(splitSnap, Resume, Params{Operation: "op-2"}))
	require.NoError(t, err)
	assert.Equal(t, check.Fail, find(t, demoteChecks, check.CoordPayloadSettled, "").Result)

	op := recoveryOp(dcctlv1.Operation_MARK_RECOVERED,
		stepIn(dcctlv1.Operation_Step_PATRONI_FOLLOW, dcctlv1.Operation_Step_SUCCEEDED),
		stepIn(dcctlv1.Operation_Step_GENERATION, dcctlv1.Operation_Step_FAILED),
	)

	for _, split := range []bool{false, true} {
		s := warm()
		if split {
			payloadSplit(s)
		}

		setOp(t, s, op)

		cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
		require.NoError(t, err)

		want := check.Pass
		if split {
			want = check.Fail
		}

		assert.Equal(t, want, find(t, cs, check.CoordPayloadSettled, "").Result, "split %v", split)
	}

	s := warm()
	payloadSplit(s)

	op.Steps[1].State = dcctlv1.Operation_Step_SUCCEEDED
	setOp(t, s, op)

	cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
	require.NoError(t, err)

	for _, c := range cs {
		assert.NotEqual(t, check.CoordPayloadSettled, c.ID, "the write succeeded, nothing is written on resume")
	}
}
