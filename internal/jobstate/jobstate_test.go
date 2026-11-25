package jobstate_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

const minHealthy = 10 * time.Second

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func runningAlloc(node string, up time.Duration) nomad.Alloc {
	return nomad.Alloc{
		ID: "a-" + node, NodeID: node, ClientStatus: "running", DesiredStatus: "run",
		TaskStates: map[string]nomad.TaskState{
			"main":    {State: "running", StartedAt: now.Add(-time.Hour)},
			"sidecar": {State: "running", StartedAt: now.Add(-up)},
		},
	}
}

func TestStopped(t *testing.T) {
	tests := []struct {
		name   string
		stop   bool
		status []string
		want   bool
	}{
		{"stopped_without_allocs", true, nil, true},
		{"stopped_allocs_done", true, []string{"complete", "failed", "lost"}, true},
		{"not_stopped", false, []string{"complete"}, false},
		{"stop_set_alloc_running", true, []string{"complete", "running"}, false},
		{"stop_set_alloc_pending", true, []string{"pending"}, false},
		{"stop_set_alloc_unknown", true, []string{"unknown"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var allocs []nomad.Alloc
			for _, s := range tt.status {
				allocs = append(allocs, nomad.Alloc{ClientStatus: s})
			}

			got, detail := jobstate.Stopped(nomad.Job{ID: "engine", Stop: tt.stop}, allocs)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.want, detail == "", "a job not stopped says why: %q", detail)
		})
	}
}

func TestServiceHealth(t *testing.T) {
	tests := []struct {
		name   string
		allocs []nomad.Alloc
		want   bool
		detail string
	}{
		{"healthy", []nomad.Alloc{runningAlloc("n1", minHealthy)}, true, ""},
		{"no_allocs", nil, false, "no allocation"},
		{"too_young", []nomad.Alloc{runningAlloc("n1", 4*time.Second)}, false, "task sidecar running for 4s of 10s"},
		{
			"task_not_running",
			[]nomad.Alloc{func() nomad.Alloc {
				a := runningAlloc("n1", time.Hour)
				a.TaskStates["sidecar"] = nomad.TaskState{State: "pending", Restarts: 1}

				return a
			}()},
			false, "task sidecar is pending",
		},
		{
			"task_never_started",
			[]nomad.Alloc{func() nomad.Alloc {
				a := runningAlloc("n1", time.Hour)
				a.TaskStates["sidecar"] = nomad.TaskState{State: "running"}

				return a
			}()},
			false, "task sidecar has no start time",
		},
		{
			"being_stopped",
			[]nomad.Alloc{func() nomad.Alloc {
				a := runningAlloc("n1", time.Hour)
				a.DesiredStatus = "stop"

				return a
			}()},
			false, "allocation is being stopped",
		},
		{
			"client_unknown",
			[]nomad.Alloc{func() nomad.Alloc {
				a := runningAlloc("n1", time.Hour)
				a.ClientStatus = "unknown"

				return a
			}()},
			false, "allocation is unknown",
		},
		{
			"no_task_states",
			[]nomad.Alloc{{ID: "a", ClientStatus: "running", DesiredStatus: "run"}},
			false, "allocation has no tasks",
		},
		{
			"old_failed_and_new_healthy",
			[]nomad.Alloc{{ID: "old", ClientStatus: "failed", DesiredStatus: "stop"}, runningAlloc("n1", time.Hour)},
			true, "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := jobstate.ServiceHealth(tt.allocs, now, minHealthy)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.detail, detail)
		})
	}
}

func node(id, status, eligibility string, drain bool, services ...string) nomad.Node {
	return nomad.Node{
		ID: id, Name: id, Status: status, SchedulingEligibility: eligibility, Drain: drain, Services: services,
	}
}

