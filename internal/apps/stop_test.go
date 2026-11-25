package apps_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/apps"
)

func TestNew_rejectsBadConfig(t *testing.T) {
	bad := cfg
	bad.MinHealthyTime = 0

	_, err := apps.New(bad, nil)
	require.Error(t, err)
}

func TestStopDatacenter_needsGuard(t *testing.T) {
	f := newFake()
	f.addJob("opensips", "service", 3, true)

	_, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{})
	require.Error(t, err)
	assert.Zero(t, f.callCount())
}

func TestStopDatacenter_stopsStagesInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("webitel-core-api", "service", 3, true)
		f.addJob("engine", "service", 2, true)
		f.addJob("freeswitch", "system", 1, true)
		f.addJob("rtpengine", "system", 1, false)
		f.stopDelays["webitel-core-api"] = 7 * time.Second

		var rec recorder

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow, Progress: rec.record})
		require.NoError(t, err)

		assert.Equal(t, dcName, res.Datacenter)
		assert.Equal(t, apps.ActionStop, res.Action)
		require.Equal(t, []int{3, 2, 1}, stageNumbers(res))

		for _, s := range res.Stages {
			assert.Equal(t, apps.StageSucceeded, s.State)

			for _, j := range s.Jobs {
				assert.Equal(t, apps.JobStopped, j.State, j.ID)
			}
		}

		stage3 := f.firstCall(t, "stop", "opensips")
		assert.Equal(t, stage3, f.firstCall(t, "stop", "webitel-core-api"), "jobs of a stage stop together")
		assert.GreaterOrEqual(t, f.firstCall(t, "stop", "engine").Sub(stage3), 7*time.Second,
			"the next stage waits for the slowest job of the previous one")
		assert.GreaterOrEqual(t, f.firstCall(t, "stop", "freeswitch").Sub(f.firstCall(t, "stop", "engine")), stopDelay)
		assert.Empty(t, f.callsOf("stop", "rtpengine"), "a stopped job is not stopped again")

		reports := rec.all()
		require.NotEmpty(t, reports)
		assert.Equal(t, 3, reports[0].Stage.Number)
		assert.Equal(t, apps.StageRunning, reports[0].Stage.State)
		assert.Equal(t, apps.JobPending, jobOf(t, reports[0].Stage, "opensips").State)
		assert.Equal(t, []apps.JobState{apps.JobPending, apps.JobStopping, apps.JobStopped}, rec.jobStates("opensips"))

		last := reports[len(reports)-1]
		assert.Equal(t, 1, last.Stage.Number)
		assert.Equal(t, apps.StageSucceeded, last.Stage.State)
		assert.Equal(t, dcName, last.Datacenter)
		assert.Equal(t, apps.ActionStop, last.Action)
	})
}

func TestStopDatacenter_noManagedJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		res, err := newApps(t).StopDatacenter(t.Context(), newFake().dc(), apps.Options{Guard: allow})
		require.NoError(t, err)
		assert.Empty(t, res.Stages)
	})
}

func TestStopDatacenter_deregisterWithoutEffectIsNotDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopNoop = true

		start := time.Now()

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.Equal(t, cfg.StopTimeout, time.Since(start))

		require.Len(t, res.Stages, 1)
		assert.Equal(t, apps.StageFailed, res.Stages[0].State)
		job := jobOf(t, res.Stages[0], "opensips")
		assert.Equal(t, apps.JobStopping, job.State)
		assert.Greater(t, len(f.callsOf("stop", "opensips")), 1, "a job still not stopped is stopped again")
	})
}

func TestStopDatacenter_activeAllocsAreNotDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("engine", "service", 2, true)
		f.stopDelays["opensips"] = never

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.ErrorContains(t, err, "opensips")

		require.Equal(t, []int{3}, stageNumbers(res), "the next stage does not start")
		job := jobOf(t, res.Stages[0], "opensips")
		assert.Equal(t, apps.JobStopping, job.State, "the last observation is kept, not replaced at the deadline")
		assert.Contains(t, job.Detail, "1 allocations active")
		require.NoError(t, job.Err)
		assert.Len(t, f.callsOf("stop", "opensips"), 1, "a job with Stop set is waited for, not stopped again")
		assert.Empty(t, f.callsOf("stop", "engine"))
	})
}

