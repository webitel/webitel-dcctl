package status_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func nomadOf(t *testing.T, n collect.Nomad) status.Nomad {
	t.Helper()

	s := warm()
	dcOf(s, dcA).Nomad = up(n)

	return datacenter(t, status.Build(s, testConfig(), now), dcA).Nomad
}

func jobNamed(t *testing.T, n status.Nomad, name string) status.Job {
	t.Helper()

	for _, st := range n.Stages {
		for _, j := range st.Jobs {
			if j.Name == name {
				return j
			}
		}
	}

	t.Fatalf("no job %s", name)

	return status.Job{}
}

func TestBuild_jobState(t *testing.T) {
	alloc := func(client, desired string, up time.Duration) nomad.Alloc {
		return nomad.Alloc{
			ID: "a", NodeID: "n1", ClientStatus: client, DesiredStatus: desired,
			TaskStates: map[string]nomad.TaskState{"main": {State: "running", StartedAt: now.Add(-up)}},
		}
	}

	tests := []struct {
		name   string
		stop   bool
		allocs []nomad.Alloc
		want   status.JobState
	}{
		{"stopped_without_allocs", true, nil, status.JobStopped},
		{"stopped_allocs_finished", true, []nomad.Alloc{alloc("complete", "stop", time.Hour)}, status.JobStopped},
		{"stopping_pending", true, []nomad.Alloc{alloc("pending", "stop", 0)}, status.JobStopping},
		{"stopping_running", true, []nomad.Alloc{alloc("running", "stop", time.Hour)}, status.JobStopping},
		{"stopping_unknown", true, []nomad.Alloc{alloc("unknown", "stop", time.Hour)}, status.JobStopping},
		{"healthy", false, []nomad.Alloc{alloc("running", "run", time.Hour)}, status.JobHealthy},
		{"starting_is_unhealthy", false, []nomad.Alloc{alloc("running", "run", time.Second)}, status.JobUnhealthy},
		{"pending_is_unhealthy", false, []nomad.Alloc{alloc("pending", "run", 0)}, status.JobUnhealthy},
		{"no_alloc_is_unhealthy", false, nil, status.JobUnhealthy},
		{"failed_is_unhealthy", false, []nomad.Alloc{alloc("failed", "run", time.Hour)}, status.JobUnhealthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := collect.Nomad{
				Managed: []nomad.Job{{ID: "engine", Name: "engine", Type: "service", Stage: 2, Stop: tt.stop}},
				Allocs:  map[string][]nomad.Alloc{"engine": tt.allocs},
				Nodes:   []nomad.Node{readyNode("n1", "engine")},
			}

			j := jobNamed(t, nomadOf(t, n), "engine")
			assert.Equal(t, tt.want, j.State)
			assert.Equal(t, tt.want == status.JobHealthy || tt.want == status.JobStopped, j.Detail == "", j.Detail)
		})
	}
}

func TestBuild_systemJob(t *testing.T) {
	drained := nomad.Node{
		ID: "n2", Name: "n2", Status: "ready", SchedulingEligibility: "ineligible", Drain: true,
		Services: []string{"media_exporter"},
	}
	down := nomad.Node{ID: "n3", Name: "n3", Status: "down", SchedulingEligibility: "eligible", Services: []string{"media_exporter"}}

	t.Run("node_in_drain", func(t *testing.T) {
		n := collect.Nomad{
			Managed: []nomad.Job{{ID: "media-exporter", Name: "media-exporter", Type: "system", Stage: 2}},
			Allocs:  map[string][]nomad.Alloc{"media-exporter": {healthyAlloc("media-exporter", "n1")}},
			Nodes:   []nomad.Node{readyNode("n1", "media_exporter"), drained, down},
		}

		j := jobNamed(t, nomadOf(t, n), "media-exporter")
		assert.Equal(t, status.JobHealthy, j.State)
		assert.Equal(t, 1, j.HealthyNodeCount)
		assert.Equal(t, 1, j.ExpectedNodeCount)
		assert.Equal(t, []jobstate.MissingNode{
			{Name: "n2", Reason: jobstate.NodeDraining},
			{Name: "n3", Reason: jobstate.NodeDown},
		}, j.MissingNodes)
	})

	t.Run("ready_node_without_alloc", func(t *testing.T) {
		n := collect.Nomad{
			Managed: []nomad.Job{{ID: "media-exporter", Name: "media-exporter", Type: "system", Stage: 2}},
			Allocs:  map[string][]nomad.Alloc{"media-exporter": {healthyAlloc("media-exporter", "n1")}},
			Nodes:   []nomad.Node{readyNode("n1", "media_exporter"), readyNode("n4", "media_exporter")},
		}

		j := jobNamed(t, nomadOf(t, n), "media-exporter")
		assert.Equal(t, status.JobUnhealthy, j.State)
		assert.Equal(t, "no healthy allocation on n4", j.Detail)
		assert.Equal(t, []jobstate.MissingNode{{Name: "n4", Reason: jobstate.NodeNoAllocation}}, j.MissingNodes)
	})

	t.Run("stopped", func(t *testing.T) {
		n := collect.Nomad{
			Managed: []nomad.Job{{ID: "media-exporter", Name: "media-exporter", Type: "system", Stage: 2, Stop: true}},
			Nodes:   []nomad.Node{readyNode("n1", "media_exporter")},
		}

		j := jobNamed(t, nomadOf(t, n), "media-exporter")
		assert.Equal(t, status.JobStopped, j.State)
		assert.Empty(t, j.MissingNodes)
		assert.Zero(t, j.ExpectedNodeCount)
	})
}

