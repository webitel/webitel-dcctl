package apps_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

var _ apps.Nomad = (*nomad.Client)(nil)

const (
	dcName = "dc_a"
	// never is a delay after which the fake never acts.
	never = -1

	stopDelay  = 3 * time.Second
	startDelay = time.Second
)

var (
	cfg = config.Apps{
		StopTimeout:         120 * time.Second,
		StageStartTimeout:   180 * time.Second,
		FailoverStopTimeout: 30 * time.Second,
		MinHealthyTime:      10 * time.Second,
	}

	errLost = errors.New("lease lost")
	errDown = errors.New("nomad is down")
)

func newApps(t *testing.T) *apps.Apps {
	t.Helper()

	a, err := apps.New(cfg, logging.New(logging.WithProvider(logtest.NewRecorder())))
	require.NoError(t, err)

	return a
}

func allow(context.Context) error { return nil }

// call is a write the fake received.
type call struct {
	op  string
	job string
	at  time.Time
}

// fakeNomad is the Nomad of one datacenter in memory. A stopped job loses its allocations
// after its stop delay, a started one gets them after its start delay: one per node that
// runs the service for a system job, a single one for a service job.
type fakeNomad struct {
	mu     sync.Mutex
	jobs   map[string]*nomad.Job
	allocs map[string][]nomad.Alloc
	nodes  []nomad.Node

	stopDelays  map[string]time.Duration
	startDelays map[string]time.Duration
	// stopNoop makes Stop answer success and change nothing, as Nomad does for a job it
	// does not know.
	stopNoop bool

	jobsErr   error
	nodesErr  error
	allocsErr map[string]error

	calls  []call
	allocN int
}

func newFake() *fakeNomad {
	return &fakeNomad{
		jobs:        map[string]*nomad.Job{},
		allocs:      map[string][]nomad.Alloc{},
		stopDelays:  map[string]time.Duration{},
		startDelays: map[string]time.Duration{},
		allocsErr:   map[string]error{},
	}
}

func (f *fakeNomad) dc() apps.Datacenter { return apps.Datacenter{Name: dcName, Nomad: f} }

func (f *fakeNomad) addNode(n nomad.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nodes = append(f.nodes, n)
}

// addJob registers a job that runs healthy since long ago, or a stopped one.
func (f *fakeNomad) addJob(id, typ string, stage int, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.jobs[id] = &nomad.Job{ID: id, Name: id, Type: typ, Stage: stage, Stop: !running}
	if running {
		f.place(id, time.Now().Add(-time.Hour))
	}
}

func (f *fakeNomad) addInvalidJob(id string, running bool) {
	f.addJob(id, "service", 0, running)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.jobs[id].StageErr = fmt.Errorf("%w: job %q has meta.dcctl_stage %q", nomad.ErrInvalidStage, id, "late")
}

func (f *fakeNomad) Jobs(context.Context) (managed, unmanaged []nomad.Job, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.jobsErr != nil {
		return nil, nil, f.jobsErr
	}

	for _, id := range slices.Sorted(maps.Keys(f.jobs)) {
		managed = append(managed, *f.jobs[id])
	}

	return managed, nil, nil
}

func (f *fakeNomad) Allocs(_ context.Context, jobID string) ([]nomad.Alloc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.allocsErr[jobID]; err != nil {
		return nil, err
	}

	out := make([]nomad.Alloc, 0, len(f.allocs[jobID]))
	for _, a := range f.allocs[jobID] {
		a.TaskStates = maps.Clone(a.TaskStates)
		out = append(out, a)
	}

	return out, nil
}

func (f *fakeNomad) Nodes(context.Context) ([]nomad.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.nodesErr != nil {
		return nil, f.nodesErr
	}

	return slices.Clone(f.nodes), nil
}

func (f *fakeNomad) Stop(_ context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call{op: "stop", job: jobID, at: time.Now()})

	job, ok := f.jobs[jobID]
	if !ok || f.stopNoop {
		return nil
	}

	job.Stop = true

	if d := f.delay(f.stopDelays, jobID, stopDelay); d >= 0 {
		time.AfterFunc(d, func() {
			f.mu.Lock()
			defer f.mu.Unlock()

			if job.Stop {
				f.finish(jobID)
			}
		})
	}

	return nil
}

func (f *fakeNomad) Start(ctx context.Context, jobID string, check func(context.Context) error) error {
	if err := check(ctx); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call{op: "start", job: jobID, at: time.Now()})

	job, ok := f.jobs[jobID]
	if !ok {
		return fmt.Errorf("job %q not found", jobID)
	}

	job.Stop = false

	if d := f.delay(f.startDelays, jobID, startDelay); d >= 0 {
		time.AfterFunc(d, func() {
			f.mu.Lock()
			defer f.mu.Unlock()

			if !job.Stop {
				f.place(jobID, time.Now())
			}
		})
	}

	return nil
}

