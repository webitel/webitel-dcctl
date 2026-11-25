package api

import (
	"fmt"

	nomadapi "github.com/hashicorp/nomad/api"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// A value of the model these functions do not know maps to UNSPECIFIED: a
// newer model value is then shown as "not set" rather than as another value.

func profileOf(p config.Profile) dcctlv1.Status_Profile {
	switch p {
	case config.ProfileWarmStandby:
		return dcctlv1.Status_WARM_STANDBY
	case config.ProfileStretch:
		return dcctlv1.Status_STRETCH
	}

	return dcctlv1.Status_PROFILE_UNSPECIFIED
}

func voterTypeOf(t status.VoterType) dcctlv1.Status_Voter_Type {
	switch t {
	case status.VoterDatacenter:
		return dcctlv1.Status_Voter_DATACENTER
	case status.VoterArbiter:
		return dcctlv1.Status_Voter_ARBITER
	}

	return dcctlv1.Status_Voter_TYPE_UNSPECIFIED
}

func sourceErrorOf(r source.Reason) dcctlv1.Source_Error {
	switch r {
	case source.ReasonTimeout:
		return dcctlv1.Source_TIMEOUT
	case source.ReasonRefused:
		return dcctlv1.Source_REFUSED
	case source.ReasonDNS:
		return dcctlv1.Source_DNS
	case source.ReasonTLS:
		return dcctlv1.Source_TLS
	case source.ReasonAuth:
		return dcctlv1.Source_AUTH
	case source.ReasonHTTP:
		return dcctlv1.Source_HTTP
	case source.ReasonInvalid:
		return dcctlv1.Source_INVALID
	case source.ReasonNoEndpoints:
		return dcctlv1.Source_NO_ENDPOINTS
	case source.ReasonNoLeader:
		return dcctlv1.Source_NO_LEADER
	}

	return dcctlv1.Source_ERROR_UNSPECIFIED
}

func quorumModeOf(m status.QuorumMode) dcctlv1.Status_Quorum_Mode {
	switch m {
	case status.QuorumQuorum:
		return dcctlv1.Status_Quorum_QUORUM
	case status.QuorumForcedOnly:
		return dcctlv1.Status_Quorum_FORCED_ONLY
	}

	return dcctlv1.Status_Quorum_MODE_UNSPECIFIED
}

func stateOf(s status.State) dcctlv1.Datacenter_State {
	switch s {
	case status.StateUnreachable:
		return dcctlv1.Datacenter_UNREACHABLE
	case status.StateUninitialized:
		return dcctlv1.Datacenter_UNINITIALIZED
	case status.StateUndetermined:
		return dcctlv1.Datacenter_UNDETERMINED
	case status.StateActive:
		return dcctlv1.Datacenter_ACTIVE
	case status.StateSplitBrain:
		return dcctlv1.Datacenter_SPLIT_BRAIN
	case status.StateDBSplit:
		return dcctlv1.Datacenter_DB_SPLIT
	case status.StateStale:
		return dcctlv1.Datacenter_STALE
	case status.StateNeedsRecovery:
		return dcctlv1.Datacenter_NEEDS_RECOVERY
	case status.StateStandby:
		return dcctlv1.Datacenter_STANDBY
	}

	return dcctlv1.Datacenter_STATE_UNSPECIFIED
}

func healthOf(h status.Health) dcctlv1.Datacenter_Health {
	switch h {
	case status.HealthOK:
		return dcctlv1.Datacenter_OK
	case status.HealthDegraded:
		return dcctlv1.Datacenter_DEGRADED
	case status.HealthDown:
		return dcctlv1.Datacenter_DOWN
	case status.HealthUnknown:
		return dcctlv1.Datacenter_UNKNOWN
	}

	return dcctlv1.Datacenter_HEALTH_UNSPECIFIED
}

func systemOf(s status.System) dcctlv1.Source_System {
	switch s {
	case status.SystemConsul:
		return dcctlv1.Source_CONSUL
	case status.SystemNomad:
		return dcctlv1.Source_NOMAD
	case status.SystemPatroni:
		return dcctlv1.Source_PATRONI
	}

	return dcctlv1.Source_SYSTEM_UNSPECIFIED
}

func roleOf(r status.Role) dcctlv1.Datacenter_Patroni_Role {
	switch r {
	case status.RolePrimary:
		return dcctlv1.Datacenter_Patroni_PRIMARY
	case status.RoleStandbyLeader:
		return dcctlv1.Datacenter_Patroni_STANDBY_LEADER
	case status.RoleReplica:
		return dcctlv1.Datacenter_Patroni_REPLICA
	case status.RoleSyncStandby:
		return dcctlv1.Datacenter_Patroni_SYNC_STANDBY
	case status.RoleQuorumStandby:
		return dcctlv1.Datacenter_Patroni_QUORUM_STANDBY
	case status.RoleUnknown:
	}

	return dcctlv1.Datacenter_Patroni_ROLE_UNSPECIFIED
}

func unusableOf(r patroni.UnusableReason) dcctlv1.Datacenter_Patroni_UnusableReason {
	switch r {
	case patroni.UnusableVersion:
		return dcctlv1.Datacenter_Patroni_VERSION
	case patroni.UnusableTLS:
		return dcctlv1.Datacenter_Patroni_TLS
	case patroni.UnusableUnreachable:
		return dcctlv1.Datacenter_Patroni_UNREACHABLE
	}

	return dcctlv1.Datacenter_Patroni_UNUSABLE_REASON_UNSPECIFIED
}

func nodeReasonOf(r jobstate.NodeReason) dcctlv1.Datacenter_Nomad_Node_Reason {
	switch r {
	case jobstate.NodeDraining:
		return dcctlv1.Datacenter_Nomad_Node_DRAINING
	case jobstate.NodeDown:
		return dcctlv1.Datacenter_Nomad_Node_DOWN
	case jobstate.NodeIneligible:
		return dcctlv1.Datacenter_Nomad_Node_INELIGIBLE
	case jobstate.NodeNoAllocation:
		return dcctlv1.Datacenter_Nomad_Node_NO_ALLOCATION
	}

	return dcctlv1.Datacenter_Nomad_Node_REASON_UNSPECIFIED
}

func jobStateOf(s status.JobState) dcctlv1.Datacenter_Nomad_Job_State {
	switch s {
	case status.JobStopped:
		return dcctlv1.Datacenter_Nomad_Job_STOPPED
	case status.JobStopping:
		return dcctlv1.Datacenter_Nomad_Job_STOPPING
	case status.JobHealthy:
		return dcctlv1.Datacenter_Nomad_Job_HEALTHY
	case status.JobUnhealthy:
		return dcctlv1.Datacenter_Nomad_Job_UNHEALTHY
	}

	return dcctlv1.Datacenter_Nomad_Job_STATE_UNSPECIFIED
}

// jobTypeOf maps the Nomad job type as Nomad reports it.
func jobTypeOf(t string) dcctlv1.Datacenter_Nomad_Job_Type {
	switch t {
	case nomadapi.JobTypeService:
		return dcctlv1.Datacenter_Nomad_Job_SERVICE
	case nomadapi.JobTypeSystem:
		return dcctlv1.Datacenter_Nomad_Job_SYSTEM
	case nomadapi.JobTypeBatch:
		return dcctlv1.Datacenter_Nomad_Job_BATCH
	case nomadapi.JobTypeSysbatch:
		return dcctlv1.Datacenter_Nomad_Job_SYSBATCH
	}

	return dcctlv1.Datacenter_Nomad_Job_TYPE_UNSPECIFIED
}

// formatLSN writes a WAL position the way PostgreSQL shows pg_lsn, like
// `0/5000060`; "" for a negative position, which no LSN is.
func formatLSN(lsn int64) string {
	if lsn < 0 {
		return ""
	}

	return fmt.Sprintf("%X/%X", lsn>>32, lsn&0xFFFFFFFF)
}

// datacenterName returns the resource name of datacenter dc, or "" for no
// datacenter.
func datacenterName(dc string) string {
	if dc == "" {
		return ""
	}

	return "datacenters/" + dc
}
