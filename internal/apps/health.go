package apps

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

const (
	jobTypeSystem = "system"

	allocPending = "pending"
	allocRunning = "running"
	allocUnknown = "unknown"
	desiredRun   = "run"
	taskRunning  = "running"

	nodeReady    = "ready"
	nodeEligible = "eligible"
)

// activeAllocs counts the allocations that may still run: pending, running or unknown.
func activeAllocs(allocs []nomad.Alloc) int {
	var n int

	for _, a := range allocs {
		switch a.ClientStatus {
		case allocPending, allocRunning, allocUnknown:
			n++
		}
	}

	return n
}

// stopped reports whether the job has Stop set and no active allocation; otherwise
// detail says what is left.
func stopped(job nomad.Job, allocs []nomad.Alloc) (ok bool, detail string) {
	active := activeAllocs(allocs)

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

// serviceHealth reports whether one allocation of the job is healthy; otherwise detail
// is the problem of an allocation, one meant to run if there is any.
func serviceHealth(allocs []nomad.Alloc, now time.Time, minHealthy time.Duration) (ok bool, detail string) {
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

// systemHealth reports whether the system job has a healthy allocation on every node
// that is ready, eligible, not draining and runs its service, and at least one healthy
// allocation at all. A node that runs the service but is out of scheduling and has no
// healthy allocation is a warning.
func systemHealth(
	jobID string, allocs []nomad.Alloc, nodes []nomad.Node, now time.Time, minHealthy time.Duration,
) (ok bool, detail string, warnings []string) {
	healthyOn := make(map[string]bool)

	for _, a := range allocs {
		if allocProblem(a, now, minHealthy) == "" {
			healthyOn[a.NodeID] = true
		}
	}

	var missing []string

	for _, n := range nodes {
		if !runsService(n, jobID) || healthyOn[n.ID] {
			continue
		}

		if why := outOfService(n); why != "" {
			warnings = append(warnings, fmt.Sprintf("node %s is %s and has no healthy allocation", n.Name, why))

			continue
		}

		missing = append(missing, n.Name)
	}

	switch {
	case len(healthyOn) == 0:
		_, problem := serviceHealth(allocs, now, minHealthy)

		return false, "no healthy allocation: " + problem, warnings
	case len(missing) > 0:
		return false, "no healthy allocation on " + strings.Join(missing, ", "), warnings
	}

	return true, "", warnings
}

// runsService reports whether meta.services of the node holds the service of the job.
// A job is named after its service with "-" in place of "_".
func runsService(n nomad.Node, jobID string) bool {
	return slices.ContainsFunc(n.Services, func(s string) bool {
		return strings.ReplaceAll(s, "_", "-") == jobID
	})
}

// outOfService returns why Nomad does not place allocations on the node, or "".
func outOfService(n nomad.Node) string {
	switch {
	case n.Status != nodeReady:
		return n.Status
	case n.Drain:
		return "draining"
	case n.SchedulingEligibility != nodeEligible:
		return "ineligible"
	}

	return ""
}