func TestSystemHealth(t *testing.T) {
	ready := func(id string) nomad.Node { return node(id, "ready", "eligible", false, "media_exporter") }

	tests := []struct {
		name     string
		nodes    []nomad.Node
		allocs   []nomad.Alloc
		want     jobstate.System
		warnings []string
	}{
		{
			name:   "every_node",
			nodes:  []nomad.Node{ready("n1"), ready("n2")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour), runningAlloc("n2", time.Hour)},
			want:   jobstate.System{Healthy: true, HealthyNodes: 2, ExpectedNodes: 2},
		},
		{
			name:   "ready_node_without_alloc",
			nodes:  []nomad.Node{ready("n1"), ready("n2")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour)},
			want: jobstate.System{
				Detail: "no healthy allocation on n2", HealthyNodes: 1, ExpectedNodes: 2,
				Missing: []jobstate.MissingNode{{Name: "n2", Reason: jobstate.NodeNoAllocation}},
			},
		},
		{
			name:   "ready_node_with_young_alloc",
			nodes:  []nomad.Node{ready("n1"), ready("n2")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour), runningAlloc("n2", time.Second)},
			want: jobstate.System{
				Detail: "no healthy allocation on n2", HealthyNodes: 1, ExpectedNodes: 2,
				Missing: []jobstate.MissingNode{{Name: "n2", Reason: jobstate.NodeNoAllocation}},
			},
		},
		{
			name: "draining_node",
			nodes: []nomad.Node{
				ready("n1"), node("n2", "ready", "ineligible", true, "media_exporter"),
			},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour)},
			want: jobstate.System{
				Healthy: true, HealthyNodes: 1, ExpectedNodes: 1,
				Missing: []jobstate.MissingNode{{Name: "n2", Reason: jobstate.NodeDraining}},
			},
			warnings: []string{"node n2 is draining and has no healthy allocation"},
		},
		{
			name:   "down_node",
			nodes:  []nomad.Node{ready("n1"), node("n2", "down", "eligible", false, "media_exporter")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour)},
			want: jobstate.System{
				Healthy: true, HealthyNodes: 1, ExpectedNodes: 1,
				Missing: []jobstate.MissingNode{{Name: "n2", Reason: jobstate.NodeDown}},
			},
			warnings: []string{"node n2 is down and has no healthy allocation"},
		},
		{
			name:   "ineligible_node",
			nodes:  []nomad.Node{ready("n1"), node("n2", "ready", "ineligible", false, "media_exporter")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour)},
			want: jobstate.System{
				Healthy: true, HealthyNodes: 1, ExpectedNodes: 1,
				Missing: []jobstate.MissingNode{{Name: "n2", Reason: jobstate.NodeIneligible}},
			},
			warnings: []string{"node n2 is ineligible and has no healthy allocation"},
		},
		{
			name:  "all_nodes_drained",
			nodes: []nomad.Node{node("n1", "ready", "ineligible", true, "media_exporter")},
			want: jobstate.System{
				Detail:  "no healthy allocation: no allocation",
				Missing: []jobstate.MissingNode{{Name: "n1", Reason: jobstate.NodeDraining}},
			},
			warnings: []string{"node n1 is draining and has no healthy allocation"},
		},
		{
			name:   "node_without_the_service",
			nodes:  []nomad.Node{ready("n1"), node("n2", "ready", "eligible", false, "opensips")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour)},
			want:   jobstate.System{Healthy: true, HealthyNodes: 1, ExpectedNodes: 1},
		},
		{
			name:   "healthy_on_draining_node_counts",
			nodes:  []nomad.Node{ready("n1"), node("n2", "ready", "ineligible", true, "media_exporter")},
			allocs: []nomad.Alloc{runningAlloc("n1", time.Hour), runningAlloc("n2", time.Hour)},
			want:   jobstate.System{Healthy: true, HealthyNodes: 2, ExpectedNodes: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jobstate.SystemHealth("media-exporter", tt.allocs, tt.nodes, now, minHealthy)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.warnings, got.Warnings())
		})
	}
}

func TestOutOfService(t *testing.T) {
	tests := []struct {
		node nomad.Node
		want jobstate.NodeReason
	}{
		{node("ready", "ready", "eligible", false), ""},
		{node("down", "down", "ineligible", true), jobstate.NodeDown},
		{node("initializing", "initializing", "eligible", false), jobstate.NodeDown},
		{node("draining", "ready", "ineligible", true), jobstate.NodeDraining},
		{node("ineligible", "ready", "ineligible", false), jobstate.NodeIneligible},
	}

	for _, tt := range tests {
		t.Run(tt.node.Name, func(t *testing.T) {
			assert.Equal(t, tt.want, jobstate.OutOfService(tt.node))
		})
	}
}
