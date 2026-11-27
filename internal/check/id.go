package check

// Coordination checks.
const (
	CoordStatePresent    ID = "coord.state_present"
	CoordNoOperation     ID = "coord.no_operation"
	CoordLockFree        ID = "coord.lock_free"
	CoordLeaderReachable ID = "coord.leader_reachable"
	CoordNoConflict      ID = "coord.no_conflict"
	// CoordNoUncommitted also fails on a record that unreachable voters may have
	// committed, except for a failover, which writes over the highest such record.
	CoordNoUncommitted ID = "coord.no_uncommitted"
	CoordNoSplit       ID = "coord.no_split"
	CoordQuorum        ID = "coord.quorum"
	// CoordIsolation always needs [ConfirmIsolation]; its facts are what the operator
	// judges the isolation of the old primary by.
	CoordIsolation              ID = "coord.isolation"
	CoordTargetValid            ID = "coord.target_valid"
	CoordTargetNotNeedsRecovery ID = "coord.target_not_needs_recovery"
	CoordDCNotActive            ID = "coord.dc_not_active"
	CoordDCNeedsRecovery        ID = "coord.dc_needs_recovery"
	// CoordProfile fails on an action the profile has no scenario for, as demote and
	// repoint in stretch.
	CoordProfile ID = "coord.profile"
	// CoordBaseUnchanged fails when the voters no longer hold the record an operation
	// was planned from. The engine decides it under the lock, after the preflight.
	CoordBaseUnchanged ID = "coord.base_unchanged"
)

// Patroni checks. A check about one datacenter has a result for each datacenter the
// action involves.
const (
	PatroniMembersFound        ID = "patroni.members_found"
	PatroniReachable           ID = "patroni.reachable"
	PatroniVersion             ID = "patroni.version"
	PatroniNotPaused           ID = "patroni.not_paused"
	PatroniHasLeader           ID = "patroni.has_leader"
	PatroniSinglePrimary       ID = "patroni.single_primary"
	PatroniSourcePrimary       ID = "patroni.source_primary"
	PatroniTargetStandbyLeader ID = "patroni.target_standby_leader"
	PatroniTargetFollowsSource ID = "patroni.target_follows_source"
	PatroniTargetLag           ID = "patroni.target_lag"
	PatroniOthersFollowSource  ID = "patroni.others_follow_source"
	PatroniTargetNotStreaming  ID = "patroni.target_not_streaming"
	PatroniTargetMaxLSN        ID = "patroni.target_max_lsn"
	// PatroniSourceDiscard needs the WAL accepted that the old primary, still primary,
	// holds beyond the target of a failover: the failover demotes it.
	PatroniSourceDiscard   ID = "patroni.source_discard"
	PatroniTargetCandidate ID = "patroni.target_candidate"
	PatroniTargetLeader    ID = "patroni.target_leader"
	PatroniCandidateLoss   ID = "patroni.candidate_loss"
	PatroniActivePrimary   ID = "patroni.active_primary"
	PatroniDCPrimary       ID = "patroni.dc_primary"
	PatroniDCStandby       ID = "patroni.dc_standby"
	PatroniDivergence      ID = "patroni.divergence"
	PatroniDCRecovered     ID = "patroni.dc_recovered"
)

// Nomad and infrastructure checks.
const (
	NomadReachable ID = "nomad.reachable"
	// NomadStagesValid fails on a managed job whose meta.dcctl_stage is invalid: stop
	// takes such a job down, while start refuses to run, so a rollback would leave the
	// datacenter without its apps.
	NomadStagesValid       ID = "nomad.stages_valid"
	NomadTargetJobsPresent ID = "nomad.target_jobs_present"
	NomadTargetJobsStopped ID = "nomad.target_jobs_stopped"
	NomadTargetPlan        ID = "nomad.target_plan"
	NomadTargetNodes       ID = "nomad.target_nodes"
	NomadActivePlan        ID = "nomad.active_plan"
	NomadDCAppsStopped     ID = "nomad.dc_apps_stopped"
	InfraTargetRabbitMQ    ID = "infra.target_rabbitmq"
)

// Checks of operations, conflict resolution, initialization and verify.
const (
	OpInterrupted ID = "op.interrupted"
	// OpStepsKnown fails on an operation with a step of a type this dcctl does not
	// know, written by a newer version: it cannot be resumed here.
	OpStepsKnown ID = "op.steps_known"
	// OpNotRolledBack fails on an operation some of whose steps were rolled back, or
	// whose rollback failed: running it forward would skip the undone steps.
	OpNotRolledBack ID = "op.not_rolled_back"
	// OpGenerationDetermined needs [ConfirmIsolation] to abandon an operation whose
	// new generation voters that do not answer may hold, which would make it
	// committed.
	OpGenerationDetermined ID = "op.generation_determined"
	OpPointOfNoReturn      ID = "op.point_of_no_return"
	ResolveConflict        ID = "resolve.conflict"
	ResolveWinner          ID = "resolve.winner"
	InitStateAbsent        ID = "init.state_absent"
	InitAllReachable       ID = "init.all_reachable"
	InitActiveValid        ID = "init.active_valid"
	InitActiveHasPrimary   ID = "init.active_has_primary"
	ArbiterReachable       ID = "arbiter.reachable"
	ArbiterEmpty           ID = "arbiter.empty"
	ArbiterStatesEqual     ID = "arbiter.states_equal"
	VerifyPatroni          ID = "verify.patroni"
	VerifyJobs             ID = "verify.jobs"
	VerifyState            ID = "verify.state"
)

// Informational checks. They affect neither state, health, alarms nor preflight.
const (
	// InfoAPI is the prepared query webitel-api that nginx of the datacenter resolves
	// /api with. In stretch the query is one for the cluster, so every datacenter gets
	// the same result.
	InfoAPI ID = "info.api"
	// InfoServices is status.info_services in the datacenter.
	InfoServices ID = "info.services"
	// InfoRabbitMQ is status.rabbitmq_service for the datacenter.
	InfoRabbitMQ ID = "info.rabbitmq"
)