func (f *fakeNomad) delay(m map[string]time.Duration, jobID string, def time.Duration) time.Duration {
	if d, ok := m[jobID]; ok {
		return d
	}

	return def
}

// place replaces the allocations of the job with running ones started at startedAt.
func (f *fakeNomad) place(jobID string, startedAt time.Time) {
	f.finish(jobID)

	nodes := []string{"n1"}
	if f.jobs[jobID].Type == "system" {
		nodes = nil

		for _, n := range f.nodes {
			if n.Status == "ready" && n.SchedulingEligibility == "eligible" && !n.Drain &&
				slices.Contains(n.Services, strings.ReplaceAll(jobID, "-", "_")) {
				nodes = append(nodes, n.ID)
			}
		}
	}

	for _, n := range nodes {
		f.allocN++
		f.allocs[jobID] = append(f.allocs[jobID], nomad.Alloc{
			ID: fmt.Sprintf("%s-%d", jobID, f.allocN), JobID: jobID, NodeID: n,
			ClientStatus: "running", DesiredStatus: "run",
			TaskStates: map[string]nomad.TaskState{"main": {State: "running", StartedAt: startedAt}},
		})
	}
}

// finish completes every allocation of the job.
func (f *fakeNomad) finish(jobID string) {
	for i := range f.allocs[jobID] {
		f.allocs[jobID][i].ClientStatus = "complete"
		f.allocs[jobID][i].DesiredStatus = "stop"
	}
}

// runAgain starts the job behind dcctl's back, as an Ansible run would.
func (f *fakeNomad) runAgain(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.jobs[jobID].Stop = false
	f.place(jobID, time.Now())
}

// stopBehindBack stops the job and its allocations at once.
func (f *fakeNomad) stopBehindBack(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.jobs[jobID].Stop = true
	f.finish(jobID)
}

// restartTasks restarts every task of the running allocations of the job.
func (f *fakeNomad) restartTasks(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, a := range f.allocs[jobID] {
		for name, ts := range a.TaskStates {
			ts.Restarts++
			ts.StartedAt = time.Now()
			a.TaskStates[name] = ts
		}
	}
}

// purge removes the job from the job list and keeps its allocations.
func (f *fakeNomad) purge(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.jobs, jobID)
}

func (f *fakeNomad) completeAllocs(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.finish(jobID)
}

func (f *fakeNomad) isStopped(jobID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.jobs[jobID].Stop
}

func (f *fakeNomad) set(fn func(f *fakeNomad)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

// callsOf returns the calls of op to jobID.
func (f *fakeNomad) callsOf(op, jobID string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []call

	for _, c := range f.calls {
		if c.op == op && c.job == jobID {
			out = append(out, c)
		}
	}

	return out
}

func (f *fakeNomad) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

// firstCall returns when op was first called for jobID.
func (f *fakeNomad) firstCall(t *testing.T, op, jobID string) time.Time {
	t.Helper()

	calls := f.callsOf(op, jobID)
	require.NotEmpty(t, calls, "%s %s was not called", op, jobID)

	return calls[0].at
}

// recorder keeps the progress reports.
type recorder struct {
	mu      sync.Mutex
	reports []apps.Progress
}

func (r *recorder) record(p apps.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.reports = append(r.reports, p)
}

func (r *recorder) all() []apps.Progress {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.reports)
}

// jobStates returns the states job went through in the reports, without repeats.
func (r *recorder) jobStates(jobID string) []apps.JobState {
	var out []apps.JobState

	for _, p := range r.all() {
		for _, j := range p.Stage.Jobs {
			if j.ID == jobID && (len(out) == 0 || out[len(out)-1] != j.State) {
				out = append(out, j.State)
			}
		}
	}

	return out
}

func stageNumbers(res apps.Result) []int {
	out := make([]int, 0, len(res.Stages))
	for _, s := range res.Stages {
		out = append(out, s.Number)
	}

	return out
}

func jobOf(t *testing.T, s apps.Stage, id string) apps.JobStatus {
	t.Helper()

	for _, j := range s.Jobs {
		if j.ID == id {
			return j
		}
	}

	require.FailNow(t, "job not in stage", "%s not in stage %d", id, s.Number)

	return apps.JobStatus{}
}

// background runs fn in a goroutine of the synctest bubble; the result comes on the channel.
func background(fn func() (apps.Result, error)) <-chan outcome {
	done := make(chan outcome, 1)

	go func() {
		res, err := fn()
		done <- outcome{res: res, err: err}
	}()

	return done
}

type outcome struct {
	res apps.Result
	err error
}
