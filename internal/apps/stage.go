package apps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

// run is one call of a primitive.
type run struct {
	cfg    config.Apps
	log    *logging.Logger
	dc     Datacenter
	opts   Options
	action Action
	result Result
}

func (a *Apps) newRun(dc Datacenter, opts Options, action Action) (*run, error) {
	r := &run{
		cfg: a.cfg, log: a.log, dc: dc, opts: opts, action: action,
		result: Result{Datacenter: dc.Name, Action: action},
	}

	if opts.Guard == nil {
		return r, fmt.Errorf("%s apps of %s: no guard", action, dc.Name)
	}

	return r, nil
}

// guard runs the caller's guard. It is also the check of every start attempt, so it
// may run from several goroutines at once. A guard that fails because ctx ended is not
// a refusal: the cause of ctx is returned instead.
func (r *run) guard(ctx context.Context) error {
	err := r.opts.Guard(ctx)

	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return context.Cause(ctx)
	}

	return fmt.Errorf("%w: %w", ErrGuard, err)
}

// listJobs reads the managed jobs, retrying until ctx ends.
func (r *run) listJobs(ctx context.Context) ([]nomad.Job, error) {
	logged := false

	for {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		managed, _, err := r.dc.Nomad.Jobs(readCtx)

		cancel()

		if err == nil {
			return managed, nil
		}

		if !logged {
			r.log.WarnCtx(ctx, "nomad jobs not read, retrying", r.fields(logging.Err(err))...)

			logged = true
		}

		if !sleep(ctx, pollInterval) {
			return nil, fmt.Errorf("%s apps of %s: list jobs: %w: %w", r.action, r.dc.Name, context.Cause(ctx), err)
		}
	}
}

// stageRun is the state of the stage being run.
type stageRun struct {
	number int
	jobs   []nomad.Job
	// status is aligned with jobs.
	status []JobStatus
	// reached holds the jobs that met the condition at some poll of the stage.
	reached map[string]bool
	state   StageState
	// index is the position of the stage in the result.
	index int
	// readFailing is set while the last poll could not read some job.
	readFailing bool
}

func (r *run) runStage(ctx context.Context, plan plannedStage) error {
	st := r.begin(plan)

	if err := r.guard(ctx); err != nil {
		return r.end(ctx, st, StageFailed, err)
	}

	r.report(st)

	started := time.Now()

	r.log.InfoCtx(ctx, "apps stage started", r.fields(logging.Int("stage", st.number), logging.Int("jobs", len(st.jobs)))...)

	for {
		if obs, ok := r.observe(ctx, st.jobs); ok {
			needs, done := r.apply(ctx, st, obs)
			if done {
				r.log.InfoCtx(ctx, "apps stage done",
					r.fields(logging.Int("stage", st.number), logging.Dur("took", time.Since(started)))...)

				return r.end(ctx, st, StageSucceeded, nil)
			}

			if err := r.write(ctx, st, needs); err != nil {
				return r.end(ctx, st, StageFailed, err)
			}

			r.report(st)
		}

		if !sleep(ctx, pollInterval) {
			return r.end(ctx, st, StageFailed, context.Cause(ctx))
		}
	}
}

func (r *run) begin(plan plannedStage) *stageRun {
	st := &stageRun{
		number:  plan.number,
		jobs:    plan.jobs,
		status:  make([]JobStatus, len(plan.jobs)),
		reached: make(map[string]bool),
		state:   StageRunning,
		index:   len(r.result.Stages),
	}

	for i, j := range plan.jobs {
		st.status[i] = JobStatus{ID: j.ID, Type: j.Type, State: JobPending}
	}

	r.result.Stages = append(r.result.Stages, Stage{Number: st.number, State: st.state})

	return st
}

// end records the final state of the stage. A failed start leaves every job still
// starting unhealthy.
func (r *run) end(ctx context.Context, st *stageRun, state StageState, cause error) error {
	st.state = state

	var unfinished []string

	for i := range st.status {
		s := &st.status[i]
		if s.State == r.goal() {
			continue
		}

		unfinished = append(unfinished, s.ID)

		if state == StageFailed && s.State == JobStarting {
			s.State = JobUnhealthy
		}
	}

	r.report(st)

	if cause == nil {
		return nil
	}

	err := fmt.Errorf("%s stage %d in %s: %s: %w", r.action, st.number, r.dc.Name, strings.Join(unfinished, ", "), cause)

	r.log.WarnCtx(ctx, "apps stage failed", r.fields(logging.Int("stage", st.number), logging.Err(err))...)

	return err
}

