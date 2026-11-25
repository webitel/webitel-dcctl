package nomad_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

func setup(t *testing.T) (*nomad.Client, *fakeNomad) {
	t.Helper()

	ca := newTestCA(t, "dc_a CA")
	f := newFakeNomad()

	return newClient(t, ca, startTLS(t, ca, f.handler())), f
}

func TestJobsSplitManagedAndUnmanaged(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, false)
	f.addJob("storage", 6, map[string]string{"dcctl_stage": "1"}, true)
	f.addJob("grafana", 7, map[string]string{"owner": "ops"}, false)
	f.addJob("bare", 8, nil, false)

	managed, unmanaged, err := c.Jobs(t.Context())
	require.NoError(t, err)

	require.Len(t, managed, 2)
	assert.Equal(t, "storage", managed[0].ID, "managed jobs are ordered by stage, then ID")
	assert.Equal(t, 1, managed[0].Stage)
	assert.True(t, managed[0].Stop)
	assert.Equal(t, "engine", managed[1].ID)
	assert.Equal(t, 3, managed[1].Stage)
	assert.Equal(t, uint64(5), managed[1].JobModifyIndex)
	assert.Equal(t, "running", managed[1].Status)

	require.Len(t, unmanaged, 2)
	assert.Equal(t, "bare", unmanaged[0].ID)
	assert.Equal(t, "grafana", unmanaged[1].ID)
}

func TestJobsInvalidStageStaysManagedWithError(t *testing.T) {
	c, f := setup(t)
	f.addJob("storage", 1, map[string]string{"dcctl_stage": "1"}, false)
	f.addJob("late", 2, map[string]string{"dcctl_stage": "late"}, false)
	f.addJob("zero", 3, map[string]string{"dcctl_stage": "0"}, false)
	f.addJob("negative", 4, map[string]string{"dcctl_stage": "-1"}, false)
	f.addJob("grafana", 5, nil, false)

	managed, unmanaged, err := c.Jobs(t.Context())
	require.NoError(t, err, "a bad job is not a bad endpoint")

	require.Len(t, managed, 4)
	assert.Equal(t, "storage", managed[0].ID)
	assert.NoError(t, managed[0].StageErr)

	for i, id := range []string{"late", "negative", "zero"} {
		j := managed[i+1]
		assert.Equal(t, id, j.ID, "invalid stages come last, by ID")
		require.ErrorIs(t, j.StageErr, nomad.ErrInvalidStage)
		assert.Contains(t, j.StageErr.Error(), id)
		assert.Zero(t, j.Stage)
	}

	require.Len(t, unmanaged, 1)
	assert.Equal(t, "grafana", unmanaged[0].ID)
}

func TestAllocs(t *testing.T) {
	c, f := setup(t)
	f.allocs["engine"] = `[{
		"ID": "a1", "JobID": "engine", "TaskGroup": "app", "NodeID": "n1",
		"ClientStatus": "running", "DesiredStatus": "run",
		"TaskStates": {
			"web": {"State": "running", "Failed": false, "Restarts": 2, "StartedAt": "2026-11-20T10:00:00.5Z"},
			"sidecar": {"State": "dead", "Failed": true, "Restarts": 0, "StartedAt": "0001-01-01T00:00:00Z"}
		}
	}, {"ID": "a2", "JobID": "engine", "ClientStatus": "pending", "DesiredStatus": "run", "TaskStates": null}]`

	allocs, err := c.Allocs(t.Context(), "engine")
	require.NoError(t, err)
	require.Len(t, allocs, 2)

	a := allocs[0]
	assert.Equal(t, "a1", a.ID)
	assert.Equal(t, "app", a.TaskGroup)
	assert.Equal(t, "n1", a.NodeID)
	assert.Equal(t, "running", a.ClientStatus)
	assert.Equal(t, "run", a.DesiredStatus)
	require.Contains(t, a.TaskStates, "web")
	assert.Equal(t, "running", a.TaskStates["web"].State)
	assert.Equal(t, uint64(2), a.TaskStates["web"].Restarts)
	assert.True(t, a.TaskStates["web"].StartedAt.Equal(time.Date(2026, 11, 20, 10, 0, 0, 500_000_000, time.UTC)))
	assert.True(t, a.TaskStates["sidecar"].Failed)

	assert.Empty(t, allocs[1].TaskStates)
}

func TestNodes(t *testing.T) {
	c, f := setup(t)
	f.nodes = `[
		{"ID": "n1", "Name": "app-1", "Datacenter": "dc_a", "Status": "ready", "SchedulingEligibility": "eligible", "Drain": false},
		{"ID": "n2", "Name": "app-2", "Datacenter": "dc_a", "Status": "down", "SchedulingEligibility": "ineligible", "Drain": true}
	]`
	f.nodeMeta = map[string]map[string]string{"n1": {"services": "engine, storage ,freeswitch"}}

	nodes, err := c.Nodes(t.Context())
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	byID := map[string]nomad.Node{nodes[0].ID: nodes[0], nodes[1].ID: nodes[1]}

	assert.Equal(t, nomad.Node{
		ID: "n1", Name: "app-1", Datacenter: "dc_a", Status: "ready", SchedulingEligibility: "eligible",
		Services: []string{"engine", "storage", "freeswitch"},
	}, byID["n1"])
	assert.True(t, byID["n2"].Drain)
	assert.Equal(t, "ineligible", byID["n2"].SchedulingEligibility)
	assert.Empty(t, byID["n2"].Services)
}

