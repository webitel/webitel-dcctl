package apps_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

func readyNode(id string, services ...string) nomad.Node {
	return nomad.Node{ID: id, Name: id, Status: "ready", SchedulingEligibility: "eligible", Services: services}
}

// stoppedDC is a passive datacenter: freeswitch on two nodes in stage 1, engine in
// stage 2, opensips in stage 3, all stopped.
func stoppedDC() *fakeNomad {
	f := newFake()
	f.addNode(readyNode("n1", "freeswitch", "engine"))
	f.addNode(readyNode("n2", "freeswitch", "opensips"))
	f.addJob("freeswitch", "system", 1, false)
	f.addJob("engine", "service", 2, false)
	f.addJob("opensips", "service", 3, false)

	return f
}

func TestStartDatacenter_startsStagesInOrderOnceHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()
		f.addJob("webitel-core", "service", 2, true)

		var rec recorder

		res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow, Progress: rec.record})
		require.NoError(t, err)

		assert.Equal(t, apps.ActionStart, res.Action)
		require.Equal(t, []int{1, 2, 3}, stageNumbers(res))

		for _, s := range res.Stages {
			assert.Equal(t, apps.StageSucceeded, s.State)
			assert.Empty(t, s.Warnings())

			for _, j := range s.Jobs {
				assert.Equal(t, apps.JobHealthy, j.State, j.ID)
			}
		}

		assert.GreaterOrEqual(t, f.firstCall(t, "start", "engine").Sub(f.firstCall(t, "start", "freeswitch")),
			startDelay+cfg.MinHealthyTime, "stage 2 starts once stage 1 runs min_healthy_time")
		assert.GreaterOrEqual(t, f.firstCall(t, "start", "opensips").Sub(f.firstCall(t, "start", "engine")),
			startDelay+cfg.MinHealthyTime)
		assert.Empty(t, f.callsOf("start", "webitel-core"), "a running job is not registered again")
		assert.Equal(t, []apps.JobState{apps.JobPending, apps.JobStarting, apps.JobHealthy}, rec.jobStates("engine"))
	})
}

func TestStartDatacenter_stageTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()
		f.startDelays["engine"] = never

		var stage2 time.Time

		res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{
			Guard: allow,
			Progress: func(p apps.Progress) {
				if p.Stage.Number == 2 && stage2.IsZero() {
					stage2 = time.Now()
				}
			},
		})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.ErrorContains(t, err, "engine")
		assert.Equal(t, cfg.StageStartTimeout, time.Since(stage2), "each stage has its own stage_start_timeout")

		require.Equal(t, []int{1, 2}, stageNumbers(res))
		assert.Equal(t, apps.StageSucceeded, res.Stages[0].State)
		assert.Equal(t, apps.StageFailed, res.Stages[1].State)

		job := jobOf(t, res.Stages[1], "engine")
		assert.Equal(t, apps.JobUnhealthy, job.State)
		assert.Equal(t, "no allocation", job.Detail)
		assert.Empty(t, f.callsOf("start", "opensips"))
	})
}

func TestStartDatacenter_systemJobSkipsNodeOutOfService(t *testing.T) {
	tests := []struct {
		name    string
		node    nomad.Node
		warning string
	}{
		{
			"draining",
			nomad.Node{ID: "n3", Name: "n3", Status: "ready", SchedulingEligibility: "ineligible", Drain: true, Services: []string{"freeswitch"}},
			"freeswitch: node n3 is draining and has no healthy allocation",
		},
		{
			"down",
			nomad.Node{ID: "n3", Name: "n3", Status: "down", SchedulingEligibility: "eligible", Services: []string{"freeswitch"}},
			"freeswitch: node n3 is down and has no healthy allocation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := stoppedDC()
				f.addNode(tt.node)

				res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
				require.NoError(t, err)

				assert.Equal(t, apps.JobHealthy, jobOf(t, res.Stages[0], "freeswitch").State)
				assert.Equal(t, []string{tt.warning}, res.Stages[0].Warnings())
			})
		})
	}
}

func TestStartDatacenter_systemJobWithoutHealthyAllocFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addNode(nomad.Node{ID: "n1", Name: "n1", Status: "ready", SchedulingEligibility: "ineligible", Drain: true, Services: []string{"freeswitch"}})
		f.addJob("freeswitch", "system", 1, false)

		res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)

		job := jobOf(t, res.Stages[0], "freeswitch")
		assert.Equal(t, apps.JobUnhealthy, job.State)
		assert.Equal(t, "no healthy allocation: no allocation", job.Detail)
		assert.Equal(t, []string{"node n1 is draining and has no healthy allocation"}, job.Warnings)
	})
}

