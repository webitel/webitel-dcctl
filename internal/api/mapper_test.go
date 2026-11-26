package api

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func TestMapStatusExample(t *testing.T) {
	st := exampleStatus()
	m := newMapper(&st, observerAddress)

	got := m.status()
	require.NoError(t, m.err)

	assertExample(t, swaggerExample(t, "/v1/status", "get", "200"), gatewayJSON(t, got), statusExampleSkips())
}

func TestMapDatacenterExample(t *testing.T) {
	st := exampleStatus()
	dc := exampleDCB()
	dc.Nomad.JobCounts = status.JobCounts{Stopped: 2}

	m := newMapper(&st, observerAddress)
	got := m.datacenter(&dc, true)
	require.NoError(t, m.err)

	assertExample(t, swaggerExample(t, "/v1/datacenters/{datacenter}", "get", "200"), gatewayJSON(t, got),
		prefixed("", skipEndpoint))
}

func TestMapDatacenterSummary(t *testing.T) {
	st := exampleStatus()
	dc := exampleDCB()
	dc.Nomad.NotReadyNodes = []status.Node{{Name: "n1", Reason: "down"}}

	m := newMapper(&st, observerAddress)
	got := m.datacenter(&dc, false)
	require.NoError(t, m.err)

	assert.Empty(t, got.GetPatroni().GetMembers())
	assert.Empty(t, got.GetNomad().GetNotReadyNodes())
	assert.Empty(t, got.GetNomad().GetStages())
	assert.Empty(t, got.GetNomad().GetUnmanagedJobs())
	assert.Empty(t, got.GetInfoChecks())
	assert.Nil(t, got.GetObserver())
	assert.Nil(t, got.GetObserveTime())
	assert.EqualValues(t, 12, got.GetNomad().GetJobCounts().GetStoppedCount())
	assert.Equal(t, "pg-b1", got.GetPatroni().GetLeader())
}

func TestMapPatroniUnknownNumbers(t *testing.T) {
	st := exampleStatus()
	dc := exampleDCB()
	dc.Patroni.Timeline = nil
	dc.Patroni.LagBytes = nil
	dc.Patroni.Members[1].Timeline = nil
	dc.Patroni.Members[1].LagBytes = nil

	got := newMapper(&st, observerAddress).datacenter(&dc, true).GetPatroni()

	assert.Nil(t, got.Timeline, "an unknown timeline is not 0")
	assert.Nil(t, got.LagBytes, "an unknown lag is not 0")
	assert.Nil(t, got.GetMembers()[1].Timeline)
	assert.Nil(t, got.GetMembers()[1].LagBytes)

	known := exampleDCB()
	got = newMapper(&st, observerAddress).datacenter(&known, true).GetPatroni()

	require.NotNil(t, got.LagBytes, "a lag of 0 is known")
	assert.Zero(t, got.GetLagBytes())
	require.NotNil(t, got.GetMembers()[1].LagBytes)
	assert.Zero(t, got.GetMembers()[1].GetLagBytes())
}

func TestMapDivergence(t *testing.T) {
	tests := []struct {
		name      string
		profile   config.Profile
		dc        string
		active    string
		div       status.Divergence
		want      dcctlv1.Datacenter_Patroni_DivergenceState
		wantLSN   string
		wantBytes int64
	}{
		{
			name: "active", profile: config.ProfileWarmStandby, dc: "dc_a", active: "dc_a",
			div: status.Divergence{Known: true}, want: dcctlv1.Datacenter_Patroni_DIVERGENCE_STATE_UNSPECIFIED,
		},
		{
			name: "stretch", profile: config.ProfileStretch, dc: "dc_b", active: "dc_a",
			div: status.Divergence{Known: true}, want: dcctlv1.Datacenter_Patroni_DIVERGENCE_STATE_UNSPECIFIED,
		},
		{
			name: "unknown", profile: config.ProfileWarmStandby, dc: "dc_b", active: "dc_a",
			div: status.Divergence{}, want: dcctlv1.Datacenter_Patroni_UNKNOWN,
		},
		{
			name: "no_effective_state", profile: config.ProfileWarmStandby, dc: "dc_b",
			div: status.Divergence{}, want: dcctlv1.Datacenter_Patroni_UNKNOWN,
		},
		{
			name: "not_diverged", profile: config.ProfileWarmStandby, dc: "dc_b", active: "dc_a",
			div: status.Divergence{Known: true}, want: dcctlv1.Datacenter_Patroni_NOT_DIVERGED,
		},
		{
			name: "diverged", profile: config.ProfileWarmStandby, dc: "dc_b", active: "dc_a",
			div:  status.Divergence{Known: true, Diverged: true, LSN: 0x5000060, Bytes: 4096},
			want: dcctlv1.Datacenter_Patroni_DIVERGED, wantLSN: "0/5000060", wantBytes: 4096,
		},
		{
			name: "unknown_hides_numbers", profile: config.ProfileWarmStandby, dc: "dc_b", active: "dc_a",
			div:  status.Divergence{Diverged: true, LSN: 0x5000060, Bytes: 4096},
			want: dcctlv1.Datacenter_Patroni_UNKNOWN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := exampleStatus()
			st.Profile = tt.profile
			st.Effective = nil

			if tt.active != "" {
				st.Effective = exampleStatus().Effective
				st.Effective.ActiveDatacenter = tt.active
			}

			dc := status.Datacenter{Name: tt.dc, Patroni: status.Patroni{Divergence: tt.div}}

			got := newMapper(&st, observerAddress).datacenter(&dc, false).GetPatroni()

			assert.Equal(t, tt.want, got.GetDivergenceState())
			assert.Equal(t, tt.wantLSN, got.GetDivergenceLsn())
			assert.Equal(t, tt.wantBytes, got.GetDivergenceBytes())
		})
	}
}