func TestPlanRunsTheJobAsStarted(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.plan = `{"FailedTGAllocs": {"web": {"NodesEvaluated": 3}, "api": {}}, "JobModifyIndex": 5}`

	res, err := c.Plan(t.Context(), "engine")
	require.NoError(t, err)
	assert.Equal(t, []string{"api", "web"}, res.FailedTGAllocs)

	require.Len(t, f.plans, 1)
	assert.Equal(t, false, f.plans[0].Job["Stop"], "plan is for the job started, not the stopped one")

	f.plan = `{"FailedTGAllocs": null, "JobModifyIndex": 5}`
	res, err = c.Plan(t.Context(), "engine")
	require.NoError(t, err)
	assert.Empty(t, res.FailedTGAllocs)
}

func TestStopDoesNotPurge(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, false)

	require.NoError(t, c.Stop(t.Context(), "engine"))

	assert.Equal(t, []string{"engine?purge=false"}, f.deletes)
	assert.Contains(t, f.jobs, "engine", "the job stays registered")
	assert.Equal(t, true, f.jobs["engine"]["Stop"])
}

func TestStartRegistersCurrentVersionWithEnforceIndex(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)

	require.NoError(t, c.Start(t.Context(), "engine", nil))

	require.Len(t, f.registers, 1)
	req := f.registers[0]
	assert.True(t, req.EnforceIndex)
	assert.Equal(t, uint64(5), req.JobModifyIndex)
	assert.Equal(t, false, req.Job["Stop"])
	groups, _ := req.Job["TaskGroups"].([]any)
	require.Len(t, groups, 1)
	assert.Equal(t, "g", groups[0].(map[string]any)["Name"], "the job is registered as read")
}

func TestStartRetriesOnIndexConflict(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.beforeRegister = func(f *fakeNomad) { f.bumpIndex("engine") }

	require.NoError(t, c.Start(t.Context(), "engine", nil))

	require.Len(t, f.registers, 1, "the rejected attempt is not registered")
	assert.Equal(t, uint64(6), f.registers[0].JobModifyIndex, "retry carries the re-read index")
	assert.Equal(t, 2, f.gets, "job is re-read after the conflict")
	assert.Equal(t, false, f.jobs["engine"]["Stop"])
}

func TestStartChecksBeforeEveryAttempt(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.beforeRegister = func(f *fakeNomad) { f.bumpIndex("engine") }

	var checks int

	require.NoError(t, c.Start(t.Context(), "engine", func(context.Context) error {
		checks++

		return nil
	}))

	assert.Equal(t, 2, checks, "the retry after the conflict is checked too")
	assert.Len(t, f.registers, 1)
}

func TestStartFailedCheckRegistersNothing(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	first, second := newFakeNomad(), newFakeNomad()
	first.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	second.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	c := newClient(t, ca, startTLS(t, ca, first.handler()), startTLS(t, ca, second.handler()))

	errLost := errors.New("lease lost")

	err := c.Start(t.Context(), "engine", func(context.Context) error { return errLost })
	require.ErrorIs(t, err, errLost)

	_, classified := source.ReasonOf(err)
	assert.False(t, classified, "the caller's refusal is not an unreachable endpoint")
	assert.Empty(t, first.registers)
	assert.Zero(t, first.gets, "the job is not even read")
	assert.Zero(t, second.gets, "the refusal is not retried on another endpoint")
}

func TestStartStopsRetryingWhenTheJobIsGone(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.beforeRegister = func(f *fakeNomad) { delete(f.jobs, "engine") }

	err := c.Start(t.Context(), "engine", nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, nomad.ErrIndexConflict)

	se, ok := errors.AsType[*source.StatusError](err)
	require.True(t, ok)
	assert.Equal(t, http.StatusNotFound, se.Code)
	assert.Equal(t, 2, f.gets, "one read before the conflict, one after")
}

func TestStartGivesUpAfterBoundedAttempts(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	f := newFakeNomad()
	f.conflictAlways = true
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)

	c, err := nomad.New(nomad.Options{
		Datacenter: dc, Service: service, CAFile: ca.writeFile(t),
		Resolver:        &fakeResolver{addrs: map[string][]string{dc + "/" + service: {startTLS(t, ca, f.handler())}}},
		EnforceAttempts: 3,
	})
	require.NoError(t, err)

	err = c.Start(t.Context(), "engine", nil)
	require.ErrorIs(t, err, nomad.ErrIndexConflict)

	_, classified := source.ReasonOf(err)
	assert.False(t, classified, "a busy job is not an unreachable endpoint")
	assert.Equal(t, 3, f.gets)
	assert.Empty(t, f.registers)
}

