package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
)

// stretchFailoverOp is an interrupted failover of stretch from dc_a to dc_b whose step
// of Patroni is in state failover.
func stretchFailoverOp(failover dcctlv1.Operation_Step_State) *dcctlv1.Operation {
	op := operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB)
	op.Type = dcctlv1.Operation_FAILOVER

	step := func(typ dcctlv1.Operation_Step_Type, dc string, st dcctlv1.Operation_Step_State) *dcctlv1.Operation_Step {
		return &dcctlv1.Operation_Step{Type: typ, Datacenter: "datacenters/" + dc, State: st}
	}

	op.Steps = []*dcctlv1.Operation_Step{
		step(dcctlv1.Operation_Step_GENERATION, dcB, dcctlv1.Operation_Step_SUCCEEDED),
		step(dcctlv1.Operation_Step_APPS_STOP, dcA, dcctlv1.Operation_Step_SUCCEEDED),
		step(dcctlv1.Operation_Step_PATRONI_FAILOVER, dcB, failover),
		step(dcctlv1.Operation_Step_APPS_START, dcB, dcctlv1.Operation_Step_PENDING),
	}

	return op
}

// A failover of stretch resumed before its step of Patroni decides the isolation of the
// datacenter it fails over from and the loss of the candidate anew.
func TestResumeStretchFailoverBeforeFailover(t *testing.T) {
	s := stretch()
	noLeader(s)
	editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-3").Lag = i64(300) })
	setOp(t, s, stretchFailoverOp(dcctlv1.Operation_Step_FAILED))

	cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
	require.NoError(t, err)

	iso := find(t, cs, check.CoordIsolation, "")
	dcs, ok := iso.Facts["datacenters"].([]any)
	require.True(t, ok)
	require.Len(t, dcs, 1)
	assert.Equal(t, dcA, dcs[0].(map[string]any)["datacenter"])

	loss := find(t, cs, check.PatroniCandidateLoss, dcB)
	assert.Equal(t, check.Fail, loss.Result)
	require.NotNil(t, loss.DiscardBytes)
	assert.Equal(t, int64(300), *loss.DiscardBytes)

	for _, c := range cs {
		assert.NotEqual(t, check.PatroniTargetMaxLSN, c.ID, "stretch has no other standby cluster")
		assert.NotEqual(t, check.PatroniSourceDiscard, c.ID, "stretch demotes nothing")
	}

	assert.Equal(t, check.NeedsConfirmation, check.Decide(cs, check.Confirmations{Isolation: true, MaxDiscardBytes: 299}).Outcome)

	v := check.Decide(cs, check.Confirmations{Isolation: true, MaxDiscardBytes: 300})
	assert.Equal(t, check.Allowed, v.Outcome)
	assert.Equal(t, check.Confirmations{Isolation: true, MaxDiscardBytes: 300}, v.Confirmed)
}

func TestResumeStretchFailoverAfterFailover(t *testing.T) {
	for _, st := range []dcctlv1.Operation_Step_State{dcctlv1.Operation_Step_SUCCEEDED, dcctlv1.Operation_Step_SKIPPED} {
		t.Run(st.String(), func(t *testing.T) {
			s := stretch()
			setOp(t, s, stretchFailoverOp(st))

			cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
			require.NoError(t, err)

			for _, c := range cs {
				assert.NotEqual(t, check.CoordIsolation, c.ID)
				assert.NotEqual(t, check.PatroniCandidateLoss, c.ID)
			}
		})
	}
}

func TestSwitchCandidate(t *testing.T) {
	in := newInput(stretch(), Switch, Params{Datacenter: dcB})
	members := in.pgCluster().members

	m, ok := SwitchCandidate(members, dcB, 1<<20)
	require.True(t, ok)
	assert.Equal(t, "pg-3", m.Name, "the most WAL first")

	_, ok = SwitchCandidate(members, dcB, 5)
	assert.False(t, ok, "every member of dc_b lags more than the limit")
}

func TestMaximumLagOnFailover(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   int64
		ok     bool
	}{
		{name: "not_read", config: nil},
		{name: "default", config: map[string]any{}, want: 1 << 20, ok: true},
		{name: "set", config: map[string]any{"maximum_lag_on_failover": float64(4096)}, want: 4096, ok: true},
		{name: "not_whole", config: map[string]any{"maximum_lag_on_failover": "4k"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MaximumLagOnFailover(tt.config)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
