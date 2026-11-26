package preflight

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

const runTimeout = 6 * time.Second

type fakeCollector struct {
	snap *collect.Snapshot
	err  error
	// hang makes Collect wait for its context.
	hang bool
}

func (f *fakeCollector) Collect(ctx context.Context) (*collect.Snapshot, error) {
	if f.hang {
		<-ctx.Done()

		return nil, ctx.Err()
	}

	return f.snap, f.err
}

type fakePlanner struct {
	mu      sync.Mutex
	results map[string]nomad.PlanResult
	errs    map[string]error
	// hang makes Plan wait for its context.
	hang    bool
	running atomic.Int32
	peak    atomic.Int32
	calls   []string
}

func (f *fakePlanner) Plan(ctx context.Context, jobID string) (nomad.PlanResult, error) {
	n := f.running.Add(1)
	defer f.running.Add(-1)

	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, jobID)
	res, err := f.results[jobID], f.errs[jobID]
	f.mu.Unlock()

	if f.hang {
		<-ctx.Done()

		return nomad.PlanResult{}, ctx.Err()
	}

	return res, err
}

func newRunner(c Collector, planners map[string]Planner, last *collect.Snapshot) *Runner {
	return New(Config{Status: testConfig(), Timeout: runTimeout}, c, planners,
		func() *collect.Snapshot { return last })
}

func switchToB() Request {
	return Request{Action: Switch, Params: Params{Datacenter: dcB}}
}

func TestRun_allowsHealthySwitch(t *testing.T) {
	p := &fakePlanner{}
	r := newRunner(&fakeCollector{snap: warm()}, map[string]Planner{dcB: p}, nil)

	v, err := r.Run(t.Context(), switchToB())
	require.NoError(t, err)

	assert.Equal(t, check.Allowed, v.Outcome, "failed: %v", v.Failed)
	assert.ElementsMatch(t, []string{"engine", "freeswitch"}, p.calls, "every managed job of the target is planned")
}

func TestEvaluate_returnsTheStateOfTheDecision(t *testing.T) {
	r := newRunner(&fakeCollector{snap: warm()}, map[string]Planner{dcB: &fakePlanner{}}, nil)

	res, err := r.Evaluate(t.Context(), switchToB())
	require.NoError(t, err)

	assert.Equal(t, check.Allowed, res.Verdict.Outcome, "failed: %v", res.Verdict.Failed)
	require.NotNil(t, res.Status.View.Effective)
	assert.Equal(t, dcA, res.Status.View.Effective.State.ActiveDatacenter)
	assert.Len(t, res.Status.Datacenters, 2)
}

// The loop still holds what dc_b showed before its Nomad stopped answering; preflight
// decides on what it collects itself, so the switch is blocked.
func TestRun_unreachableSourceFailsDespiteLastKnownData(t *testing.T) {
	fresh := warm()
	cutNomad(fresh, dcB)

	p := &fakePlanner{}
	r := newRunner(&fakeCollector{snap: fresh}, map[string]Planner{dcB: p}, warm())

	v, err := r.Run(t.Context(), switchToB())
	require.NoError(t, err)

	require.Equal(t, check.Blocked, v.Outcome)

	for _, id := range []check.ID{
		check.NomadReachable, check.NomadStagesValid, check.NomadTargetJobsPresent, check.NomadTargetJobsStopped,
		check.NomadTargetPlan,
	} {
		assert.Equal(t, check.Fail, find(t, v.Checks, id, dcB).Result, id)
	}

	assert.Equal(t, check.Warn, find(t, v.Checks, check.NomadTargetNodes, dcB).Result)
	assert.Empty(t, p.calls, "nothing to plan in a Nomad that does not answer")
}

func TestRun_plans(t *testing.T) {
	tests := []struct {
		name     string
		planner  *fakePlanner
		planners func(p Planner) map[string]Planner
		want     check.Result
	}{
		{name: "placed", planner: &fakePlanner{}, want: check.Pass},
		{name: "not_placed", planner: &fakePlanner{
			results: map[string]nomad.PlanResult{"engine": {FailedTGAllocs: []string{"api"}}},
		}, want: check.Fail},
		{name: "plan_error", planner: &fakePlanner{errs: map[string]error{"freeswitch": errors.New("500")}}, want: check.Fail},
		{name: "no_planner", planner: &fakePlanner{}, planners: func(Planner) map[string]Planner { return nil }, want: check.Fail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planners := map[string]Planner{dcB: tt.planner}
			if tt.planners != nil {
				planners = tt.planners(tt.planner)
			}

			v, err := newRunner(&fakeCollector{snap: warm()}, planners, nil).Run(t.Context(), switchToB())
			require.NoError(t, err)

			c := find(t, v.Checks, check.NomadTargetPlan, dcB)
			assert.Equal(t, tt.want, c.Result, c.Detail)
		})
	}
}