func (r *run) report(st *stageRun) {
	stage := Stage{Number: st.number, State: st.state, Jobs: st.status}.clone()
	r.result.Stages[st.index] = stage

	if r.opts.Progress != nil {
		r.opts.Progress(Progress{Datacenter: r.dc.Name, Action: r.action, Stage: stage.clone()})
	}
}

// goal is the state every job of a stage has to reach.
func (r *run) goal() JobState {
	if r.action == ActionStop {
		return JobStopped
	}

	return JobHealthy
}

// observation is what one poll read; allocs and allocErrs are aligned with the jobs of
// the stage.
type observation struct {
	at        time.Time
	jobs      map[string]nomad.Job
	jobsErr   error
	allocs    [][]nomad.Alloc
	allocErrs []error
	nodes     []nomad.Node
	nodesErr  error
}

// observe reads the jobs of the stage. ok is false when ctx ended meanwhile: what was
// read then says nothing about the jobs.
func (r *run) observe(ctx context.Context, jobs []nomad.Job) (obs observation, ok bool) {
	// The time is taken before the reads: uptime may only come out shorter than it is.
	obs.at = time.Now()

	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	obs.allocs = make([][]nomad.Alloc, len(jobs))
	obs.allocErrs = make([]error, len(jobs))

	managed, _, err := r.dc.Nomad.Jobs(readCtx)
	if err != nil {
		obs.jobsErr = err
	} else {
		obs.jobs = make(map[string]nomad.Job, len(managed))
		for _, j := range managed {
			obs.jobs[j.ID] = j
		}

		var g errgroup.Group

		g.SetLimit(parallelism)

		for i, j := range jobs {
			g.Go(func() error {
				obs.allocs[i], obs.allocErrs[i] = r.dc.Nomad.Allocs(readCtx, j.ID)

				return nil
			})
		}

		if r.needsNodes(jobs) {
			g.Go(func() error {
				obs.nodes, obs.nodesErr = r.dc.Nomad.Nodes(readCtx)

				return nil
			})
		}

		_ = g.Wait()
	}

	return obs, ctx.Err() == nil
}

func (r *run) needsNodes(jobs []nomad.Job) bool {
	return r.action == ActionStart && slices.ContainsFunc(jobs, func(j nomad.Job) bool { return j.Type == jobstate.TypeSystem })
}

// apply judges every job of the stage by the observation. It returns the indexes of the
// jobs to stop or start, and whether every job meets the condition.
func (r *run) apply(ctx context.Context, st *stageRun, obs observation) (needs []int, done bool) {
	done = true

	var readErr error

	for i, j := range st.jobs {
		prev := st.status[i].State
		s, need := r.judge(j, i, obs, st.reached[j.ID])
		st.status[i] = s

		switch {
		case s.State == r.goal():
			st.reached[j.ID] = true
		case s.State == JobUnknown:
			readErr = cmp.Or(readErr, s.Err)
		case prev == r.goal():
			r.log.WarnCtx(ctx, "job left the condition of its stage",
				r.fields(logging.Int("stage", st.number), logging.Str("job", j.ID), logging.Str("detail", s.Detail))...)
		}

		done = done && s.State == r.goal()

		if need {
			needs = append(needs, i)
		}
	}

	switch {
	case readErr != nil && !st.readFailing:
		r.log.WarnCtx(ctx, "nomad state not read, retrying", r.fields(logging.Int("stage", st.number), logging.Err(readErr))...)
	case readErr == nil && st.readFailing:
		r.log.InfoCtx(ctx, "nomad state read again", r.fields(logging.Int("stage", st.number))...)
	}

	st.readFailing = readErr != nil

	return needs, done
}

