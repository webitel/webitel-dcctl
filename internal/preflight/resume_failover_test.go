package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
)

// failoverOp is an interrupted failover from dc_a to dc_b whose promotion is in state
// promote; with others it repoints them.
func failoverOp(promote dcctlv1.Operation_Step_State, others ...string) *dcctlv1.Operation {
	op := operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB)
	op.Type = dcctlv1.Operation_FAILOVER

	step := func(typ dcctlv1.Operation_Step_Type, dc string, st dcctlv1.Operation_Step_State) *dcctlv1.Operation_Step {
		return &dcctlv1.Operation_Step{Type: typ, Datacenter: "datacenters/" + dc, State: st}
	}

	op.Steps = []*dcctlv1.Operation_Step{
		step(dcctlv1.Operation_Step_GENERATION, dcB, dcctlv1.Operation_Step_SUCCEEDED),
		step(dcctlv1.Operation_Step_APPS_STOP, dcA, dcctlv1.Operation_Step_SUCCEEDED),
		step(dcctlv1.Operation_Step_PATRONI_DEMOTE, dcA, dcctlv1.Operation_Step_SUCCEEDED),
		step(dcctlv1.Operation_Step_PATRONI_PROMOTE, dcB, promote),
		step(dcctlv1.Operation_Step_APPS_START, dcB, dcctlv1.Operation_Step_PENDING),
	}

	for _, dc := range others {
		op.Steps = append(op.Steps, step(dcctlv1.Operation_Step_PATRONI_REPOINT, dc, dcctlv1.Operation_Step_PENDING))
	}

	return op
}

// A failover resumed before its promotion decides the isolation and the WAL its
// promotion discards anew, against the standbys it repoints only.
func TestResumeFailoverBeforePromote(t *testing.T) {
	s := withC(warm(), 4950)
	setOp(t, s, failoverOp(dcctlv1.Operation_Step_FAILED, dcC))

	cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
	require.NoError(t, err)

	iso := find(t, cs, check.CoordIsolation, "")
	assert.Equal(t, check.ConfirmIsolation, iso.Confirmation)
	assert.Equal(t, []any{map[string]any{"datacenter": dcA, "patroni_reachable": true, "role": "primary"}}, iso.Facts["datacenters"])

	lsn := find(t, cs, check.PatroniTargetMaxLSN, dcB)
	assert.Equal(t, check.Fail, lsn.Result)
	assert.Contains(t, lsn.Detail, dcC)
	requireStruct(t, cs...)

	v := check.Decide(cs, check.Confirmations{Isolation: true})
	assert.Equal(t, check.NeedsConfirmation, v.Outcome)

	v = check.Decide(cs, check.Confirmations{Isolation: true, NotMaxLSN: true})
	assert.Equal(t, check.Allowed, v.Outcome)
	assert.Equal(t, check.Confirmations{Isolation: true, NotMaxLSN: true}, v.Confirmed)
}

func TestResumeFailoverChecks(t *testing.T) {
	tests := []struct {
		name string
		op   *dcctlv1.Operation
		// lsn is the result of patroni.target_max_lsn; empty when the set has none.
		lsn       check.Result
		isolation bool
	}{
		{
			name: "standby_not_on_route", op: failoverOp(dcctlv1.Operation_Step_PENDING),
			lsn: check.Skip, isolation: true,
		},
		{name: "promoted", op: failoverOp(dcctlv1.Operation_Step_SUCCEEDED, dcC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := withC(warm(), 4950)
			setOp(t, s, tt.op)

			cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
			require.NoError(t, err)

			var (
				lsn       check.Result
				isolation bool
			)

			for _, c := range cs {
				if c.ID == check.PatroniTargetMaxLSN {
					lsn = c.Result

					assert.Contains(t, c.Facts, "target", "the facts of the target are kept whatever the result")
				}

				isolation = isolation || c.ID == check.CoordIsolation
			}

			assert.Equal(t, tt.lsn, lsn)
			assert.Equal(t, tt.isolation, isolation)
		})
	}
}
