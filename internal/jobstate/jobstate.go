// Package jobstate tells whether a managed Nomad job is stopped or healthy. The apps
// primitives wait for these conditions and the status reports them, so both judge a job
// the same way:
//
//   - stopped: Stop is set and no allocation is pending, running or unknown;
//   - healthy service job: an allocation runs with every task running for at least the
//     minimal healthy time;
//   - healthy system job: such an allocation on every node that is ready, eligible, not
//     draining and has the service in meta.services. A node out of scheduling without
//     one is a warning; no healthy allocation at all keeps the job unhealthy.
package jobstate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

// TypeSystem is the Nomad type of a job that runs on every node of its service.
const TypeSystem = "system"

const (
	allocPending = "pending"
	allocRunning = "running"
	allocUnknown = "unknown"
	desiredRun   = "run"
	taskRunning  = "running"

	nodeReady    = "ready"
	nodeEligible = "eligible"
)

// NodeReason is why Nomad does not place allocations on a node.
type NodeReason string

const (
	// NodeDown is a node whose status is not ready.
	NodeDown       NodeReason = "down"
	NodeDraining   NodeReason = "draining"
	NodeIneligible NodeReason = "ineligible"
	// NodeNoAllocation is a node in scheduling without a healthy allocation of a system
	// job, which keeps the job unhealthy.
	NodeNoAllocation NodeReason = "no_allocation"
)

// ActiveAllocs counts the allocations that may still run: pending, running or unknown.
func ActiveAllocs(allocs []nomad.Alloc) int {
	var n int

	for _, a := range allocs {
		switch a.ClientStatus {
		case allocPending, allocRunning, allocUnknown:
			n++
		}
	}

	return n
}

// Stopped reports whether the job has Stop set and no active allocation; otherwise
// detail says what is left.
func Stopped(job nomad.Job, allocs []nomad.Alloc) (ok bool, detail string) {
	active := ActiveAllocs(allocs)

	switch {
	case !job.Stop && active > 0:
		return false, fmt.Sprintf("not stopped, %d allocations active", active)
	case !job.Stop:
		return false, "not stopped"
	case active > 0:
		return false, fmt.Sprintf("%d allocations active", active)
	}

	return true, ""
}

// UnknownLeft reports whether the job has Stop set and every allocation still active
// is unknown: its node does not answer, and Nomad stops the allocation once the node
// is back. Nothing can be done about such a job until then.
func UnknownLeft(job nomad.Job, allocs []nomad.Alloc) bool {
	if !job.Stop {
		return false
	}

	unknown := 0

	for _, a := range allocs {
		switch a.ClientStatus {
		case allocPending, allocRunning:
			return false
		case allocUnknown:
			unknown++
		}
	}

	return unknown > 0
}

// allocProblem returns why the allocation is not healthy, or "" when it is: it runs and
// every task has been running for at least minHealthy. Nomad sets StartedAt each time a
// task starts, so a task restarted within the window is not healthy yet. StartedAt comes
// from the clock of the Nomad client and now from ours, so the clocks must be kept in
// sync, by NTP.
func allocProblem(a nomad.Alloc, now time.Time, minHealthy time.Duration) string {
	switch {
	case a.DesiredStatus != desiredRun:
		return "allocation is being stopped"
	case a.ClientStatus != allocRunning:
		return "allocation is " + a.ClientStatus
	case len(a.TaskStates) == 0:
		return "allocation has no tasks"
	}

	for _, name := range slices.Sorted(maps.Keys(a.TaskStates)) {
		ts := a.TaskStates[name]

		switch up := now.Sub(ts.StartedAt); {
		case ts.State != taskRunning:
			return fmt.Sprintf("task %s is %s", name, ts.State)
		case ts.StartedAt.IsZero():
			return fmt.Sprintf("task %s has no start time", name)
		case up < minHealthy:
			return fmt.Sprintf("task %s running for %s of %s", name, up.Round(time.Second), minHealthy)
		}
	}

	return ""
}

// ServiceHealth reports whether one allocation of the job is healthy; otherwise detail
// is the problem of an allocation, one meant to run if there is any.
func ServiceHealth(allocs []nomad.Alloc, now time.Time, minHealthy time.Duration) (ok bool, detail string) {
	detail = "no allocation"
	meantToRun := false

	for _, a := range allocs {
		problem := allocProblem(a, now, minHealthy)
		if problem == "" {
			return true, ""
		}

		if !meantToRun || a.DesiredStatus == desiredRun {
			detail, meantToRun = problem, a.DesiredStatus == desiredRun
		}
	}

	return false, detail
}

// System is the health of a system job.
type System struct {
	Healthy bool
	// Detail says why the job is not healthy; empty when it is.
	Detail string
	// HealthyNodes counts the nodes with a healthy allocation, in scheduling or not.
	HealthyNodes int
	// ExpectedNodes counts the nodes that must have one: ready, eligible, not draining
	// and with the service.
	ExpectedNodes int
	// Missing are the nodes with the service and no healthy allocation, in the order of
	// the nodes.
	Missing []MissingNode
}

// MissingNode is a node of the service without a healthy allocation of a system job.
type MissingNode struct {
	Name   string
	Reason NodeReason
}

// Warnings describe the nodes out of scheduling without a healthy allocation.
func (s System) Warnings() []string {
	var out []string

	for _, m := range s.Missing {
		if m.Reason != NodeNoAllocation {
			out = append(out, fmt.Sprintf("node %s is %s and has no healthy allocation", m.Name, m.Reason))
		}
	}

	return out
}

// SystemHealth judges the system job jobID by its allocations on nodes, see [System].
func SystemHealth(jobID string, allocs []nomad.Alloc, nodes []nomad.Node, now time.Time, minHealthy time.Duration) System {
	healthyOn := make(map[string]bool)

	for _, a := range allocs {
		if allocProblem(a, now, minHealthy) == "" {
			healthyOn[a.NodeID] = true
		}
	}

	s := System{HealthyNodes: len(healthyOn)}

	var missing []string

	for _, n := range nodes {
		if !runsService(n, jobID) {
			continue
		}

		why := OutOfService(n)
		if why == "" {
			s.ExpectedNodes++
		}

		if healthyOn[n.ID] {
			continue
		}

		if why == "" {
			why = NodeNoAllocation

			missing = append(missing, n.Name)
		}

		s.Missing = append(s.Missing, MissingNode{Name: n.Name, Reason: why})
	}

	switch {
	case len(healthyOn) == 0:
		_, problem := ServiceHealth(allocs, now, minHealthy)
		s.Detail = "no healthy allocation: " + problem
	case len(missing) > 0:
		s.Detail = "no healthy allocation on " + strings.Join(missing, ", ")
	default:
		s.Healthy = true
	}

	return s
}

// runsService reports whether meta.services of the node holds the service of the job.
// A job is named after its service with "-" in place of "_".
func runsService(n nomad.Node, jobID string) bool {
	return slices.ContainsFunc(n.Services, func(s string) bool {
		return strings.ReplaceAll(s, "_", "-") == jobID
	})
}

// OutOfService returns why Nomad does not place allocations on the node, or "".
func OutOfService(n nomad.Node) NodeReason {
	switch {
	case n.Status != nodeReady:
		return NodeDown
	case n.Drain:
		return NodeDraining
	case n.SchedulingEligibility != nodeEligible:
		return NodeIneligible
	}

	return ""
}
