package status

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

// JobState is the state of a managed job.
type JobState string

const (
	JobStopped JobState = "stopped"
	// JobStopping has Stop set and allocations still pending, running or unknown.
	JobStopping JobState = "stopping"
	JobHealthy  JobState = "healthy"
	// JobUnhealthy is not stopped and not healthy, also while it starts.
	JobUnhealthy JobState = "unhealthy"
)

// Nomad is the Nomad cluster of a datacenter.
type Nomad struct {
	// Leader is the RPC address of the leader; empty when it is not known.
	Leader              string
	Version             string
	ReadyNodeCount      int
	IneligibleNodeCount int
	DrainingNodeCount   int
	DownNodeCount       int
	// NotReadyNodes are in the order Nomad lists them.
	NotReadyNodes []Node
	JobCounts     JobCounts
	// Stages are in ascending order; stage 0 holds the jobs with an invalid stage.
	Stages        []Stage
	UnmanagedJobs []UnmanagedJob
}

// Node is a Nomad node out of scheduling.
type Node struct {
	Name   string
	Reason jobstate.NodeReason
}

// JobCounts counts managed jobs by state.
type JobCounts struct {
	Stopped   int
	Stopping  int
	Healthy   int
	Unhealthy int
}

func countJob(c *JobCounts, s JobState) {
	switch s {
	case JobStopped:
		c.Stopped++
	case JobStopping:
		c.Stopping++
	case JobHealthy:
		c.Healthy++
	case JobUnhealthy:
		c.Unhealthy++
	}
}

// Total counts the jobs.
func (c JobCounts) Total() int {
	return c.Stopped + c.Stopping + c.Healthy + c.Unhealthy
}

// Stage is the managed jobs of one stage, in name order.
type Stage struct {
	Stage     int
	JobCounts JobCounts
	Jobs      []Job
}

// Job is a managed job: a Nomad job with meta.dcctl_stage.
type Job struct {
	Name string
	// Stage is 0 when meta.dcctl_stage is invalid.
	Stage        int
	StageInvalid bool
	// Type is the Nomad job type: service, system, batch or sysbatch.
	Type  string
	Stop  bool
	State JobState
	// Detail says why the job is not stopped or healthy; empty when it is either.
	Detail string
	// AllocationCounts counts the allocations by their client status.
	AllocationCounts map[string]int
	// ModifyIndex is the JobModifyIndex.
	ModifyIndex uint64
	// HealthyNodeCount, ExpectedNodeCount and MissingNodes are set for a system job
	// that is not stopped, see [jobstate.System].
	HealthyNodeCount  int
	ExpectedNodeCount int
	MissingNodes      []jobstate.MissingNode
}

// UnmanagedJob is a job without meta.dcctl_stage, which dcctl does not touch.
type UnmanagedJob struct {
	Name        string
	NomadStatus string
}

func nomadOf(n collect.Nomad, now time.Time, minHealthy time.Duration) Nomad {
	out := Nomad{Leader: n.Leader, Version: n.Version}

	for _, node := range n.Nodes {
		reason := jobstate.OutOfService(node)

		switch reason {
		case "":
			out.ReadyNodeCount++

			continue
		case jobstate.NodeDown:
			out.DownNodeCount++
		case jobstate.NodeDraining:
			out.DrainingNodeCount++
		case jobstate.NodeIneligible:
			out.IneligibleNodeCount++
		case jobstate.NodeNoAllocation:
			// A reason of a system job, never of a node by itself.
		}

		out.NotReadyNodes = append(out.NotReadyNodes, Node{Name: node.Name, Reason: reason})
	}

	byStage := make(map[int]*Stage)

	for _, j := range n.Managed {
		job := jobOf(j, n.Allocs[j.ID], n.Nodes, now, minHealthy)

		st, ok := byStage[job.Stage]
		if !ok {
			st = &Stage{Stage: job.Stage}
			byStage[job.Stage] = st
		}

		st.Jobs = append(st.Jobs, job)
		countJob(&st.JobCounts, job.State)
		countJob(&out.JobCounts, job.State)
	}

	for _, number := range slices.Sorted(maps.Keys(byStage)) {
		st := byStage[number]
		slices.SortFunc(st.Jobs, func(a, b Job) int { return strings.Compare(a.Name, b.Name) })
		out.Stages = append(out.Stages, *st)
	}

	for _, j := range n.Unmanaged {
		out.UnmanagedJobs = append(out.UnmanagedJobs, UnmanagedJob{Name: j.ID, NomadStatus: j.Status})
	}

	slices.SortFunc(out.UnmanagedJobs, func(a, b UnmanagedJob) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// jobOf judges the job by the same conditions the apps primitives wait for.
func jobOf(j nomad.Job, allocs []nomad.Alloc, nodes []nomad.Node, now time.Time, minHealthy time.Duration) Job {
	job := Job{
		Name:         j.ID,
		Stage:        j.Stage,
		StageInvalid: j.StageErr != nil,
		Type:         j.Type,
		Stop:         j.Stop,
		ModifyIndex:  j.JobModifyIndex,
	}

	if len(allocs) > 0 {
		job.AllocationCounts = make(map[string]int)
		for _, a := range allocs {
			job.AllocationCounts[a.ClientStatus]++
		}
	}

	if j.Stop {
		ok, detail := jobstate.Stopped(j, allocs)

		job.State, job.Detail = JobStopped, detail
		if !ok {
			job.State = JobStopping
		}

		return job
	}

	var healthy bool

	if j.Type == jobstate.TypeSystem {
		sys := jobstate.SystemHealth(j.ID, allocs, nodes, now, minHealthy)
		healthy, job.Detail = sys.Healthy, sys.Detail
		job.HealthyNodeCount, job.ExpectedNodeCount, job.MissingNodes = sys.HealthyNodes, sys.ExpectedNodes, sys.Missing
	} else {
		healthy, job.Detail = jobstate.ServiceHealth(allocs, now, minHealthy)
	}

	job.State = JobUnhealthy
	if healthy {
		job.State = JobHealthy
	}

	return job
}
