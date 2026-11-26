// Package fencing stops the managed apps of the datacenter of this instance while the
// effective state names another datacenter active. It is the guard against split brain
// that dcctl applies by itself on every collection snapshot; it only ever stops, and it
// never touches Patroni.
//
// A snapshot makes it act when all of these hold:
//
//   - the active datacenter is known, see [coord.ActiveKnown], and is not this one;
//   - the voters are not in conflict;
//   - no copy of dcctl/op holds an unfinished operation or cannot be decoded;
//   - Nomad of this datacenter answered in the snapshot, no longer ago than three times
//     status.interval plus status.request_timeout, and lists a managed job that is not
//     stopped. A stopped job whose only active allocations are unknown does not count:
//     their node does not answer, and Nomad stops them once it is back.
//
// The last known data of a source that does not answer never makes it act. The copies
// of dcctl/op in a KV that does not answer are not checked, as for an operation that
// starts: fencing must still act while this datacenter is cut off from the one whose KV
// holds an operation. dcctl/op and the voters are read again, consistently, before each
// stage and each round of stops, and the stop ends as soon as these no longer allow it.
// A stop that ends this way before anything was stopped is only logged.
package fencing

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// staleCycles is how many cycles, each with its pause and its longest wait for the
// sources, the Nomad data of a snapshot may be old, as for the liveness of the collector.
const staleCycles = 3

var (
	errConflict      = errors.New("the voters are in conflict")
	errActiveUnknown = errors.New("the active datacenter is not known")
	errActiveHere    = errors.New("the effective state names this datacenter active")
)

// Watcher gives the collection snapshots; [*collect.Collector] is one.
type Watcher interface {
	Watch() (*collect.Snapshot, <-chan struct{})
}

// Config configures a [Fencer].
type Config struct {
	// Datacenter is the datacenter of this instance, the only one it stops. Its Nomad
	// must read consistently, see [apps.Nomad].
	Datacenter apps.Datacenter
	// Status configures the model every snapshot is evaluated with.
	Status status.Config
	// RequestTimeout is status.request_timeout.
	RequestTimeout time.Duration
}

// Deps are what a [Fencer] reads and writes. State and Ops must read consistently.
type Deps struct {
	Snapshots Watcher
	Apps      *apps.Apps
	State     *coord.Coordinator
	Ops       *coord.OpStore
	Journal   *journal.Journal
	// MeterProvider is the global one when nil.
	MeterProvider metric.MeterProvider
}

// Fencer evaluates every snapshot and stops the apps of its datacenter.
type Fencer struct {
	cfg    Config
	deps   Deps
	log    *logging.Logger
	maxAge time.Duration
	stops  metric.Int64Counter

	// lastRun is when the last stop ended; only [Fencer.Run] uses it.
	lastRun time.Time
}

// New returns the fencer of cfg.Datacenter. It does no I/O; [Fencer.Run] starts it.
func New(cfg Config, deps Deps, log *logging.Logger) (*Fencer, error) {
	switch {
	case cfg.Datacenter.Name == "", cfg.Datacenter.Nomad == nil:
		return nil, errors.New("no datacenter with its nomad")
	case cfg.Status.Interval <= 0, cfg.RequestTimeout <= 0:
		return nil, errors.New("status interval and request timeout must be positive")
	case deps.Snapshots == nil, deps.Apps == nil, deps.State == nil, deps.Ops == nil, deps.Journal == nil:
		return nil, errors.New("snapshots, apps, state, ops and journal are required")
	case log == nil:
		return nil, errors.New("no logger")
	}

	mp := deps.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}

	stops, err := newStopsCounter(mp)
	if err != nil {
		return nil, err
	}

	return &Fencer{
		cfg:    cfg,
		deps:   deps,
		log:    log,
		maxAge: staleCycles * (cfg.Status.Interval + cfg.RequestTimeout),
		stops:  stops,
	}, nil
}

// Run evaluates every new snapshot until ctx is done. A stop runs in this goroutine, so
// stops never overlap; the snapshots published meanwhile are skipped but the latest.
// Call it once.
func (f *Fencer) Run(ctx context.Context) {
	var seen uint64

	for ctx.Err() == nil {
		snap, next := f.deps.Snapshots.Watch()
		if snap != nil && snap.Seq != seen {
			seen = snap.Seq
			f.evaluate(ctx, snap)

			continue
		}

		select {
		case <-ctx.Done():
		case <-next:
		}
	}
}

