// Package apps stops and starts the managed Nomad jobs of a datacenter by stages.
//
// A job is managed when it has meta.dcctl_stage. Stop goes from the highest stage down,
// start from the lowest up; the jobs of a stage are handled together and the next stage
// begins once every job of the stage meets the condition:
//
//   - stopped: Stop is set and no allocation is pending, running or unknown;
//   - healthy service job: an allocation runs with every task running for at least
//     apps.min_healthy_time;
//   - healthy system job: such an allocation on every node that is ready, eligible, not
//     draining and has the service in meta.services. A node out of scheduling without
//     one is a warning; no healthy allocation at all keeps the job unhealthy.
//
// A job with an invalid meta.dcctl_stage is stopped with the first stop stage, and
// start refuses to run while there is one.
//
// The state is polled. A read that fails leaves the job unknown, never done, and is
// retried until the deadline. A job that leaves the condition while its stage runs is
// stopped or started again.
package apps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

const (
	pollInterval = 2 * time.Second
	// readTimeout bounds the reads of one poll; writeTimeout bounds one round of stops or
	// starts, a start with its conflict retries included.
	readTimeout  = 15 * time.Second
	writeTimeout = 30 * time.Second
	// parallelism bounds the job reads, or the stops or starts, of a stage running at
	// once; the node read runs beside the job reads with its own parallel reads.
	parallelism = 8
)

var (
	// ErrTimeout means a stage did not reach its condition before its deadline.
	ErrTimeout = errors.New("apps did not reach the condition in time")
	// ErrGuard wraps the error of the [Guard] that ended a primitive.
	ErrGuard = errors.New("guard refused")
)

// Nomad is the Nomad cluster of one datacenter. Its reads must be consistent: a
// follower cut off from the leader may show a running job as stopped. [*nomad.Client]
// built with [nomad.Options.ConsistentReads] implements it.
type Nomad interface {
	Jobs(ctx context.Context) (managed, unmanaged []nomad.Job, err error)
	Allocs(ctx context.Context, jobID string) ([]nomad.Alloc, error)
	Nodes(ctx context.Context) ([]nomad.Node, error)
	Stop(ctx context.Context, jobID string) error
	Start(ctx context.Context, jobID string, check func(context.Context) error) error
}

// Datacenter is the datacenter whose apps a primitive stops or starts.
type Datacenter struct {
	Name  string
	Nomad Nomad
}

// Guard confirms that the caller may still change the apps, for example that it holds
// the lease of the current generation. It runs before each stage, before each round of
// stops and before each start attempt; an error ends the primitive at once with
// [ErrGuard]. It is called from up to eight starts at once, so it must be safe for
// concurrent use. An error of ctx should be returned as it is: a guard cut by the
// deadline ends the primitive as a timeout, not as a refusal.
type Guard func(ctx context.Context) error

// Options are the caller's part of a primitive.
type Options struct {
	// Guard is required.
	Guard Guard
	// Progress, when set, receives every [Progress] from the goroutine running the
	// primitive, which waits for it to return.
	Progress func(Progress)
}

// Apps runs the primitives with the timings of apps in the config.
type Apps struct {
	cfg config.Apps
	log *logging.Logger
}

// New returns the primitives with the timings in cfg.
func New(cfg config.Apps, log *logging.Logger) (*Apps, error) {
	switch {
	case cfg.StopTimeout <= 0, cfg.FailoverStopTimeout <= 0, cfg.StageStartTimeout <= 0, cfg.MinHealthyTime <= 0:
		return nil, errors.New("apps timings must be positive")
	case log == nil:
		return nil, errors.New("no logger")
	}

	return &Apps{cfg: cfg, log: log}, nil
}

// StopDatacenter stops the managed jobs of dc, stages from the highest down, within
// apps.stop_timeout for all stages together. After the last stage the jobs are read
// again: a managed job registered or started again meanwhile gets another pass of the
// stages, until none is left or the time is up. The result holds the stages that ran,
// the further passes included; on an error the last of them is where it ended.
func (a *Apps) StopDatacenter(ctx context.Context, dc Datacenter, opts Options) (Result, error) {
	return a.stop(ctx, dc, opts, a.cfg.StopTimeout)
}

