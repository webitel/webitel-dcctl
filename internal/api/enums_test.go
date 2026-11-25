package api

import (
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// assertEnum checks every pair of an enum table, by the name of the input.
func assertEnum[In ~string, Out comparable](t *testing.T, conv func(In) Out, table map[In]Out) {
	t.Helper()

	for in, want := range table {
		assert.Equal(t, want, conv(in), "input %q", in)
	}
}

func TestProfileOf(t *testing.T) {
	assertEnum(t, profileOf, map[config.Profile]dcctlv1.Status_Profile{
		config.ProfileWarmStandby: dcctlv1.Status_WARM_STANDBY,
		config.ProfileStretch:     dcctlv1.Status_STRETCH,
		"":                        dcctlv1.Status_PROFILE_UNSPECIFIED,
		"other":                   dcctlv1.Status_PROFILE_UNSPECIFIED,
	})
}

func TestVoterTypeOf(t *testing.T) {
	assertEnum(t, voterTypeOf, map[status.VoterType]dcctlv1.Status_Voter_Type{
		status.VoterDatacenter: dcctlv1.Status_Voter_DATACENTER,
		status.VoterArbiter:    dcctlv1.Status_Voter_ARBITER,
		"":                     dcctlv1.Status_Voter_TYPE_UNSPECIFIED,
	})
}

func TestSourceErrorOf(t *testing.T) {
	assertEnum(t, sourceErrorOf, map[source.Reason]dcctlv1.Source_Error{
		"":                       dcctlv1.Source_ERROR_UNSPECIFIED,
		source.ReasonTimeout:     dcctlv1.Source_TIMEOUT,
		source.ReasonRefused:     dcctlv1.Source_REFUSED,
		source.ReasonDNS:         dcctlv1.Source_DNS,
		source.ReasonTLS:         dcctlv1.Source_TLS,
		source.ReasonAuth:        dcctlv1.Source_AUTH,
		source.ReasonHTTP:        dcctlv1.Source_HTTP,
		source.ReasonInvalid:     dcctlv1.Source_INVALID,
		source.ReasonNoEndpoints: dcctlv1.Source_NO_ENDPOINTS,
		source.ReasonNoLeader:    dcctlv1.Source_NO_LEADER,
		source.Reason("unheard"): dcctlv1.Source_ERROR_UNSPECIFIED,
	})
}

func TestQuorumModeOf(t *testing.T) {
	assertEnum(t, quorumModeOf, map[status.QuorumMode]dcctlv1.Status_Quorum_Mode{
		status.QuorumQuorum:     dcctlv1.Status_Quorum_QUORUM,
		status.QuorumForcedOnly: dcctlv1.Status_Quorum_FORCED_ONLY,
		"":                      dcctlv1.Status_Quorum_MODE_UNSPECIFIED,
	})
}

func TestStateOf(t *testing.T) {
	assertEnum(t, stateOf, map[status.State]dcctlv1.Datacenter_State{
		status.StateUnreachable:   dcctlv1.Datacenter_UNREACHABLE,
		status.StateUninitialized: dcctlv1.Datacenter_UNINITIALIZED,
		status.StateUndetermined:  dcctlv1.Datacenter_UNDETERMINED,
		status.StateActive:        dcctlv1.Datacenter_ACTIVE,
		status.StateSplitBrain:    dcctlv1.Datacenter_SPLIT_BRAIN,
		status.StateDBSplit:       dcctlv1.Datacenter_DB_SPLIT,
		status.StateStale:         dcctlv1.Datacenter_STALE,
		status.StateNeedsRecovery: dcctlv1.Datacenter_NEEDS_RECOVERY,
		status.StateStandby:       dcctlv1.Datacenter_STANDBY,
		"":                        dcctlv1.Datacenter_STATE_UNSPECIFIED,
	})
}

func TestHealthOf(t *testing.T) {
	assertEnum(t, healthOf, map[status.Health]dcctlv1.Datacenter_Health{
		status.HealthOK:       dcctlv1.Datacenter_OK,
		status.HealthDegraded: dcctlv1.Datacenter_DEGRADED,
		status.HealthDown:     dcctlv1.Datacenter_DOWN,
		status.HealthUnknown:  dcctlv1.Datacenter_UNKNOWN,
		"":                    dcctlv1.Datacenter_HEALTH_UNSPECIFIED,
	})
}

func TestSystemOf(t *testing.T) {
	assertEnum(t, systemOf, map[status.System]dcctlv1.Source_System{
		status.SystemConsul:  dcctlv1.Source_CONSUL,
		status.SystemNomad:   dcctlv1.Source_NOMAD,
		status.SystemPatroni: dcctlv1.Source_PATRONI,
		"":                   dcctlv1.Source_SYSTEM_UNSPECIFIED,
	})
}

func TestRoleOf(t *testing.T) {
	assertEnum(t, roleOf, map[status.Role]dcctlv1.Datacenter_Patroni_Role{
		status.RoleUnknown:       dcctlv1.Datacenter_Patroni_ROLE_UNSPECIFIED,
		status.RolePrimary:       dcctlv1.Datacenter_Patroni_PRIMARY,
		status.RoleStandbyLeader: dcctlv1.Datacenter_Patroni_STANDBY_LEADER,
		status.RoleReplica:       dcctlv1.Datacenter_Patroni_REPLICA,
		status.RoleSyncStandby:   dcctlv1.Datacenter_Patroni_SYNC_STANDBY,
		status.RoleQuorumStandby: dcctlv1.Datacenter_Patroni_QUORUM_STANDBY,
	})
}

func TestUnusableOf(t *testing.T) {
	assertEnum(t, unusableOf, map[patroni.UnusableReason]dcctlv1.Datacenter_Patroni_UnusableReason{
		"":                          dcctlv1.Datacenter_Patroni_UNUSABLE_REASON_UNSPECIFIED,
		patroni.UnusableVersion:     dcctlv1.Datacenter_Patroni_VERSION,
		patroni.UnusableTLS:         dcctlv1.Datacenter_Patroni_TLS,
		patroni.UnusableUnreachable: dcctlv1.Datacenter_Patroni_UNREACHABLE,
	})
}

func TestNodeReasonOf(t *testing.T) {
	assertEnum(t, nodeReasonOf, map[jobstate.NodeReason]dcctlv1.Datacenter_Nomad_Node_Reason{
		jobstate.NodeDraining:     dcctlv1.Datacenter_Nomad_Node_DRAINING,
		jobstate.NodeDown:         dcctlv1.Datacenter_Nomad_Node_DOWN,
		jobstate.NodeIneligible:   dcctlv1.Datacenter_Nomad_Node_INELIGIBLE,
		jobstate.NodeNoAllocation: dcctlv1.Datacenter_Nomad_Node_NO_ALLOCATION,
		"":                        dcctlv1.Datacenter_Nomad_Node_REASON_UNSPECIFIED,
	})
}

func TestJobStateOf(t *testing.T) {
	assertEnum(t, jobStateOf, map[status.JobState]dcctlv1.Datacenter_Nomad_Job_State{
		status.JobStopped:   dcctlv1.Datacenter_Nomad_Job_STOPPED,
		status.JobStopping:  dcctlv1.Datacenter_Nomad_Job_STOPPING,
		status.JobHealthy:   dcctlv1.Datacenter_Nomad_Job_HEALTHY,
		status.JobUnhealthy: dcctlv1.Datacenter_Nomad_Job_UNHEALTHY,
		"":                  dcctlv1.Datacenter_Nomad_Job_STATE_UNSPECIFIED,
	})
}

func TestJobTypeOf(t *testing.T) {
	assertEnum(t, jobTypeOf, map[string]dcctlv1.Datacenter_Nomad_Job_Type{
		nomadapi.JobTypeService:  dcctlv1.Datacenter_Nomad_Job_SERVICE,
		nomadapi.JobTypeSystem:   dcctlv1.Datacenter_Nomad_Job_SYSTEM,
		nomadapi.JobTypeBatch:    dcctlv1.Datacenter_Nomad_Job_BATCH,
		nomadapi.JobTypeSysbatch: dcctlv1.Datacenter_Nomad_Job_SYSBATCH,
		"":                       dcctlv1.Datacenter_Nomad_Job_TYPE_UNSPECIFIED,
	})
}

func TestCheckResultOf(t *testing.T) {
	assertEnum(t, checkResultOf, map[check.Result]dcctlv1.Check_Result{
		check.Pass: dcctlv1.Check_PASS,
		check.Fail: dcctlv1.Check_FAIL,
		check.Warn: dcctlv1.Check_WARN,
		check.Skip: dcctlv1.Check_SKIP,
		"":         dcctlv1.Check_RESULT_UNSPECIFIED,
		"other":    dcctlv1.Check_RESULT_UNSPECIFIED,
	})
}

func TestCheckEffectOf(t *testing.T) {
	assertEnum(t, checkEffectOf, map[check.Effect]dcctlv1.Check_Effect{
		check.EffectNone: dcctlv1.Check_EFFECT_UNSPECIFIED,
		check.Block:      dcctlv1.Check_BLOCK,
		check.WarnOnly:   dcctlv1.Check_WARN_ONLY,
		check.Confirm:    dcctlv1.Check_CONFIRM,
		"other":          dcctlv1.Check_EFFECT_UNSPECIFIED,
	})
}

func TestFormatLSN(t *testing.T) {
	tests := []struct {
		lsn  int64
		want string
	}{
		{0, "0/0"},
		{0x5000060, "0/5000060"},
		{0x16B374D848, "16/B374D848"},
		{-1, ""},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, formatLSN(tt.lsn), "formatLSN(%d)", tt.lsn)
	}
}

func TestResourceName(t *testing.T) {
	assert.Equal(t, "datacenters/dc_a", datacenterName("dc_a"))
	assert.Empty(t, datacenterName(""))
}
