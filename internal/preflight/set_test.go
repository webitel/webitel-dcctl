package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// item is a check of a set: its id, datacenter and effect.
type item struct {
	id     check.ID
	dc     string
	effect check.Effect
}

func itemsOf(cs []check.Check) []item {
	out := make([]item, len(cs))
	for i, c := range cs {
		out[i] = item{c.ID, c.Datacenter, c.Effect}
	}

	return out
}

const (
	b = check.Block
	w = check.WarnOnly
	c = check.Confirm
	n = check.EffectNone
)

func common() []item {
	return []item{
		{check.CoordStatePresent, "", b},
		{check.CoordNoOperation, "", b},
		{check.CoordLockFree, "", b},
		{check.CoordLeaderReachable, "", b},
		{check.CoordNoConflict, "", b},
		{check.CoordNoUncommitted, "", b},
		{check.CoordNoSplit, "", b},
	}
}

func basicItems(dc string) []item {
	return []item{
		{check.PatroniMembersFound, dc, b},
		{check.PatroniReachable, dc, b},
		{check.PatroniVersion, dc, b},
		{check.PatroniNotPaused, dc, b},
		{check.PatroniHasLeader, dc, b},
	}
}

func appsItems() []item {
	return []item{
		{check.NomadReachable, dcA, b},
		{check.NomadReachable, dcB, b},
		{check.NomadStagesValid, dcA, b},
		{check.NomadStagesValid, dcB, b},
		{check.NomadTargetJobsPresent, dcB, b},
		{check.NomadTargetJobsStopped, dcB, b},
		{check.NomadTargetPlan, dcB, b},
		{check.NomadTargetNodes, dcB, w},
	}
}