// StopDatacenterFailover is [Apps.StopDatacenter] within apps.failover_stop_timeout,
// for the attempt to stop the old datacenter during a failover.
func (a *Apps) StopDatacenterFailover(ctx context.Context, dc Datacenter, opts Options) (Result, error) {
	return a.stop(ctx, dc, opts, a.cfg.FailoverStopTimeout)
}

// StartDatacenter starts the managed jobs of dc, stages from the lowest up, each stage
// within apps.stage_start_timeout. A running job is not registered again, only waited
// for. It refuses with [nomad.ErrInvalidStage] before starting anything when a managed
// job has an invalid stage, because there is no telling when such a job should start.
func (a *Apps) StartDatacenter(ctx context.Context, dc Datacenter, opts Options) (Result, error) {
	r, err := a.newRun(dc, opts, ActionStart)
	if err != nil {
		return r.result, err
	}

	listCtx, cancel := context.WithTimeoutCause(ctx, a.cfg.StageStartTimeout, ErrTimeout)
	jobs, err := r.listJobs(listCtx)

	cancel()

	if err != nil {
		return r.result, err
	}

	var invalid []error

	for _, j := range jobs {
		if j.StageErr != nil {
			invalid = append(invalid, j.StageErr)
		}
	}

	if len(invalid) > 0 {
		return r.result, fmt.Errorf("start apps of %s: %w", dc.Name, errors.Join(invalid...))
	}

	for _, s := range stages(jobs, ActionStart) {
		stageCtx, cancel := context.WithTimeoutCause(ctx, a.cfg.StageStartTimeout, ErrTimeout)
		err := r.runStage(stageCtx, s)

		cancel()

		if err != nil {
			return r.result, err
		}
	}

	return r.result, nil
}

func (a *Apps) stop(ctx context.Context, dc Datacenter, opts Options, timeout time.Duration) (Result, error) {
	r, err := a.newRun(dc, opts, ActionStop)
	if err != nil {
		return r.result, err
	}

	ctx, cancel := context.WithTimeoutCause(ctx, timeout, ErrTimeout)
	defer cancel()

	jobs, err := r.listJobs(ctx)
	if err != nil {
		return r.result, err
	}

	for {
		for _, s := range stages(jobs, ActionStop) {
			if err := r.runStage(ctx, s); err != nil {
				return r.result, err
			}
		}

		all, err := r.listJobs(ctx)
		if err != nil {
			return r.result, err
		}

		jobs = slices.DeleteFunc(all, func(j nomad.Job) bool { return j.Stop })
		if len(jobs) == 0 {
			return r.result, nil
		}

		r.log.WarnCtx(ctx, "managed jobs run after their stage, stopping them again",
			r.fields(logging.Int("jobs", len(jobs)), logging.Str("first", jobs[0].ID))...)
	}
}

// plannedStage is a stage with the jobs that were managed when the primitive began.
type plannedStage struct {
	number int
	jobs   []nomad.Job
}

// stages groups the jobs by stage, in the order of the action. A job with an invalid
// stage joins the first stage of a stop: it is unknown what it serves, so it goes as
// early as possible.
func stages(jobs []nomad.Job, action Action) []plannedStage {
	top := 1

	for _, j := range jobs {
		if j.StageErr == nil {
			top = max(top, j.Stage)
		}
	}

	byNumber := make(map[int][]nomad.Job)

	for _, j := range jobs {
		n := j.Stage
		if j.StageErr != nil {
			n = top
		}

		byNumber[n] = append(byNumber[n], j)
	}

	out := make([]plannedStage, 0, len(byNumber))
	for n, js := range byNumber {
		slices.SortFunc(js, func(a, b nomad.Job) int { return strings.Compare(a.ID, b.ID) })
		out = append(out, plannedStage{number: n, jobs: js})
	}

	slices.SortFunc(out, func(a, b plannedStage) int {
		if action == ActionStop {
			return cmp.Compare(b.number, a.number)
		}

		return cmp.Compare(a.number, b.number)
	})

	return out
}
