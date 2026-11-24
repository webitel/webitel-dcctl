package nomad

import "time"

// Job is a registered Nomad job as the job list shows it.
type Job struct {
	ID     string
	Name   string
	Type   string
	Status string
	// Stop is set when the job is registered but stopped.
	Stop bool
	// Stage is meta.dcctl_stage, at least 1; it is zero for an unmanaged job and for a
	// managed job whose stage is invalid.
	Stage int
	// StageErr is set on a managed job whose meta.dcctl_stage is not an integer of at
	// least 1. It is [ErrInvalidStage]; what to do with such a job is up to the caller.
	StageErr       error
	ModifyIndex    uint64
	JobModifyIndex uint64
}

// Alloc is an allocation of a job.
type Alloc struct {
	ID            string
	JobID         string
	TaskGroup     string
	NodeID        string
	ClientStatus  string
	DesiredStatus string
	// TaskStates maps the task name to its state.
	TaskStates map[string]TaskState
}

// TaskState is the state of one task of an allocation.
type TaskState struct {
	State    string
	Failed   bool
	Restarts uint64
	// StartedAt is the zero time when the task has not started.
	StartedAt time.Time
}

// Node is a Nomad client node.
type Node struct {
	ID                    string
	Name                  string
	Datacenter            string
	Status                string
	SchedulingEligibility string
	Drain                 bool
	// Services is the list from meta.services.
	Services []string
}

// PlanResult is the outcome of a dry-run register of a job.
type PlanResult struct {
	// FailedTGAllocs lists the task groups Nomad could not place, sorted.
	FailedTGAllocs []string
}

// EndpointStatus is whether one Nomad server answers and passes TLS verification.
type EndpointStatus struct {
	Addr string
	// Err is a [*source.Error] when the endpoint is not usable.
	Err error
}