func cat(parts ...[]item) []item {
	var out []item
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

func TestChecks_sets(t *testing.T) {
	tests := []struct {
		name   string
		snap   func() *collect.Snapshot
		action Action
		params Params
		want   []item
	}{
		{
			name: "switch_warm_standby", snap: warm, action: Switch, params: Params{Datacenter: dcB},
			want: cat(common(),
				[]item{
					{check.CoordQuorum, "", b},
					{check.CoordPayloadSettled, "", b},
					{check.CoordTargetValid, dcB, b},
					{check.CoordTargetNotNeedsRecovery, dcB, b},
				},
				basicItems(dcA), basicItems(dcB),
				[]item{
					{check.PatroniSinglePrimary, "", b},
					{check.PatroniSourcePrimary, dcA, b},
					{check.PatroniTargetStandbyLeader, dcB, b},
					{check.PatroniTargetFollowsSource, dcB, b},
					{check.PatroniTargetLag, dcB, b},
					{check.PatroniOthersFollowSource, "", w},
				},
				appsItems(),
				[]item{{check.InfraTargetRabbitMQ, dcB, b}}),
		},
		{
			name: "switch_stretch", snap: stretch, action: Switch, params: Params{Datacenter: dcB},
			want: cat(common(),
				[]item{{check.CoordQuorum, "", b}, {check.CoordPayloadSettled, "", b}, {check.CoordTargetValid, dcB, b}},
				basicItems(""),
				[]item{{check.PatroniTargetCandidate, dcB, b}},
				appsItems(),
				[]item{{check.InfraTargetRabbitMQ, dcB, b}}),
		},
		{
			name: "failover_warm_standby", snap: warm, action: Failover, params: Params{Datacenter: dcB},
			want: cat(common(),
				[]item{
					{check.CoordQuorum, "", w},
					{check.CoordPayloadSettled, "", b},
					{check.CoordIsolation, "", c},
					{check.CoordTargetValid, dcB, b},
					{check.CoordTargetNotNeedsRecovery, dcB, b},
				},
				basicItems(dcB),
				[]item{
					{check.PatroniTargetStandbyLeader, dcB, b},
					{check.PatroniTargetNotStreaming, dcB, b},
					{check.PatroniTargetMaxLSN, dcB, c},
					{check.PatroniSourceDiscard, dcA, c},
					{check.NomadReachable, dcB, b},
					{check.NomadStagesValid, dcB, b},
					{check.NomadTargetPlan, dcB, b},
					{check.NomadTargetNodes, dcB, w},
					{check.InfraTargetRabbitMQ, dcB, w},
				}),
		},
		{
			name: "failover_stretch", snap: stretch, action: Failover, params: Params{Datacenter: dcB},
			want: cat(common(), []item{
				{check.CoordQuorum, "", b},
				{check.CoordPayloadSettled, "", b},
				{check.CoordIsolation, "", c},
				{check.CoordTargetValid, dcB, b},
				{check.PatroniMembersFound, dcB, b},
				{check.PatroniTargetLeader, dcB, b},
				{check.PatroniCandidateLoss, dcB, c},
				{check.NomadReachable, dcB, b},
				{check.NomadStagesValid, dcB, b},
				{check.NomadTargetPlan, dcB, b},
				{check.NomadTargetNodes, dcB, w},
				{check.InfraTargetRabbitMQ, dcB, w},
			}),
		},
		{
			name: "demote", snap: warm, action: Demote, params: Params{Datacenter: dcB},
			want: cat([]item{{check.CoordProfile, "", b}}, common(), []item{
				{check.CoordQuorum, "", b}, {check.CoordPayloadSettled, "", b}, {check.CoordDCNotActive, dcB, b},
			}, basicItems(dcB), basicItems(dcA), []item{
				{check.PatroniActivePrimary, dcA, b},
				{check.PatroniDCPrimary, dcB, b},
				{check.PatroniDivergence, dcB, c},
				{check.NomadDCAppsStopped, dcB, w},
			}),
		},
		{
			name: "repoint", snap: warm, action: Repoint, params: Params{Datacenter: dcB},
			want: cat([]item{{check.CoordProfile, "", b}}, common(), []item{{check.CoordDCNotActive, dcB, b}}, basicItems(dcB), basicItems(dcA), []item{
				{check.PatroniActivePrimary, dcA, b}, {check.PatroniDCStandby, dcB, b}, {check.PatroniDivergence, dcB, c},
			}),
		},
		{
			name: "mark_recovered", snap: warm, action: MarkRecovered, params: Params{Datacenter: dcB},
			want: cat([]item{{check.CoordProfile, "", b}}, common(), []item{
				{check.CoordQuorum, "", b},
				{check.CoordPayloadSettled, "", b},
				{check.CoordDCNeedsRecovery, dcB, b},
				{check.PatroniDCRecovered, dcB, b},
			}),
		},
		{
			name: "resume", snap: warm, action: Resume, params: Params{Operation: "op-2"},
			want: []item{
				{check.OpInterrupted, "", b},
				{check.OpStepsKnown, "", b},
				{check.OpNotRolledBack, "", b},
				{check.CoordLockFree, "", b},
				{check.CoordLeaderReachable, "", b},
				{check.CoordNoConflict, "", b},
				{check.CoordQuorum, "", w},
			},
		},
		{
			name: "resume_to_forced", snap: warm, action: Resume, params: Params{Operation: "op-2", ToForced: true},
			want: []item{
				{check.OpInterrupted, "", b},
				{check.OpStepsKnown, "", b},
				{check.OpNotRolledBack, "", b},
				{check.CoordLockFree, "", b},
				{check.CoordLeaderReachable, "", b},
				{check.CoordNoConflict, "", b},
				{check.CoordQuorum, "", w},
				{check.CoordIsolation, "", c},
			},
		},
		{
			name: "abandon", snap: warm, action: Abandon, params: Params{Operation: "op-2"},
			want: []item{{check.OpInterrupted, "", b}, {check.OpGenerationDetermined, "", c}, {check.OpPointOfNoReturn, "", w}},
		},
		{
			name: "resolve", snap: warm, action: Resolve, params: Params{Datacenter: dcB},
			want: []item{
				{check.ResolveConflict, "", b},
				{check.ResolveWinner, dcB, b},
				{check.CoordNoOperation, "", b},
				{check.CoordLockFree, "", b},
				{check.CoordLeaderReachable, "", b},
				{check.CoordQuorum, "", w},
				{check.CoordIsolation, "", c},
			},
		},
		{
			name: "init", snap: warm, action: Init, params: Params{Datacenter: dcA},
			want: []item{
				{check.InitStateAbsent, "", b},
				{check.InitAllReachable, "", b},
				{check.InitActiveValid, dcA, b},
				{check.InitActiveHasPrimary, dcA, b},
			},
		},
		{
			name: "arbiter_init", snap: warm, action: ArbiterInit,
			want: []item{
				{check.CoordNoOperation, "", b},
				{check.InitAllReachable, "", b},
				{check.ArbiterReachable, "", b},
				{check.ArbiterEmpty, "", b},
				{check.ArbiterStatesEqual, "", b},
			},
		},
		{
			name: "apps_sync", snap: warm, action: AppsSync,
			want: cat(common(), []item{
				{check.PatroniActivePrimary, dcA, b},
				{check.NomadReachable, dcA, b},
				{check.NomadStagesValid, dcA, b},
				{check.NomadActivePlan, dcA, b},
			}),
		},
		{
			name: "verify", snap: warm, action: Verify,
			want: []item{
				{check.VerifyPatroni, dcA, n},
				{check.VerifyPatroni, dcB, n},
				{check.VerifyJobs, dcA, n},
				{check.VerifyJobs, dcB, n},
				{check.VerifyState, "", n},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs, err := Checks(newInput(tt.snap(), tt.action, tt.params))
			require.NoError(t, err)

			assert.Equal(t, tt.want, itemsOf(cs))
			requireStruct(t, cs...)
		})
	}
}

func TestChecks_unknownAction(t *testing.T) {
	_, err := Checks(newInput(warm(), "rejoin", Params{}))
	require.ErrorIs(t, err, ErrUnknownAction)
}

// verdict decides the checks of the action with the given confirmations.
func verdict(t *testing.T, in *Input, given check.Confirmations) check.Verdict {
	t.Helper()

	cs, err := Checks(in)
	require.NoError(t, err)

	return check.Decide(cs, given)
}

func TestChecks_healthyInstallationAllowsSwitch(t *testing.T) {
	v := verdict(t, newInput(warm(), Switch, Params{Datacenter: dcB}), check.Confirmations{})
	assert.Equal(t, check.Allowed, v.Outcome, "failed: %v", v.Failed)

	v = verdict(t, newInput(stretch(), Switch, Params{Datacenter: dcB}), check.Confirmations{})
	assert.Equal(t, check.Allowed, v.Outcome, "failed: %v", v.Failed)
}

func TestChecks_invalidStageBlocksSwitch(t *testing.T) {
	for _, dc := range []string{dcA, dcB} {
		t.Run(dc, func(t *testing.T) {
			s := warm()
			invalidStage(s, dc)

			v := verdict(t, newInput(s, Switch, Params{Datacenter: dcB}), check.Confirmations{})
			require.Equal(t, check.Blocked, v.Outcome)
			assert.Equal(t, []check.Check{find(t, v.Checks, check.NomadStagesValid, dc)}, v.Failed)
		})
	}
}

// isolatedA is warm after dc_a went away: the standby of dc_b stopped streaming.
func isolatedA() *collect.Snapshot {
	s := warm()
	cut(s, dcA)
	editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) { p.ReplicationState = "stopped" })

	return s
}