func TestMapUndetermined(t *testing.T) {
	st := exampleStatus()
	st.Effective = nil
	st.Datacenters[0].State = status.StateUndetermined
	st.Datacenters[0].Health = status.HealthUnknown
	st.Datacenters[0].HealthReasons = []status.HealthReason{status.ReasonRoleUndetermined}

	m := newMapper(&st, observerAddress)
	got := m.status()
	require.NoError(t, m.err)

	assert.Empty(t, got.GetActiveDatacenter())
	assert.Zero(t, got.GetGeneration())
	assert.Nil(t, got.GetUpdateTime())
	assert.Equal(t, dcctlv1.Datacenter_UNDETERMINED, got.GetDatacenters()[0].GetState())
	assert.Equal(t, dcctlv1.Datacenter_UNKNOWN, got.GetDatacenters()[0].GetHealth())
	assert.Equal(t, []string{"role_undetermined"}, got.GetDatacenters()[0].GetHealthReasons())
}

func TestMapVoters(t *testing.T) {
	st := exampleStatus()
	st.Profile = config.ProfileStretch
	st.Voters = []status.Voter{
		{Name: coord.StretchName, Type: status.VoterDatacenter, Reason: source.ReasonNoLeader, Generation: 7},
		{Name: coord.ArbiterName, Type: status.VoterArbiter, Reachable: true, Initialized: true, Generation: 7, Valid: true},
	}

	m := newMapper(&st, observerAddress)
	got := m.status().GetVoters()
	require.NoError(t, m.err)
	require.Len(t, got, 2)

	assert.Equal(t, dcctlv1.Status_Voter_DATACENTER, got[0].GetType())
	assert.Empty(t, got[0].GetDatacenter())
	assert.False(t, got[0].GetReachable())
	assert.Equal(t, dcctlv1.Source_NO_LEADER, got[0].GetError())
	assert.Equal(t, dcctlv1.Status_Voter_ARBITER, got[1].GetType())
	assert.True(t, got[1].GetInitialized())
}

func TestMapSourceUnreachable(t *testing.T) {
	st := exampleStatus()
	since := observeTime.Add(-30 * time.Second)
	dc := status.Datacenter{Name: "dc_b", Sources: []status.Source{
		{System: status.SystemPatroni, Reachable: true, ObserveTime: observeTime},
		{System: status.SystemConsul, Reason: source.ReasonNoEndpoints, UnreachableSince: since},
	}}

	got := newMapper(&st, observerAddress).datacenter(&dc, false).GetSources()
	require.Len(t, got, 3, "every system, in the order Consul, Nomad, Patroni")

	assert.Equal(t, dcctlv1.Source_CONSUL, got[0].GetSystem())
	assert.False(t, got[0].GetReachable())
	assert.Equal(t, dcctlv1.Source_NO_ENDPOINTS, got[0].GetError())
	assert.Equal(t, since, got[0].GetUnreachableStartTime().AsTime())
	assert.Nil(t, got[0].GetObserveTime(), "a source never read has no observe_time")

	assert.Equal(t, dcctlv1.Source_NOMAD, got[1].GetSystem())
	assert.False(t, got[1].GetReachable(), "a system the model lacks is unreachable")

	assert.Equal(t, dcctlv1.Source_PATRONI, got[2].GetSystem())
	assert.True(t, got[2].GetReachable())
}

