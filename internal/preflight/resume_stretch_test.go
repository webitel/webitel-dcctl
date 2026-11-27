package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
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

func TestFailoverLoss(t *testing.T) {
	members := func(fn func(p *collect.Patroni)) []status.Member {
		s := stretch()
		noLeader(s)
		editPatroni(s, dcA, fn)

		return newInput(s, Failover, Params{Datacenter: dcB}).pgCluster().members
	}

	tests := []struct {
		name       string
		edit       func(p *collect.Patroni)
		cand       string
		loss       int64
		notCounted []string
		err        error
	}{
		{name: "lag", edit: func(p *collect.Patroni) { memberNamed(p, "pg-3").Lag = i64(300) }, cand: "pg-3", loss: 300},
		{name: "survivor_ahead", edit: func(*collect.Patroni) {}, cand: "pg-3", loss: 10},
		{
			// Beyond maximum_lag_on_failover a member is still a candidate.
			name: "over_the_lag_limit", edit: func(p *collect.Patroni) {
				memberNamed(p, "pg-3").Lag = i64(1 << 30)
				memberNamed(p, "pg-4").Lag = i64(1 << 30)
			},
			cand: "pg-3", loss: 1 << 30,
		},
		{
			// pg-3 stayed on timeline 2: it is no candidate, pg-4 on timeline 3 is.
			name: "candidate_on_old_timeline", edit: func(p *collect.Patroni) {
				memberNamed(p, "pg-3").Probe.Timeline = i64(2)
			},
			cand: "pg-4", loss: 30,
		},
		{
			// What pg-2 holds on timeline 2 is off the history of timeline 3: the loss is the lag of pg-3.
			name: "survivor_on_lower_timeline", edit: func(p *collect.Patroni) {
				memberNamed(p, "pg-2").Probe.Timeline = i64(2)
				memberNamed(p, "pg-2").Probe.Xlog.ReceivedLocation = i64(50000)
			},
			cand: "pg-3", loss: 10,
		},
		{
			name: "timeline_unknown", edit: func(p *collect.Patroni) {
				memberNamed(p, "pg-2").Probe.Timeline = nil
				memberNamed(p, "pg-2").Timeline = nil
			},
			cand: "pg-3", err: ErrLossUnknown,
		},
		{
			// pg-2 answers without a WAL position: it is not counted, as one that does
			// not answer.
			name: "position_unknown", edit: func(p *collect.Patroni) { memberNamed(p, "pg-2").Probe.Xlog = patroni.Xlog{} },
			cand: "pg-3", loss: 10, notCounted: []string{"pg-2"},
		},
		{
			name: "candidate_position_unknown", edit: func(p *collect.Patroni) {
				memberNamed(p, "pg-3").Probe.Xlog = patroni.Xlog{}
				memberNamed(p, "pg-4").Probe.Xlog = patroni.Xlog{}
			},
			cand: "pg-4", notCounted: []string{"pg-3", "pg-4"}, err: ErrLossUnknown,
		},
		{
			name: "no_candidate", edit: func(p *collect.Patroni) {
				memberNamed(p, "pg-3").NoFailover = true
				memberNamed(p, "pg-4").FailoverPriority = 0
			},
			err: ErrNoCandidate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := FailoverLoss(members(tt.edit), dcB)
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, tt.cand, l.Candidate.Name)
			assert.Equal(t, tt.loss, l.Bytes)
			assert.Equal(t, tt.notCounted, l.NotCounted)
		})
	}
}

// A member that answers without a WAL position is named, not counted.
func TestCandidateLossNamesWhatIsNotCounted(t *testing.T) {
	s := stretch()
	noLeader(s)
	editPatroni(s, dcA, func(p *collect.Patroni) {
		memberNamed(p, "pg-2").Probe.Xlog = patroni.Xlog{}
		memberNamed(p, "pg-3").Lag = i64(300)
	})

	c := newInput(s, Failover, Params{Datacenter: dcB}).candidateLoss()
	assert.Equal(t, check.Fail, c.Result)
	require.NotNil(t, c.DiscardBytes)
	assert.Equal(t, int64(300), *c.DiscardBytes)
	assert.Equal(t, []string{"pg-2"}, c.Facts["not_counted"])
	assert.Contains(t, c.Detail, "(no WAL position: pg-2)")
}