func TestChecks_failoverNeedsIsolation(t *testing.T) {
	in := newInput(isolatedA(), Failover, Params{Datacenter: dcB})

	v := verdict(t, in, check.Confirmations{})
	require.Equal(t, check.NeedsConfirmation, v.Outcome, "failed: %v", v.Failed)
	assert.Equal(t, []check.Check{find(t, v.Checks, check.CoordIsolation, "")}, v.Unconfirmed)

	v = verdict(t, in, check.Confirmations{Isolation: true, MaxDiscardBytes: 10})
	require.Equal(t, check.Allowed, v.Outcome)
	assert.Equal(t, check.Confirmations{Isolation: true}, v.Confirmed)
	assert.Empty(t, v.Warnings, "the old active datacenter is not one that may be split")
}

func TestChecks_partialAdvance(t *testing.T) {
	s := isolatedA()
	s.Voters[0].KV = down[collect.KV]()
	s.Voters[1] = kvVoter(dcB, state(6, dcB))

	for _, a := range []Action{Switch, Demote, Repoint, MarkRecovered, AppsSync} {
		t.Run(string(a), func(t *testing.T) {
			v := verdict(t, newInput(s, a, Params{Datacenter: dcB}), check.Confirmations{Isolation: true})
			assert.Equal(t, check.Blocked, v.Outcome)
			assert.Contains(t, v.Failed, find(t, v.Checks, check.CoordNoUncommitted, ""))
		})
	}
}

// A failover proceeds from the highest record that unreachable voters may hold: none of
// the coordination checks blocks it.
func TestChecks_partialAdvanceAllowsFailover(t *testing.T) {
	s := warm()
	partial(s)
	s.Observer = collect.Instance{Datacenter: dcA, Node: "dcctl-" + dcA}

	// Generation 6 makes dc_b active: dc_a stopped its apps and was demoted before dc_b
	// went away.
	d, _ := s.Datacenter(dcA)
	d.Nomad = up(stoppedNomad())

	editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-a1").Role = patroni.ClusterRoleStandbyLeader })

	v := verdict(t, newInput(s, Failover, Params{Datacenter: dcA}), check.Confirmations{Isolation: true})

	for _, f := range v.Failed {
		assert.NotContains(t, string(f.ID), "coord.", "%s: %s", f.ID, f.Detail)
	}

	assert.Equal(t, check.Pass, find(t, v.Checks, check.CoordNoUncommitted, "").Result)
	assert.Equal(t, check.Pass, find(t, v.Checks, check.CoordTargetValid, dcA).Result)
}