func TestMapCheck(t *testing.T) {
	st := exampleStatus()
	dc := status.Datacenter{Name: "dc_b", InfoChecks: []check.Check{{
		ID: "patroni.not_max_lsn", Result: check.Fail, Effect: check.Confirm,
		Confirmation: check.ConfirmNotMaxLSN, Datacenter: "dc_b", Detail: "dc_c has a larger LSN",
		Facts: map[string]any{"lag_bytes": 4096}, DiscardBytes: ptr[int64](4096),
	}}}

	m := newMapper(&st, observerAddress)
	got := m.datacenter(&dc, true).GetInfoChecks()
	require.NoError(t, m.err)
	require.Len(t, got, 1)

	assert.Equal(t, "patroni.not_max_lsn", got[0].GetId())
	assert.Equal(t, dcctlv1.Check_FAIL, got[0].GetResult())
	assert.Equal(t, dcctlv1.Check_CONFIRM, got[0].GetEffect())
	assert.Equal(t, "not_max_lsn", got[0].GetConfirmation())
	assert.Equal(t, "datacenters/dc_b", got[0].GetDatacenter())
	assert.Equal(t, "dc_c has a larger LSN", got[0].GetDetail())
	assert.InDelta(t, 4096, got[0].GetFacts().GetFields()["lag_bytes"].GetNumberValue(), 0)
}

func TestMapCheck_factsStructCannotHold(t *testing.T) {
	st := exampleStatus()
	dc := status.Datacenter{Name: "dc_b", InfoChecks: []check.Check{{
		ID: check.InfoAPI, Result: check.Pass, Facts: map[string]any{"ch": make(chan int)},
	}}}

	m := newMapper(&st, observerAddress)
	m.datacenter(&dc, true)

	require.Error(t, m.err)
	assert.Contains(t, m.err.Error(), "info.api")
}

func TestMapMissingNodes(t *testing.T) {
	st := exampleStatus()
	dc := exampleDCB()
	dc.Nomad.Stages[0].Jobs[0].MissingNodes = []jobstate.MissingNode{{Name: "n2", Reason: jobstate.NodeNoAllocation}}

	got := newMapper(&st, observerAddress).datacenter(&dc, true)

	nodes := got.GetNomad().GetStages()[0].GetJobs()[0].GetMissingNodes()
	require.Len(t, nodes, 1)
	assert.Equal(t, dcctlv1.Datacenter_Nomad_Node_NO_ALLOCATION, nodes[0].GetReason())
}

func TestMapperNarrowing(t *testing.T) {
	t.Run("int32_overflow", func(t *testing.T) {
		m := newMapper(&status.Status{}, "")

		assert.Zero(t, m.i32("ready_node_count", math.MaxInt32+1))
		require.Error(t, m.err)
		assert.Contains(t, m.err.Error(), "ready_node_count")
	})

	t.Run("int32_underflow", func(t *testing.T) {
		m := newMapper(&status.Status{}, "")

		assert.Zero(t, m.i32("stage", math.MinInt32-1))
		require.Error(t, m.err)
	})

	t.Run("int64_overflow", func(t *testing.T) {
		m := newMapper(&status.Status{}, "")

		assert.Zero(t, m.i64("modify_index", math.MaxUint64))
		require.Error(t, m.err)
	})

	t.Run("in_range", func(t *testing.T) {
		m := newMapper(&status.Status{}, "")

		assert.EqualValues(t, math.MaxInt32, m.i32("x", math.MaxInt32))
		assert.EqualValues(t, math.MaxInt64, m.i64("y", math.MaxInt64))
		assert.NoError(t, m.err)
	})

	t.Run("modify_index_in_datacenter", func(t *testing.T) {
		st := exampleStatus()
		dc := exampleDCB()
		dc.Nomad.Stages[0].Jobs[0].ModifyIndex = math.MaxUint64

		m := newMapper(&st, "")
		m.datacenter(&dc, true)

		assert.Error(t, m.err)
	})
}

func TestMapReplayTime(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
	}{
		{"2026-11-23 10:14:54.000000+00:00", time.Date(2026, 11, 23, 10, 14, 54, 0, time.UTC)},
		{"2026-11-23 12:14:54.5+02:00", time.Date(2026, 11, 23, 10, 14, 54, 500_000_000, time.UTC)},
	}

	for _, tt := range tests {
		got := replayTime(tt.in)
		require.NotNil(t, got, "replayTime(%q)", tt.in)
		assert.Equal(t, tt.want, got.AsTime(), "replayTime(%q)", tt.in)
	}
}

func TestMapReplayTimeUnparsed(t *testing.T) {
	for _, in := range []string{"", "yesterday"} {
		assert.Nil(t, replayTime(in), "replayTime(%q)", in)
	}
}
