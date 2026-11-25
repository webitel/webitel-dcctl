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

// remoteGuard is a guard that asks a remote store and takes d to answer.
func remoteGuard(d time.Duration) apps.Guard {
	return func(ctx context.Context) error {
		t := time.NewTimer(d)
		defer t.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}

// slowAfter makes the reads or the stops of the fake hang until their context ends,
// once the given time has passed.
type slowAfter struct {
	*fakeNomad

	from      time.Time
	slowJobs  bool
	slowStops bool
}

func (s slowAfter) hang(ctx context.Context, on bool) error {
	if !on || time.Now().Before(s.from) {
		return nil
	}

	<-ctx.Done()

	return ctx.Err()
}

func (s slowAfter) Jobs(ctx context.Context) (managed, unmanaged []nomad.Job, err error) {
	if err := s.hang(ctx, s.slowJobs); err != nil {
		return nil, nil, err
	}

	return s.fakeNomad.Jobs(ctx)
}

func (s slowAfter) Stop(ctx context.Context, jobID string) error {
	if err := s.hang(ctx, s.slowStops); err != nil {
		return err
	}

	return s.fakeNomad.Stop(ctx, jobID)
}

// slowNodes answers the node list after d.
type slowNodes struct {
	*fakeNomad

	d time.Duration
}

func (s slowNodes) Nodes(ctx context.Context) ([]nomad.Node, error) {
	time.Sleep(s.d)

	return s.fakeNomad.Nodes(ctx)
}

// noopStart answers every start after the check and changes nothing.
type noopStart struct{ *fakeNomad }

func (s noopStart) Start(ctx context.Context, _ string, check func(context.Context) error) error {
	return check(ctx)
}

func TestStopDatacenterFailover_guardCutByDeadlineIsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopNoop = true

		_, err := newApps(t).StopDatacenterFailover(t.Context(), f.dc(), apps.Options{Guard: remoteGuard(1500 * time.Millisecond)})
		require.ErrorIs(t, err, apps.ErrTimeout, "the failover goes on after a timeout")
		assert.NotErrorIs(t, err, apps.ErrGuard, "a refusal would abort the failover")
	})
}

func TestStartDatacenter_startCheckCutByDeadlineIsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("engine", "service", 2, false)

		dc := apps.Datacenter{Name: dcName, Nomad: noopStart{f}}

		_, err := newApps(t).StartDatacenter(t.Context(), dc, apps.Options{Guard: remoteGuard(3 * time.Second)})
		require.ErrorIs(t, err, apps.ErrTimeout)
		assert.NotErrorIs(t, err, apps.ErrGuard)
	})
}

func TestStopDatacenter_readCutByDeadlineKeepsLastObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopDelays["opensips"] = never

		dc := apps.Datacenter{Name: dcName, Nomad: slowAfter{fakeNomad: f, from: time.Now().Add(110 * time.Second), slowJobs: true}}

		res, err := newApps(t).StopDatacenter(t.Context(), dc, apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)

		job := jobOf(t, res.Stages[0], "opensips")
		assert.Equal(t, apps.JobStopping, job.State, "a read cut by the deadline says nothing about the job")
		require.NoError(t, job.Err)
	})
}

func TestStopDatacenter_writeCutByDeadlineLeavesNoError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.stopNoop = true

		dc := apps.Datacenter{Name: dcName, Nomad: slowAfter{fakeNomad: f, from: time.Now().Add(110 * time.Second), slowStops: true}}

		res, err := newApps(t).StopDatacenter(t.Context(), dc, apps.Options{Guard: allow})
		require.ErrorIs(t, err, apps.ErrTimeout)

		job := jobOf(t, res.Stages[0], "opensips")
		assert.Equal(t, apps.JobStopping, job.State)
		require.NoError(t, job.Err, "a stop cut by the deadline is not a failure of the job")
	})
}

func TestStartDatacenter_slowReadDoesNotHideARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addNode(readyNode("n1", "freeswitch"))
		f.addJob("freeswitch", "system", 1, false)

		var healthyAt time.Time

		dc := apps.Datacenter{Name: dcName, Nomad: slowNodes{fakeNomad: f, d: 9 * time.Second}}
		done := background(func() (apps.Result, error) {
			return newApps(t).StartDatacenter(t.Context(), dc, apps.Options{
				Guard: allow,
				Progress: func(p apps.Progress) {
					if healthyAt.IsZero() && p.Stage.Jobs[0].State == apps.JobHealthy {
						healthyAt = time.Now()
					}
				},
			})
		})

		for len(f.callsOf("start", "freeswitch")) == 0 {
			time.Sleep(100 * time.Millisecond)
		}

		time.Sleep(time.Until(f.firstCall(t, "start", "freeswitch").Add(startDelay + 5*time.Second)))
		f.restartTasks("freeswitch")

		restartedAt := time.Now()

		out := <-done
		require.NoError(t, out.err)
		assert.GreaterOrEqual(t, healthyAt.Sub(restartedAt), cfg.MinHealthyTime,
			"the task restarted inside the window runs min_healthy_time before it counts")
	})
}