// trigger is what made a snapshot start a stop.
type trigger struct {
	active     string
	generation int64
	// jobs are the managed jobs that are not stopped, in ID order.
	jobs []string
	// patroniRole is the role of the Patroni leader of this datacenter.
	patroniRole status.Role
}

func (f *Fencer) evaluate(ctx context.Context, snap *collect.Snapshot) {
	// A cycle that began before the last stop ended may show the jobs it stopped still
	// running.
	if snap.ObserveTime.Add(-snap.CycleDuration).Before(f.lastRun) {
		return
	}

	t, ok := f.check(snap, time.Now())
	if !ok {
		return
	}

	f.fence(ctx, snap.Observer, t)
	f.lastRun = time.Now()
}

// check reports whether snap makes this datacenter be fenced at now.
func (f *Fencer) check(snap *collect.Snapshot, now time.Time) (trigger, bool) {
	d, ok := snap.Datacenter(f.cfg.Datacenter.Name)
	if !ok || !d.Nomad.Reachable || now.Sub(d.Nomad.ObserveTime) > f.maxAge {
		return trigger{}, false
	}

	jobs := running(d.Nomad.Data)
	if len(jobs) == 0 {
		return trigger{}, false
	}

	st := status.Build(snap, f.cfg.Status, now)
	if refusal(st.View, status.Operations(snap), f.cfg.Datacenter.Name) != nil {
		return trigger{}, false
	}

	t := trigger{
		active:     st.View.Effective.State.ActiveDatacenter,
		generation: st.View.Effective.State.Generation,
		jobs:       jobs,
	}

	if i := slices.IndexFunc(st.Datacenters, func(d status.Datacenter) bool { return d.Name == f.cfg.Datacenter.Name }); i >= 0 {
		t.patroniRole = st.Datacenters[i].Patroni.Role
	}

	return t, true
}

// running returns the managed jobs of n that are not stopped, in ID order, but those
// left only with unknown allocations.
func running(n collect.Nomad) []string {
	var out []string

	for _, j := range n.Managed {
		allocs := n.Allocs[j.ID]
		if ok, _ := jobstate.Stopped(j, allocs); !ok && !jobstate.UnknownLeft(j, allocs) {
			out = append(out, j.ID)
		}
	}

	slices.Sort(out)

	return out
}

// refusal returns why the voters and dcctl/op do not let datacenter dc be fenced, or
// nil when they do.
func refusal(view coord.View, ops coord.OpView, dc string) error {
	switch {
	case len(view.Conflict) > 0:
		return errConflict
	case !coord.ActiveKnown(view):
		return errActiveUnknown
	case view.Effective.State.ActiveDatacenter == dc:
		return errActiveHere
	}

	return ops.CheckIdle()
}

// guard reads dcctl/op and the voters again before each stage and round of stops.
// dcctl/op goes first: an operation writes it before it changes the voters, so an
// operation that changed them is never missed.
func (f *Fencer) guard(ctx context.Context) error {
	ops, err := f.deps.Ops.Read(ctx)
	if err != nil {
		return err
	}

	view, err := f.deps.State.Read(ctx)
	if err != nil {
		return err
	}

	return refusal(view, ops, f.cfg.Datacenter.Name)
}

// stopWatch is the Nomad of one stop; it tells whether a job was stopped.
type stopWatch struct {
	apps.Nomad

	sent atomic.Bool
}

func (n *stopWatch) Stop(ctx context.Context, jobID string) error {
	n.sent.Store(true)

	return n.Nomad.Stop(ctx, jobID)
}

func (f *Fencer) fence(ctx context.Context, observer collect.Instance, t trigger) {
	f.log.WarnCtx(ctx, "stopping the apps of this datacenter, another one is active",
		logging.Str("datacenter", f.cfg.Datacenter.Name),
		logging.Str("active_datacenter", t.active),
		logging.Int64("generation", t.generation),
		logging.Str("jobs", strings.Join(t.jobs, ",")),
	)

	nomad := &stopWatch{Nomad: f.cfg.Datacenter.Nomad}
	dc := apps.Datacenter{Name: f.cfg.Datacenter.Name, Nomad: nomad}

	res, err := f.deps.Apps.StopDatacenter(ctx, dc, apps.Options{Guard: f.guard, TolerateUnknown: true})
	if errors.Is(err, apps.ErrGuard) && !nomad.sent.Load() {
		f.log.InfoCtx(ctx, "stop of the apps of this datacenter refused before any job was stopped",
			logging.Str("datacenter", dc.Name), logging.Err(err))

		return
	}

	f.report(ctx, observer, t, res, err)
}