func TestStopDatacenterFailover_givesUpAfterFailoverTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopDelays["opensips"] = never

		start := time.Now()

		_, err := newApps(t).StopDatacenterFailover(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.Equal(t, cfg.FailoverStopTimeout, time.Since(start))
	})
}

func TestStopDatacenter_timeoutCoversEveryStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("engine", "service", 2, true)
		f.stopDelays["opensips"] = 100 * time.Second
		f.stopDelays["engine"] = 100 * time.Second

		start := time.Now()

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.Equal(t, cfg.StopTimeout, time.Since(start), "stop_timeout bounds all stages together")
		assert.Equal(t, []int{3, 2}, stageNumbers(res))
	})
}

func TestStopDatacenter_stopsAJobThatRunsAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("nginx-vip", "service", 3, true)
		f.stopDelays["nginx-vip"] = 20 * time.Second

		var rec recorder

		done := background(func() (apps.Result, error) {
			return newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow, Progress: rec.record})
		})

		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.True(t, f.isStopped("opensips"))
		f.runAgain("opensips")

		out := <-done
		require.NoError(t, out.err)
		assert.Len(t, f.callsOf("stop", "opensips"), 2, "the job that runs again is stopped again")
		assert.Equal(t, []apps.JobState{
			apps.JobPending, apps.JobStopping, apps.JobStopped, apps.JobStopping, apps.JobStopped,
		}, rec.jobStates("opensips"))
	})
}

func TestStopDatacenter_stopsJobsThatRunAfterTheirStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("freeswitch", "service", 1, true)
		f.stopDelays["freeswitch"] = 20 * time.Second

		done := background(func() (apps.Result, error) {
			return newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		})

		time.Sleep(10 * time.Second)
		f.runAgain("opensips")
		f.addJob("nginx-vip", "service", 3, true)

		out := <-done
		require.NoError(t, out.err)
		require.Equal(t, []int{3, 1, 3}, stageNumbers(out.res), "the jobs running after the stages get another pass")

		last := out.res.Stages[2]
		assert.Equal(t, apps.JobStopped, jobOf(t, last, "opensips").State)
		assert.Equal(t, apps.JobStopped, jobOf(t, last, "nginx-vip").State)
		assert.True(t, f.isStopped("nginx-vip"))
		assert.Len(t, f.callsOf("stop", "opensips"), 2)
	})
}

func TestStopDatacenter_guardFailureEndsBeforeNextStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("engine", "service", 2, true)

		guard := func(context.Context) error {
			if f.isStopped("opensips") {
				return errLost
			}

			return nil
		}

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: guard})
		require.ErrorIs(t, err, errLost)
		require.ErrorIs(t, err, apps.ErrGuard)

		require.Equal(t, []int{3, 2}, stageNumbers(res), "the refused stage shows where the stop ended")
		assert.Equal(t, apps.StageSucceeded, res.Stages[0].State)
		assert.Equal(t, apps.StageFailed, res.Stages[1].State)
		assert.Equal(t, apps.JobPending, jobOf(t, res.Stages[1], "engine").State)
		assert.Empty(t, f.callsOf("stop", "engine"), "stage 2 is not started")
	})
}

func TestStopDatacenter_guardRefusesStageWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)

		calls := 0
		guard := func(context.Context) error {
			calls++
			if calls > 1 {
				return errLost
			}

			return nil
		}

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: guard})
		require.ErrorIs(t, err, errLost)
		assert.Zero(t, f.callCount(), "nothing is stopped once the guard refuses")
		require.Len(t, res.Stages, 1)
		assert.Equal(t, apps.StageFailed, res.Stages[0].State)
	})
}