func TestStartUnknownJob(t *testing.T) {
	c, _ := setup(t)

	err := c.Start(t.Context(), "missing", nil)
	require.Error(t, err)
	assert.Equal(t, source.ReasonHTTP, reasonOf(t, err))
}

func TestStopMissingJobSucceedsLikeNomad(t *testing.T) {
	c, f := setup(t)

	require.NoError(t, c.Stop(t.Context(), "missing"))
	assert.Equal(t, []string{"missing?purge=false"}, f.deletes)
}

func TestPlanErrorKeepsNomadMessage(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.planStatus = http.StatusBadRequest

	_, err := c.Plan(t.Context(), "engine")
	require.Error(t, err)
	assert.Equal(t, source.ReasonHTTP, reasonOf(t, err))
	assert.Contains(t, err.Error(), "group has no tasks")

	se, ok := errors.AsType[*source.StatusError](err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, se.Code)
}

func TestJobsOlderNomadIsInvalid(t *testing.T) {
	for _, version := range []string{"1.4.9", "0.12.0", "garbage", ""} {
		t.Run(version, func(t *testing.T) {
			c, f := setup(t)
			f.version = version
			f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, false)

			managed, unmanaged, err := c.Jobs(t.Context())
			require.Error(t, err)
			assert.Equal(t, source.ReasonInvalid, reasonOf(t, err))
			assert.Empty(t, managed)
			assert.Empty(t, unmanaged)
		})
	}
}

func TestJobsAcceptsSupportedVersions(t *testing.T) {
	for _, version := range []string{"1.5.0", "1.10.3+ent", "2.0.0-beta1", "v1.7.2"} {
		t.Run(version, func(t *testing.T) {
			c, f := setup(t)
			f.version = version
			f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, false)

			managed, _, err := c.Jobs(t.Context())
			require.NoError(t, err)
			assert.Len(t, managed, 1)
		})
	}
}

func TestJobIDIsOnePathSegment(t *testing.T) {
	c, f := setup(t)
	f.addJob("a/../b?x=1", 1, nil, false)

	require.NoError(t, c.Stop(t.Context(), "a/../b?x=1"))
	assert.Equal(t, []string{"a/../b?x=1?purge=false"}, f.deletes)
}

func TestStatusReadsMayBeStaleButStartReadIsConsistent(t *testing.T) {
	c, f := setup(t)
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.allocs["engine"] = "[]"
	f.nodes = `[{"ID": "n1"}]`

	_, _, err := c.Jobs(t.Context())
	require.NoError(t, err)
	_, err = c.Allocs(t.Context(), "engine")
	require.NoError(t, err)
	_, err = c.Nodes(t.Context())
	require.NoError(t, err)

	for _, route := range []string{"GET /v1/jobs", "GET /v1/job/engine/allocations", "GET /v1/nodes", "GET /v1/node/n1"} {
		assert.True(t, f.stale[route], route)
	}

	require.NoError(t, c.Start(t.Context(), "engine", nil))
	assert.False(t, f.stale["GET /v1/job/engine"], "the read Start registers from is consistent")
}

func TestConsistentReadsAskTheLeader(t *testing.T) {
	ca := newTestCA(t, "dc_a CA")
	f := newFakeNomad()
	f.addJob("engine", 5, map[string]string{"dcctl_stage": "3"}, true)
	f.allocs["engine"] = "[]"
	f.nodes = `[{"ID": "n1"}]`

	c, err := nomad.New(nomad.Options{
		Datacenter: dc, Service: service, CAFile: ca.writeFile(t),
		Resolver:        &fakeResolver{addrs: map[string][]string{dc + "/" + service: {startTLS(t, ca, f.handler())}}},
		ConsistentReads: true,
	})
	require.NoError(t, err)

	_, _, err = c.Jobs(t.Context())
	require.NoError(t, err)
	_, err = c.Allocs(t.Context(), "engine")
	require.NoError(t, err)
	_, err = c.Nodes(t.Context())
	require.NoError(t, err)

	for _, route := range []string{
		"GET /v1/agent/self", "GET /v1/jobs", "GET /v1/job/engine/allocations", "GET /v1/nodes", "GET /v1/node/n1",
	} {
		assert.False(t, f.stale[route], route)
	}
}

func TestAllocsIncludeEarlierJobsOfTheSameID(t *testing.T) {
	c, f := setup(t)
	f.allocs["engine"] = "[]"

	_, err := c.Allocs(t.Context(), "engine")
	require.NoError(t, err)
	assert.Contains(t, f.queries["GET /v1/job/engine/allocations"], "all=true")
}

func TestVersion(t *testing.T) {
	c, f := setup(t)
	f.version = "1.7.2+ent"

	version, err := c.Version(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "1.7.2+ent", version)
	assert.True(t, f.stale["GET /v1/agent/self"], "a status read may be answered by a follower")
}
