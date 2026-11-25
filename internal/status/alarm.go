package status

import (
	"time"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// AlarmID identifies an alarm type.
type AlarmID string

// The alarms, in the order of webitel.dcctl.v1.Status.Alarm.Type.
const (
	// AlarmConflict: two valid records of one generation name different active
	// datacenters.
	AlarmConflict AlarmID = "conflict"
	// AlarmUncommitted: a voter holds a record above the effective generation that is
	// neither forced nor on a majority, even counting the voters that do not answer.
	AlarmUncommitted AlarmID = "uncommitted"
	// AlarmStale: the KV voter of a datacenter stays behind the effective generation
	// or without dcctl/state.
	AlarmStale AlarmID = "stale"
	// AlarmSplitBrain: an inactive datacenter runs a managed job. Without a known
	// active datacenter it is raised once for the installation, with no datacenter,
	// when managed jobs run in more than one datacenter.
	AlarmSplitBrain AlarmID = "split_brain"
	// AlarmDBSplit: warm_standby, the Patroni leader of an inactive datacenter is a
	// primary.
	AlarmDBSplit AlarmID = "db_split"
	// AlarmMultiplePrimaries: more than one Patroni member reports itself primary, or
	// the Consul of a cluster has more than one passing primary instance.
	AlarmMultiplePrimaries AlarmID = "multiple_primaries"
	// AlarmDBPrimaryMissing: the active datacenter has no primary in warm_standby, the
	// cluster has no leader in stretch.
	AlarmDBPrimaryMissing AlarmID = "db_primary_missing"
	// AlarmDatacenterUnreachable: no source of a datacenter answers. Critical for the
	// active datacenter.
	AlarmDatacenterUnreachable AlarmID = "datacenter_unreachable"
	// AlarmDBLeaderMismatch: stretch, the Patroni leader runs outside the active
	// datacenter.
	AlarmDBLeaderMismatch AlarmID = "db_leader_mismatch"
	// AlarmLeaseLost: the lease of the active datacenter is not renewed on a majority.
	AlarmLeaseLost AlarmID = "lease_lost"
	// AlarmAppsDegraded: a managed job of the active datacenter is not healthy.
	AlarmAppsDegraded AlarmID = "apps_degraded"
	// AlarmDBDegraded: a Patroni member of a datacenter does not answer, is unusable
	// or not running, or Patroni is paused.
	AlarmDBDegraded AlarmID = "db_degraded"
	// AlarmStandbyNotFollowing: warm_standby, the leader of an inactive datacenter
	// does not stream from the active datacenter on its timeline.
	AlarmStandbyNotFollowing AlarmID = "standby_not_following"
	// AlarmVoterUnreachable: a voter does not answer, or answers from a Consul without
	// a leader.
	AlarmVoterUnreachable AlarmID = "voter_unreachable"
	// AlarmQuorumLost: the voters that answer are fewer than a majority.
	AlarmQuorumLost AlarmID = "quorum_lost"
	// AlarmArbiterUninitialized: the arbiter answers without the state.
	AlarmArbiterUninitialized AlarmID = "arbiter_uninitialized"
	// AlarmOperationInterrupted: the current operation is interrupted.
	AlarmOperationInterrupted AlarmID = "operation_interrupted"
)

// AlarmIDs returns every alarm, in the order of the proto enum.
func AlarmIDs() []AlarmID {
	return []AlarmID{
		AlarmConflict, AlarmUncommitted, AlarmStale, AlarmSplitBrain, AlarmDBSplit, AlarmMultiplePrimaries,
		AlarmDBPrimaryMissing, AlarmDatacenterUnreachable, AlarmDBLeaderMismatch, AlarmLeaseLost, AlarmAppsDegraded,
		AlarmDBDegraded, AlarmStandbyNotFollowing, AlarmVoterUnreachable, AlarmQuorumLost, AlarmArbiterUninitialized,
		AlarmOperationInterrupted,
	}
}

// Type returns the proto enum of the alarm, TYPE_UNSPECIFIED for an unknown one.
func (id AlarmID) Type() dcctlv1.Status_Alarm_Type {
	return alarmDefs[id].typ
}

// Scope returns what the alarm is raised for.
func (id AlarmID) Scope() AlarmScope {
	return alarmDefs[id].scope
}

// AlarmScope is what an alarm is raised for: the installation, each datacenter, each
// voter, or the arbiter alone.
type AlarmScope string

const (
	AlarmScopeInstallation AlarmScope = "installation"
	// AlarmScopeDatacenter alarms have their datacenter. split_brain is also raised for
	// the installation while the active datacenter is not known, and stale has no
	// datacenter in stretch, whose one voter is the Consul of every datacenter.
	AlarmScopeDatacenter AlarmScope = "datacenter"
	AlarmScopeVoter      AlarmScope = "voter"
	AlarmScopeArbiter    AlarmScope = "arbiter"
)

// Severity is how urgent an alarm is.
type Severity string

const (
	// SeverityCritical: data at risk or the active datacenter does not serve; the
	// operator has to act.
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
)

// Proto returns the proto enum of the severity.
func (s Severity) Proto() dcctlv1.Status_Alarm_Severity {
	switch s {
	case SeverityCritical:
		return dcctlv1.Status_Alarm_CRITICAL
	case SeverityWarning:
		return dcctlv1.Status_Alarm_WARNING
	}

	return dcctlv1.Status_Alarm_SEVERITY_UNSPECIFIED
}

// AlarmKey tells one alarm from another: an alarm of the installation has neither a
// datacenter nor a voter, one of a datacenter has the datacenter, one of a voter has
// the voter.
type AlarmKey struct {
	ID         AlarmID
	Datacenter string
	// Voter is the name of the voter, see [Voter.Name].
	Voter string
}

// Alarm is a raised alarm. It exists while its condition holds.
type Alarm struct {
	ID       AlarmID
	Severity Severity
	// Datacenter is of a datacenter alarm, or of the KV voter of a voter alarm in
	// warm_standby.
	Datacenter string
	// Voter and VoterType are set for the alarms of a voter.
	Voter     string
	VoterType VoterType
	// StartTime is the end of the cycle that raised the alarm.
	StartTime time.Time
	Detail    string
	// Facts are the data the alarm is based on. They hold only what
	// google.protobuf.Struct takes: strings, booleans, ints, []any and maps of them.
	Facts map[string]any
}

// Key returns the key of the alarm.
func (a Alarm) Key() AlarmKey {
	return AlarmKey{ID: a.ID, Datacenter: a.Datacenter, Voter: a.Voter}
}

// alarmDef is how an alarm is raised.
type alarmDef struct {
	typ      dcctlv1.Status_Alarm_Type
	severity Severity
	scope    AlarmScope
	// delayed alarms are raised once the condition holds for status.degraded_after.
	delayed bool
	// suppressible alarms are not raised for the datacenters of a running operation,
	// which makes their condition hold on the way.
	suppressible bool
}

// lease_lost is not delayed: its condition already holds only after the counter
// stood still for T + acquire_after.
var alarmDefs = map[AlarmID]alarmDef{
	AlarmConflict:              {typ: dcctlv1.Status_Alarm_CONFLICT, scope: AlarmScopeInstallation, severity: SeverityCritical},
	AlarmUncommitted:           {typ: dcctlv1.Status_Alarm_UNCOMMITTED, scope: AlarmScopeInstallation, severity: SeverityCritical},
	AlarmStale:                 {typ: dcctlv1.Status_Alarm_STALE, scope: AlarmScopeDatacenter, severity: SeverityWarning, delayed: true},
	AlarmSplitBrain:            {typ: dcctlv1.Status_Alarm_SPLIT_BRAIN, scope: AlarmScopeDatacenter, severity: SeverityCritical},
	AlarmDBSplit:               {typ: dcctlv1.Status_Alarm_DB_SPLIT, scope: AlarmScopeDatacenter, severity: SeverityCritical},
	AlarmMultiplePrimaries:     {typ: dcctlv1.Status_Alarm_MULTIPLE_PRIMARIES, scope: AlarmScopeInstallation, severity: SeverityCritical},
	AlarmDBPrimaryMissing:      {typ: dcctlv1.Status_Alarm_DB_PRIMARY_MISSING, scope: AlarmScopeDatacenter, severity: SeverityCritical, delayed: true, suppressible: true},
	AlarmDatacenterUnreachable: {typ: dcctlv1.Status_Alarm_DATACENTER_UNREACHABLE, scope: AlarmScopeDatacenter, severity: SeverityWarning, delayed: true},
	AlarmDBLeaderMismatch:      {typ: dcctlv1.Status_Alarm_DB_LEADER_MISMATCH, scope: AlarmScopeInstallation, severity: SeverityWarning, delayed: true, suppressible: true},
	AlarmLeaseLost:             {typ: dcctlv1.Status_Alarm_LEASE_LOST, scope: AlarmScopeInstallation, severity: SeverityWarning, suppressible: true},
	AlarmAppsDegraded:          {typ: dcctlv1.Status_Alarm_APPS_DEGRADED, scope: AlarmScopeDatacenter, severity: SeverityWarning, delayed: true, suppressible: true},
	AlarmDBDegraded:            {typ: dcctlv1.Status_Alarm_DB_DEGRADED, scope: AlarmScopeDatacenter, severity: SeverityWarning, delayed: true},
	AlarmStandbyNotFollowing:   {typ: dcctlv1.Status_Alarm_STANDBY_NOT_FOLLOWING, scope: AlarmScopeDatacenter, severity: SeverityWarning, delayed: true, suppressible: true},
	AlarmVoterUnreachable:      {typ: dcctlv1.Status_Alarm_VOTER_UNREACHABLE, scope: AlarmScopeVoter, severity: SeverityWarning, delayed: true},
	AlarmQuorumLost:            {typ: dcctlv1.Status_Alarm_QUORUM_LOST, scope: AlarmScopeInstallation, severity: SeverityWarning, delayed: true},
	AlarmArbiterUninitialized:  {typ: dcctlv1.Status_Alarm_ARBITER_UNINITIALIZED, scope: AlarmScopeArbiter, severity: SeverityWarning, delayed: true},
	AlarmOperationInterrupted:  {typ: dcctlv1.Status_Alarm_OPERATION_INTERRUPTED, scope: AlarmScopeInstallation, severity: SeverityWarning, delayed: true},
}