func TestBuild_nomadLeader(t *testing.T) {
	n := runningNomad()
	n.Leader = "10.1.0.91:4647"
	assert.Equal(t, "10.1.0.91:4647", nomadOf(t, n).Leader)

	n.Leader = ""
	assert.Empty(t, nomadOf(t, n).Leader, "unknown")
}

func TestBuild_nomadSummary(t *testing.T) {
	n := runningNomad()
	n.Managed = append(n.Managed,
		nomad.Job{ID: "opensips", Name: "opensips", Type: "service", Stage: 3, Stop: true},
		nomad.Job{ID: "broken", Name: "broken", Type: "service", StageErr: nomad.ErrInvalidStage},
	)
	n.Allocs["engine"] = append(n.Allocs["engine"], nomad.Alloc{ID: "old", ClientStatus: "failed", DesiredStatus: "stop"})
	n.Nodes = append(n.Nodes,
		nomad.Node{ID: "n2", Name: "n2", Status: "ready", SchedulingEligibility: "ineligible", Drain: true},
		nomad.Node{ID: "n3", Name: "n3", Status: "down", SchedulingEligibility: "eligible"},
		nomad.Node{ID: "n4", Name: "n4", Status: "ready", SchedulingEligibility: "ineligible"},
	)

	got := nomadOf(t, n)

	assert.Equal(t, "1.9.0", got.Version)
	assert.Equal(t, 1, got.ReadyNodeCount)
	assert.Equal(t, 1, got.DrainingNodeCount)
	assert.Equal(t, 1, got.DownNodeCount)
	assert.Equal(t, 1, got.IneligibleNodeCount)
	assert.Equal(t, []status.Node{
		{Name: "n2", Reason: jobstate.NodeDraining},
		{Name: "n3", Reason: jobstate.NodeDown},
		{Name: "n4", Reason: jobstate.NodeIneligible},
	}, got.NotReadyNodes)
	assert.Equal(t, status.JobCounts{Stopped: 1, Healthy: 2, Unhealthy: 1}, got.JobCounts)
	assert.Equal(t, []status.UnmanagedJob{{Name: "consul-esm", NomadStatus: "running"}}, got.UnmanagedJobs)

	require.Len(t, got.Stages, 4)

	stages := make([]int, len(got.Stages))
	for i, st := range got.Stages {
		stages[i] = st.Stage
	}

	assert.Equal(t, []int{0, 1, 2, 3}, stages, "an invalid stage is stage 0")
	assert.True(t, got.Stages[0].Jobs[0].StageInvalid)
	assert.Equal(t, status.JobCounts{Healthy: 1}, got.Stages[2].JobCounts)

	engine := jobNamed(t, got, "engine")
	assert.Equal(t, map[string]int{"running": 1, "failed": 1}, engine.AllocationCounts)
	assert.Equal(t, uint64(12), engine.ModifyIndex)
	assert.Equal(t, "service", engine.Type)
}
