package apps

import "slices"

// Action is what a primitive does to the apps of a datacenter.
type Action string

const (
	ActionStop  Action = "stop"
	ActionStart Action = "start"
)

// StageState is the state of one stage.
type StageState string

const (
	StageRunning   StageState = "running"
	StageSucceeded StageState = "succeeded"
	StageFailed    StageState = "failed"
)

// JobState is the state of one job within its stage.
type JobState string

const (
	// JobPending is a job not read yet in its stage.
	JobPending  JobState = "pending"
	JobStopping JobState = "stopping"
	JobStopped  JobState = "stopped"
	JobStarting JobState = "starting"
	JobHealthy  JobState = "healthy"
	// JobUnhealthy is a job that lost its health within the stage, or did not reach it
	// before the stage failed.
	JobUnhealthy JobState = "unhealthy"
	// JobUnknown is a job whose state could not be read; Err tells why.
	JobUnknown JobState = "unknown"
)

// JobStatus is one job of a stage as the last poll saw it.
type JobStatus struct {
	ID string
	// Type is the Nomad job type: service or system.
	Type  string
	State JobState
	// Detail says what the job still lacks to reach the condition of the stage.
	Detail string
	// Warnings do not keep the job from the condition, such as a node in drain without
	// an allocation of a system job.
	Warnings []string
	// Err is the last error reading or writing the job.
	Err error
}

// Stage is one stage of a primitive with its jobs in ID order.
type Stage struct {
	// Number is the meta.dcctl_stage of the jobs.
	Number int
	State  StageState
	Jobs   []JobStatus
}

// Warnings returns the warnings of every job, each prefixed with the job ID.
func (s Stage) Warnings() []string {
	var out []string

	for _, j := range s.Jobs {
		for _, w := range j.Warnings {
			out = append(out, j.ID+": "+w)
		}
	}

	return out
}

func (s Stage) clone() Stage {
	s.Jobs = slices.Clone(s.Jobs)
	for i := range s.Jobs {
		s.Jobs[i].Warnings = slices.Clone(s.Jobs[i].Warnings)
	}

	return s
}

// Progress is reported when a stage begins, after every poll of it and when it ends.
type Progress struct {
	Datacenter string
	Action     Action
	Stage      Stage
}

// Result holds the stages a primitive ran, in the order it ran them, each as it ended.
// A stage the guard refused to begin is there too, failed, with its jobs pending.
type Result struct {
	Datacenter string
	Action     Action
	Stages     []Stage
}