func TestRun_appsSyncPlansStoppedJobsOfActive(t *testing.T) {
	s := warm()
	s.Observer = collect.Instance{Datacenter: dcA, Node: "dcctl-" + dcA}
	setNomad(s, dcA, func(n *collect.Nomad) {
		n.Managed[0].Stop = true
		n.Allocs = map[string][]nomad.Alloc{"freeswitch": n.Allocs["freeswitch"]}
	})

	p := &fakePlanner{}
	v, err := newRunner(&fakeCollector{snap: s}, map[string]Planner{dcA: p}, nil).Run(t.Context(), Request{Action: AppsSync})
	require.NoError(t, err)

	assert.Equal(t, []string{"engine"}, p.calls)
	assert.Equal(t, check.Pass, find(t, v.Checks, check.NomadActivePlan, dcA).Result)
}

func TestRun_hungPlanFailsWithinTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		begin := time.Now()

		v, err := newRunner(&fakeCollector{snap: warm()}, map[string]Planner{dcB: &fakePlanner{hang: true}}, nil).
			Run(t.Context(), switchToB())
		require.NoError(t, err)

		assert.Equal(t, runTimeout, time.Since(begin))
		assert.Equal(t, check.Fail, find(t, v.Checks, check.NomadTargetPlan, dcB).Result)
		assert.Equal(t, check.Blocked, v.Outcome)
	})
}

func TestRun_collectionDoesNotFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, err := newRunner(&fakeCollector{hang: true}, nil, nil).Run(t.Context(), switchToB())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestRun_canceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		_, err := newRunner(&fakeCollector{snap: warm()}, map[string]Planner{dcB: &fakePlanner{hang: true}}, nil).
			Run(ctx, switchToB())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestRun_unknownAction(t *testing.T) {
	_, err := newRunner(&fakeCollector{snap: warm()}, nil, nil).Run(t.Context(), Request{Action: "rejoin"})
	require.ErrorIs(t, err, ErrUnknownAction)
}

func TestRun_failoverFactsFromLastKnown(t *testing.T) {
	p := &fakePlanner{}
	r := newRunner(&fakeCollector{snap: isolatedA()}, map[string]Planner{dcB: p}, warm())

	v, err := r.Run(t.Context(), Request{
		Action: Failover, Params: Params{Datacenter: dcB}, Confirmations: check.Confirmations{Isolation: true},
	})
	require.NoError(t, err)

	assert.Equal(t, check.Allowed, v.Outcome, "failed: %v", v.Failed)

	old := find(t, v.Checks, check.CoordIsolation, "").Facts["datacenters"].([]any)
	require.Len(t, old, 1)
	assert.Contains(t, old[0], "last_seen")
}

func TestRun_plansInParallelWithinLimit(t *testing.T) {
	s := warm()
	setNomad(s, dcB, func(n *collect.Nomad) {
		n.Managed = nil
		n.Allocs = map[string][]nomad.Alloc{}

		for i := range 3 * planParallelism {
			id := fmt.Sprintf("job-%d", i)
			n.Managed = append(n.Managed, nomad.Job{ID: id, Name: id, Type: "service", Stage: 1, Stop: true})
		}
	})

	p := &fakePlanner{}
	v, err := newRunner(&fakeCollector{snap: s}, map[string]Planner{dcB: p}, nil).Run(t.Context(), switchToB())
	require.NoError(t, err)

	assert.Len(t, p.calls, 3*planParallelism)
	assert.LessOrEqual(t, p.peak.Load(), int32(planParallelism))
	assert.Equal(t, 3*planParallelism, find(t, v.Checks, check.NomadTargetPlan, dcB).Facts["planned"])
}

func TestConfigFrom(t *testing.T) {
	cfg := &config.Serve{Status: config.Status{RequestTimeout: 3 * time.Second}}

	assert.Equal(t, 6*time.Second, ConfigFrom(cfg).Timeout)
}
