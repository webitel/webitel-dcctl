package telemetry

import "go.opentelemetry.io/otel/attribute"

// The attributes of the webitel semantic conventions registry, which has no generated
// Go package yet.
const (
	datacenterNameKey  = attribute.Key("webitel.datacenter.name")
	datacenterStateKey = attribute.Key("webitel.dcctl.datacenter.state")
	voterNameKey       = attribute.Key("webitel.dcctl.voter.name")
	voterStateKey      = attribute.Key("webitel.dcctl.voter.state")
	quorumModeKey      = attribute.Key("webitel.dcctl.quorum.mode")
	alarmNameKey       = attribute.Key("webitel.dcctl.alarm.name")
	patroniRoleKey     = attribute.Key("webitel.dcctl.patroni.role")
	jobStateKey        = attribute.Key("webitel.dcctl.job.state")
	jobStageKey        = attribute.Key("webitel.dcctl.job.stage")
	sourceSystemKey    = attribute.Key("webitel.dcctl.source.system")
	sourceStateKey     = attribute.Key("webitel.dcctl.source.state")
	operationTypeKey   = attribute.Key("webitel.dcctl.operation.type")
	operationStateKey  = attribute.Key("webitel.dcctl.operation.state")
	operationModeKey   = attribute.Key("webitel.dcctl.operation.mode")
	stepTypeKey        = attribute.Key("webitel.dcctl.step.type")
	stepStateKey       = attribute.Key("webitel.dcctl.step.state")
)

// The values of webitel.dcctl.voter.state.
const (
	voterOK            = "ok"
	voterUnreachable   = "unreachable"
	voterUninitialized = "uninitialized"
)

// The values of webitel.dcctl.patroni.role.
const (
	rolePrimary       = "primary"
	roleStandbyLeader = "standby_leader"
	roleReplica       = "replica"
	roleOther         = "other"
	roleUnknown       = "unknown"
)

// The values of webitel.dcctl.source.system and webitel.dcctl.source.state.
const (
	systemArbiter = "arbiter"

	sourceOK          = "ok"
	sourcePartial     = "partial"
	sourceUnreachable = "unreachable"
)
