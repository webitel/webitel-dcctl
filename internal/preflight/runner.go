package preflight

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/status"
)

const (
	// runRequests is how many status.request_timeout one run takes: the collection,
	// then the plans.
	runRequests = 2
	// planParallelism bounds the plans running at once.
	planParallelism = 8
)

// Collector collects a snapshot of its own; [collect.Collector] with consistent reads is
// one.
type Collector interface {
	Collect(ctx context.Context) (*collect.Snapshot, error)
}

// Planner dry-runs the registration of a managed job in one datacenter;
// [nomad.Client] is one.
type Planner interface {
	Plan(ctx context.Context, jobID string) (nomad.PlanResult, error)
}

// Config configures a [Runner].
type Config struct {
	Status status.Config
	// Timeout bounds one run, the collection and the plans together.
	Timeout time.Duration
}

// ConfigFrom returns the config of the runner of serve.
func ConfigFrom(cfg *config.Serve) Config {
	return Config{Status: status.ConfigFrom(cfg), Timeout: runRequests * cfg.Status.RequestTimeout}
}

// Runner runs the checks of an action on data it collects for the action.
type Runner struct {
	cfg       Config
	collector Collector
	planners  map[string]Planner
	last      func() *collect.Snapshot
}

// New returns a runner. planners has the planner of every datacenter. last returns the
// last snapshot of the collection loop, or nil; only the facts of coord.isolation come
// from it.
func New(cfg Config, collector Collector, planners map[string]Planner, last func() *collect.Snapshot) *Runner {
	return &Runner{cfg: cfg, collector: collector, planners: planners, last: last}
}

// Request is an action whose checks are run.
type Request struct {
	Action        Action
	Params        Params
	Confirmations check.Confirmations
}

// Run collects every source anew, plans the managed jobs the action starts and decides
// the checks of the action with its confirmations. A source that does not answer
// within the timeout fails the checks that need it. Run fails when ctx ends, when the
// collection does not finish within the timeout, or for an unknown action.
func (r *Runner) Run(ctx context.Context, req Request) (check.Verdict, error) {
	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	snap, err := r.collector.Collect(runCtx)
	if err != nil {
		return check.Verdict{}, fmt.Errorf("collect: %w", err)
	}

	in := &Input{
		Action: req.Action, Params: req.Params, Snapshot: snap,
		Status: status.Build(snap, r.cfg.Status, time.Now()), Config: r.cfg.Status,
		LastKnown: r.last(),
	}

	in.Plans = r.plan(runCtx, in.planJobs())

	if err := ctx.Err(); err != nil {
		return check.Verdict{}, err
	}

	checks, err := Checks(in)
	if err != nil {
		return check.Verdict{}, err
	}

	return check.Decide(checks, req.Confirmations), nil
}

// plan plans the jobs of every datacenter at once. A plan that fails or does not finish
// in time keeps its error, which fails its check.
func (r *Runner) plan(ctx context.Context, jobs map[string][]string) map[string]map[string]Plan {
	out := make(map[string]map[string]Plan, len(jobs))
	for dc, ids := range jobs {
		out[dc] = make(map[string]Plan, len(ids))
	}

	var (
		mu sync.Mutex
		g  errgroup.Group
	)

	g.SetLimit(planParallelism)

	set := func(dc, id string, p Plan) {
		mu.Lock()
		defer mu.Unlock()

		out[dc][id] = p
	}

	for dc, ids := range jobs {
		p, ok := r.planners[dc]

		for _, id := range ids {
			if !ok {
				set(dc, id, Plan{Err: fmt.Errorf("no Nomad planner for %s", dc)})

				continue
			}

			g.Go(func() error {
				res, err := p.Plan(ctx, id)
				set(dc, id, Plan{Result: res, Err: err})

				return nil
			})
		}
	}

	_ = g.Wait() // every plan keeps its own error

	return out
}