func TestStartDatacenter_restartRearmsMinHealthyTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("engine", "service", 2, false)
		f.addJob("call-center", "service", 2, false)
		f.startDelays["call-center"] = 30 * time.Second

		var rec recorder

		start := time.Now()
		done := background(func() (apps.Result, error) {
			return newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow, Progress: rec.record})
		})

		time.Sleep(35 * time.Second)
		f.restartTasks("engine")

		out := <-done
		require.NoError(t, out.err)
		assert.GreaterOrEqual(t, time.Since(start), 35*time.Second+cfg.MinHealthyTime,
			"the stage waits until the restarted job runs min_healthy_time again")
		assert.Equal(t, []apps.JobState{
			apps.JobPending, apps.JobStarting, apps.JobHealthy, apps.JobUnhealthy, apps.JobHealthy,
		}, rec.jobStates("engine"))
	})
}

func TestStartDatacenter_startsAgainAJobStoppedDuringStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("engine", "service", 2, false)
		f.addJob("call-center", "service", 2, false)
		f.startDelays["call-center"] = 30 * time.Second

		done := background(func() (apps.Result, error) {
			return newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		})

		time.Sleep(15 * time.Second)
		f.stopBehindBack("engine")

		out := <-done
		require.NoError(t, out.err)
		assert.Len(t, f.callsOf("start", "engine"), 2)
		assert.Equal(t, apps.JobHealthy, jobOf(t, out.res.Stages[0], "engine").State)
	})
}

func TestStartDatacenter_refusesInvalidStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()
		f.addInvalidJob("broken", false)

		guarded := false
		guard := func(context.Context) error {
			guarded = true

			return nil
		}

		res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: guard})
		require.ErrorIs(t, err, nomad.ErrInvalidStage)
		assert.ErrorContains(t, err, "broken")
		assert.Empty(t, res.Stages)
		assert.Zero(t, f.callCount(), "nothing is started")
		assert.False(t, guarded)
	})
}

func TestStartDatacenter_guardFailureEndsBeforeNextStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()

		guard := func(context.Context) error {
			if !f.isStopped("freeswitch") {
				return errLost
			}

			return nil
		}

		res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: guard})
		require.ErrorIs(t, err, errLost)
		require.ErrorIs(t, err, apps.ErrGuard)

		require.Equal(t, []int{1, 2}, stageNumbers(res))
		assert.Equal(t, apps.StageFailed, res.Stages[1].State)
		assert.Equal(t, apps.JobPending, jobOf(t, res.Stages[1], "engine").State)
		assert.Empty(t, f.callsOf("start", "engine"))
	})
}

func TestStartDatacenter_guardRefusesStartAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()

		calls := 0
		guard := func(context.Context) error {
			calls++
			if calls > 1 {
				return errLost
			}

			return nil
		}

		res, err := newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: guard})
		require.ErrorIs(t, err, errLost)
		require.ErrorIs(t, err, apps.ErrGuard)
		assert.Equal(t, 2, calls, "the guard runs before the stage and before the start attempt")
		assert.Zero(t, f.callCount(), "the refused attempt registers nothing")
		require.Len(t, res.Stages, 1)
		assert.Equal(t, apps.StageFailed, res.Stages[0].State)
	})
}

func TestStartDatacenter_readErrorsAreNotSuccess(t *testing.T) {
	tests := []struct {
		name string
		fail func(f *fakeNomad)
	}{
		{"job_list", func(f *fakeNomad) { f.jobsErr = errDown }},
		{"allocations", func(f *fakeNomad) { f.allocsErr["freeswitch"] = errDown }},
		{"nodes", func(f *fakeNomad) { f.nodesErr = errDown }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := stoppedDC()

				done := background(func() (apps.Result, error) {
					return newApps(t).StartDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
				})

				synctest.Wait()
				f.set(tt.fail)

				out := <-done
				require.ErrorIs(t, out.err, apps.ErrTimeout, "freeswitch runs, but dcctl could not see it")

				job := jobOf(t, out.res.Stages[0], "freeswitch")
				assert.Equal(t, apps.JobUnknown, job.State)
				require.ErrorIs(t, job.Err, errDown)
			})
		})
	}
}