// judge returns the status of job i of the stage and whether it needs a stop or start.
// reached tells that the job met the condition earlier in the stage.
func (r *run) judge(job nomad.Job, i int, obs observation, reached bool) (JobStatus, bool) {
	s := JobStatus{ID: job.ID, Type: job.Type}

	if obs.jobsErr != nil {
		s.State, s.Detail, s.Err = JobUnknown, "state not read", obs.jobsErr

		return s, false
	}

	cur, listed := obs.jobs[job.ID]
	allocs := obs.allocs[i]

	// A job read without its allocations is unknown, yet one listed as not stopped, or
	// not started, still gets its stop or start.
	readErr := obs.allocErrs[i]
	if r.action == ActionStart && job.Type == jobstate.TypeSystem {
		readErr = cmp.Or(readErr, obs.nodesErr)
	}

	if readErr != nil {
		s.State, s.Detail, s.Err = JobUnknown, "state not read", readErr

		return s, listed && cur.Stop == (r.action == ActionStart)
	}

	if r.action == ActionStop {
		return judgeStop(s, cur, listed, allocs, reached, r.opts.TolerateUnknown)
	}

	return r.judgeStart(s, cur, listed, allocs, obs, reached)
}

func judgeStop(s JobStatus, cur nomad.Job, listed bool, allocs []nomad.Alloc, reached, tolerateUnknown bool) (JobStatus, bool) {
	if !listed {
		if n := jobstate.ActiveAllocs(allocs); n > 0 {
			s.State, s.Detail = JobStopping, fmt.Sprintf("job is no longer registered, %d allocations active", n)

			return s, false
		}

		s.State, s.Warnings = JobStopped, []string{"job is no longer registered"}

		return s, false
	}

	ok, detail := jobstate.Stopped(cur, allocs)
	if ok {
		s.State = JobStopped

		return s, false
	}

	if tolerateUnknown && jobstate.UnknownLeft(cur, allocs) {
		s.State = JobStopped
		s.Warnings = []string{fmt.Sprintf("%d allocations unknown, their nodes do not answer", jobstate.ActiveAllocs(allocs))}

		return s, false
	}

	if reached && !cur.Stop {
		detail = "running again: " + detail
	}

	s.State, s.Detail = JobStopping, detail

	return s, !cur.Stop
}

func (r *run) judgeStart(
	s JobStatus, cur nomad.Job, listed bool, allocs []nomad.Alloc, obs observation, reached bool,
) (JobStatus, bool) {
	notYet := JobStarting
	if reached {
		notYet = JobUnhealthy
	}

	switch {
	case !listed:
		s.State, s.Detail = notYet, "job is no longer registered"

		return s, true
	case cur.Stop:
		s.State, s.Detail = notYet, "stopped"
		if reached {
			s.Detail = "stopped again"
		}

		return s, true
	}

	var ok bool

	if cur.Type == jobstate.TypeSystem {
		sys := jobstate.SystemHealth(cur.ID, allocs, obs.nodes, obs.at, r.cfg.MinHealthyTime)
		ok, s.Detail, s.Warnings = sys.Healthy, sys.Detail, sys.Warnings()
	} else {
		ok, s.Detail = jobstate.ServiceHealth(allocs, obs.at, r.cfg.MinHealthyTime)
	}

	s.State = notYet
	if ok {
		s.State = JobHealthy
	}

	return s, false
}

// write stops or starts the jobs at the indexes. A stop round is guarded once; a start
// is guarded before each of its attempts by the Nomad client. A refusal of the guard is
// returned; other errors stay on the jobs, to be retried at the next poll.
func (r *run) write(ctx context.Context, st *stageRun, needs []int) error {
	if len(needs) == 0 {
		return nil
	}

	if r.action == ActionStop {
		if err := r.guard(ctx); err != nil {
			return err
		}
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	errs := make([]error, len(needs))

	var g errgroup.Group

	g.SetLimit(parallelism)

	for k, i := range needs {
		id := st.jobs[i].ID

		g.Go(func() error {
			if r.action == ActionStop {
				errs[k] = r.dc.Nomad.Stop(writeCtx, id)
			} else {
				errs[k] = r.dc.Nomad.Start(writeCtx, id, r.guard)
			}

			return nil
		})
	}

	_ = g.Wait()

	for k, err := range errs {
		if errors.Is(err, ErrGuard) {
			return fmt.Errorf("%s %s: %w", r.action, st.jobs[needs[k]].ID, err)
		}
	}

	// A write cut by the deadline says nothing about the job.
	if ctx.Err() != nil {
		return nil
	}

	for k, err := range errs {
		if err != nil {
			st.status[needs[k]].Err = fmt.Errorf("%s: %w", r.action, err)
		}
	}

	return nil
}

func (r *run) fields(extra ...logging.Field) []logging.Field {
	return append([]logging.Field{logging.Str("datacenter", r.dc.Name), logging.Str("action", string(r.action))}, extra...)
}

// sleep waits for d and reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