func TestStopDatacenter_invalidStageStopsFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("engine", "service", 2, true)
		f.addInvalidJob("broken", true)

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.NoError(t, err)

		require.Equal(t, []int{3, 2}, stageNumbers(res))
		assert.Equal(t, apps.JobStopped, jobOf(t, res.Stages[0], "broken").State)
		assert.Equal(t, f.firstCall(t, "stop", "opensips"), f.firstCall(t, "stop", "broken"))
	})
}

func TestStopDatacenter_onlyInvalidStages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addInvalidJob("broken", true)

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.NoError(t, err)
		assert.Equal(t, []int{1}, stageNumbers(res))
		assert.True(t, f.isStopped("broken"))
	})
}

func TestStopDatacenter_readErrorsAreNotSuccess(t *testing.T) {
	tests := []struct {
		name string
		fail func(f *fakeNomad)
	}{
		{"job_list", func(f *fakeNomad) { f.jobsErr = errDown }},
		{"allocations", func(f *fakeNomad) { f.allocsErr["opensips"] = errDown }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFake()
				f.addJob("opensips", "service", 3, true)

				done := background(func() (apps.Result, error) {
					return newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
				})

				synctest.Wait()
				f.set(tt.fail)

				out := <-done
				require.ErrorIs(t, out.err, apps.ErrTimeout, "the job is stopped in Nomad, but dcctl could not see it")

				job := jobOf(t, out.res.Stages[0], "opensips")
				assert.Equal(t, apps.JobUnknown, job.State)
				require.ErrorIs(t, job.Err, errDown)
			})
		})
	}
}

func TestStopDatacenter_stopsEvenWhileAllocsAreUnreadable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.allocsErr["opensips"] = errDown

		res, err := newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.Len(t, f.callsOf("stop", "opensips"), 1, "a job listed as running is stopped at once")
		assert.Equal(t, apps.JobUnknown, jobOf(t, res.Stages[0], "opensips").State)
	})
}

func TestStopDatacenter_readErrorsAreRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopDelays["opensips"] = 50 * time.Second
		f.jobsErr = errDown

		var rec recorder

		done := background(func() (apps.Result, error) {
			return newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow, Progress: rec.record})
		})

		time.Sleep(20 * time.Second)
		f.set(func(f *fakeNomad) { f.jobsErr = nil })

		time.Sleep(20 * time.Second)
		f.set(func(f *fakeNomad) { f.allocsErr["opensips"] = errDown })

		time.Sleep(20 * time.Second)
		f.set(func(f *fakeNomad) { delete(f.allocsErr, "opensips") })

		out := <-done
		require.NoError(t, out.err)
		assert.Contains(t, rec.jobStates("opensips"), apps.JobUnknown)
		assert.Equal(t, apps.JobStopped, jobOf(t, out.res.Stages[0], "opensips").State)
	})
}

func TestStopDatacenter_purgedJobStopsWithItsAllocs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopDelays["opensips"] = never

		var rec recorder

		done := background(func() (apps.Result, error) {
			return newApps(t).StopDatacenter(t.Context(), f.dc(), apps.Options{Guard: allow, Progress: rec.record})
		})

		time.Sleep(5 * time.Second)
		f.purge("opensips")
		time.Sleep(10 * time.Second)
		synctest.Wait()

		reports := rec.all()
		require.NotEmpty(t, reports)
		assert.Equal(t, apps.JobStopping, jobOf(t, reports[len(reports)-1].Stage, "opensips").State,
			"a job gone from the list with allocations running is not stopped")

		f.completeAllocs("opensips")

		out := <-done
		require.NoError(t, out.err)

		job := jobOf(t, out.res.Stages[0], "opensips")
		assert.Equal(t, apps.JobStopped, job.State)
		assert.Equal(t, []string{"job is no longer registered"}, job.Warnings)
		assert.Equal(t, []string{"opensips: job is no longer registered"}, out.res.Stages[0].Warnings())
	})
}

func TestStopDatacenter_cancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopDelays["opensips"] = never

		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(5*time.Second, cancel)

		_, err := newApps(t).StopDatacenter(ctx, f.dc(), apps.Options{Guard: allow})
		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, apps.ErrTimeout)
	})
}
