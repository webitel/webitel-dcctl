package status

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// TrackerConfig configures a [Tracker].
type TrackerConfig struct {
	Model Config
	// DegradedAfter is status.degraded_after.
	DegradedAfter     time.Duration
	LeasePeriod       time.Duration
	LeaseAcquireAfter time.Duration
	// PeerAddress is listen.peer, the address of the observing instance in the journal.
	PeerAddress string
}

// TrackerConfigFrom returns the part of cfg the tracker uses.
func TrackerConfigFrom(cfg *config.Serve) TrackerConfig {
	return TrackerConfig{
		Model:             ConfigFrom(cfg),
		DegradedAfter:     cfg.Status.DegradedAfter,
		LeasePeriod:       cfg.Coordination.LeasePeriod,
		LeaseAcquireAfter: cfg.Coordination.LeaseAcquireAfter,
		PeerAddress:       cfg.Listen.Peer,
	}
}

// AlarmJournal journals the raising and clearing of alarms, see [journal.Journal].
type AlarmJournal interface {
	Append(ctx context.Context, e *dcctlv1.JournalEntry) (journal.Appended, error)
}

// Watcher gives the last snapshot and a channel closed when a newer one is published,
// see [collect.Collector.Watch].
type Watcher interface {
	Watch() (*collect.Snapshot, <-chan struct{})
}

// Observed is the status of one cycle with what depends on the cycles before it. It is
// shared by its readers, which must not modify it.
type Observed struct {
	Status Status
	Lease  coord.LeaseStatus
	// Alarms are the raised alarms in the order of [AlarmIDs], then by datacenter and
	// voter.
	Alarms []Alarm
}

// Tracker builds the status of every snapshot and keeps across cycles what the alarms
// and the lease are judged by. A critical alarm is raised on the first cycle its
// condition holds, a warning and an alarm of the active datacenter once it held for
// status.degraded_after; any is cleared on the first cycle its condition is false and
// every source it needs answered. While an operation runs under its lock, the alarms
// its steps cause are not raised for its datacenters. Every raise, clear and change of
// severity is journaled with the observing instance as the initiator.
type Tracker struct {
	cfg     TrackerConfig
	journal AlarmJournal
	log     *logging.Logger

	mu       sync.Mutex
	timeline coord.LeaseTimeline
	alarms   map[AlarmKey]*tracked
	// lastActive is the last active datacenter the voters told.
	lastActive string
	// operation is the name of the running operation and operationDCs the datacenters
	// it has been about: its target and every active datacenter seen while it runs.
	operation    string
	operationDCs []string
	// operationLocked is set when the last cycle saw dcctl/lock held for operation.
	operationLocked bool
	// seq is Snapshot.Seq of the last snapshot observed.
	seq uint64

	current atomic.Pointer[Observed]
}

// tracked is an alarm whose condition holds, raised or not yet.
type tracked struct {
	// since is the first cycle of the condition holding in a row.
	since  time.Time
	raised bool
	alarm  Alarm
}

// change is a raise, a clear or a change of severity to journal.
type change struct {
	alarm   Alarm
	cleared bool
	at      time.Time
}

// NewTracker returns a tracker that journals to j.
func NewTracker(cfg TrackerConfig, j AlarmJournal, log *logging.Logger) (*Tracker, error) {
	if cfg.DegradedAfter <= 0 {
		return nil, fmt.Errorf("status.degraded_after %s must be positive", cfg.DegradedAfter)
	}

	tl, err := coord.NewLeaseTimeline(cfg.LeasePeriod, cfg.LeaseAcquireAfter)
	if err != nil {
		return nil, err
	}

	return &Tracker{cfg: cfg, journal: j, log: log, timeline: tl, alarms: make(map[AlarmKey]*tracked)}, nil
}

// Current returns what the last cycle observed, nil before the first.
func (t *Tracker) Current() *Observed {
	return t.current.Load()
}

// Run observes every snapshot w publishes until ctx is done. A snapshot published while
// the previous one is observed may be skipped for a newer one.
func (t *Tracker) Run(ctx context.Context, w Watcher) {
	for {
		snap, next := w.Watch()
		if snap != nil {
			t.Observe(ctx, snap)
		}

		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

// Observe builds the status of snap, evaluates the lease and the alarms at its
// ObserveTime and journals what changed. It is called right after the snapshot is
// collected, as the lease is judged by the monotonic clock of this instance at the
// call. The journal entries are written before it returns.
//
// snap must be one of the collection loop, [collect.Collector.Watch], whose snapshots
// are numbered in order; never one collected apart, as for a preflight. A snapshot not
// newer than the last one observed is dropped, and the current result returned.
func (t *Tracker) Observe(ctx context.Context, snap *collect.Snapshot) *Observed {
	obs, changes, ok := t.advance(snap)
	if !ok {
		return t.Current()
	}

	t.record(ctx, snap.Observer, changes)

	return obs
}

// advance evaluates snap against what the cycles before it left and publishes the
// result. It reports false for a snapshot not newer than the last one.
func (t *Tracker) advance(snap *collect.Snapshot) (*Observed, []change, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if snap.Seq <= t.seq {
		return nil, nil, false
	}

	t.seq = snap.Seq

	at := snap.ObserveTime
	st := Build(snap, t.cfg.Model, at)

	var effective *coord.Record
	if st.Effective != nil {
		effective = st.View.Effective
		t.lastActive = st.Effective.ActiveDatacenter
	}

	lease, timeline := coord.EvaluateLease(t.timeline, leaseReads(snap), effective)
	t.timeline = timeline

	t.trackOperation(snap, st)

	var changes []change

	for _, f := range EvaluateAlarms(AlarmInput{Snapshot: snap, Status: st, Lease: lease, LastActive: t.lastActive}) {
		if c, ok := t.apply(f, &st, at); ok {
			changes = append(changes, c)
		}
	}

	obs := &Observed{Status: st, Lease: lease, Alarms: t.raised()}
	t.current.Store(obs)

	return obs, changes, true
}

// apply advances the alarm of f and returns what to journal. An alarm missing from
// the findings is kept as it is.
func (t *Tracker) apply(f Finding, st *Status, at time.Time) (change, bool) {
	def := alarmDefs[f.Key.ID]
	a := t.alarms[f.Key]

	if f.Signal == SignalUnknown && a != nil && a.raised && conflictResolved(a.alarm, st) {
		f.Signal = SignalClear
	}

	switch f.Signal {
	case SignalUnknown:
		return change{}, false
	case SignalClear:
		delete(t.alarms, f.Key)

		if a == nil || !a.raised {
			return change{}, false
		}

		return change{alarm: a.alarm, cleared: true, at: at}, true
	case SignalRaise:
	}

	if a != nil && a.raised {
		severityChanged := a.alarm.Severity != f.Severity
		a.alarm.Severity, a.alarm.Detail, a.alarm.Facts = f.Severity, f.Detail, f.Facts

		return change{alarm: a.alarm, at: at}, severityChanged
	}

	// The condition held during the operation is expected; once it ends, the delay
	// starts anew.
	if def.suppressible && t.suppressed(f.involved) {
		delete(t.alarms, f.Key)

		return change{}, false
	}

	if a == nil {
		a = &tracked{since: at}
		t.alarms[f.Key] = a
	}

	if def.delayed && at.Sub(a.since) < t.cfg.DegradedAfter {
		return change{}, false
	}

	a.raised = true
	a.alarm = Alarm{
		ID: f.Key.ID, Severity: f.Severity, Datacenter: f.Key.Datacenter, Voter: f.Key.Voter, VoterType: f.VoterType,
		StartTime: at, Detail: f.Detail, Facts: f.Facts,
	}

	return change{alarm: a.alarm, at: at}, true
}

// conflictResolved reports whether a raised conflict is behind a decided effective
// state of a higher generation, as after resolve: the voters that do not answer cannot
// hold a newer conflict then, and the old one no longer decides anything.
func conflictResolved(a Alarm, st *Status) bool {
	gen, ok := a.Facts["generation"].(int64)

	return a.ID == AlarmConflict && ok && st.Effective != nil && st.Effective.Generation > gen
}

// trackOperation remembers the datacenters of the running operation. A switch names
// its source only as the active datacenter until the new generation is written, so
// every active datacenter seen while it runs is one of it.
func (t *Tracker) trackOperation(snap *collect.Snapshot, st Status) {
	op := st.Operation
	if op.GetState() != dcctlv1.Operation_RUNNING {
		t.operation, t.operationDCs, t.operationLocked = "", nil, false

		return
	}

	if op.GetName() != t.operation {
		t.operation, t.operationDCs = op.GetName(), nil
	}

	t.operationLocked = lockedFor(snap, op.GetName())

	add := func(dc string) {
		if dc != "" && !slices.Contains(t.operationDCs, dc) {
			t.operationDCs = append(t.operationDCs, dc)
		}
	}

	add(datacenterName(op.GetTarget()))

	if st.Effective != nil {
		add(st.Effective.ActiveDatacenter)
	}
}

// lockedFor reports whether a KV that answered, knowing its leader, shows dcctl/lock
// held by a session for the operation named name. A record left running by a leader
// that died has no lock behind it.
func lockedFor(snap *collect.Snapshot, name string) bool {
	id := strings.TrimPrefix(name, "operations/")

	return slices.ContainsFunc(snap.Voters, func(v collect.Voter) bool {
		e := v.KV.Data.Lock
		if !v.KV.Reachable || !v.KV.Data.Meta.KnownLeader || e == nil || e.Session == "" {
			return false
		}

		var l kv.Lock
		if err := json.Unmarshal(e.Value, &l); err != nil {
			return false
		}

		return strings.TrimPrefix(l.Operation, "operations/") == id
	})
}

// suppressed reports whether a running operation, confirmed by its lock, is about one
// of the datacenters, or runs at all when the condition names none.
func (t *Tracker) suppressed(involved []string) bool {
	if t.operation == "" || !t.operationLocked {
		return false
	}

	return len(involved) == 0 || slices.ContainsFunc(involved, func(dc string) bool {
		return slices.Contains(t.operationDCs, dc)
	})
}

func (t *Tracker) raised() []Alarm {
	var out []Alarm

	for _, a := range t.alarms {
		if a.raised {
			out = append(out, a.alarm)
		}
	}

	slices.SortFunc(out, func(a, b Alarm) int {
		return cmp.Or(
			cmp.Compare(a.ID.Type(), b.ID.Type()), cmp.Compare(a.Datacenter, b.Datacenter), cmp.Compare(a.Voter, b.Voter),
		)
	})

	return out
}

// record journals the changes in parallel, so that a KV that does not answer delays
// the cycle by one timeout and not by one per change.
func (t *Tracker) record(ctx context.Context, observer collect.Instance, changes []change) {
	var wg sync.WaitGroup

	for _, c := range changes {
		wg.Go(func() {
			if _, err := t.journal.Append(ctx, t.entry(ctx, observer, c)); err != nil {
				t.log.WarnCtx(ctx, "alarm not journaled",
					logging.Str("alarm", string(c.alarm.ID)), logging.Str("datacenter", c.alarm.Datacenter), logging.Err(err))
			}
		})
	}

	wg.Wait()
}

func (t *Tracker) entry(ctx context.Context, observer collect.Instance, c change) *dcctlv1.JournalEntry {
	a := c.alarm
	e := &dcctlv1.JournalEntry{
		CreateTime: timestamppb.New(c.at),
		Type:       dcctlv1.JournalEntry_ALARM,
		Initiator: &dcctlv1.Initiator{
			Instance: &dcctlv1.Instance{
				Node: observer.Node, Datacenter: datacenterResource(observer.Datacenter), Address: t.cfg.PeerAddress,
			},
			System: true,
		},
		Datacenter: datacenterResource(a.Datacenter),
		Action:     &dcctlv1.JournalEntry_AlarmType{AlarmType: a.ID.Type()},
		Detail:     a.Detail,
	}

	switch {
	case c.cleared:
		e.Result, e.Detail = dcctlv1.JournalEntry_OK, "cleared: "+a.Detail
	case a.Severity == SeverityCritical:
		e.Result = dcctlv1.JournalEntry_ERROR
	default:
		e.Result = dcctlv1.JournalEntry_WARNING
	}

	if len(a.Facts) > 0 {
		params, err := structpb.NewStruct(a.Facts)
		if err != nil {
			t.log.WarnCtx(ctx, "alarm facts not journaled", logging.Str("alarm", string(a.ID)), logging.Err(err))
		} else {
			e.Params = params
		}
	}

	return e
}

func datacenterResource(dc string) string {
	if dc == "" {
		return ""
	}

	return "datacenters/" + dc
}

// leaseReads returns the reads of dcctl/lease of every voter in the snapshot, the way
// the coordinator reads them.
func leaseReads(snap *collect.Snapshot) []coord.VoterLease {
	out := make([]coord.VoterLease, 0, len(snap.Voters)+1)

	for _, v := range snap.Voters {
		name := voterName(snap.Profile, v)

		switch e := v.KV.Data.Lease; {
		case !v.KV.Reachable:
			out = append(out, coord.VoterLease{
				Name: name, Kind: coord.KindKV, Status: coord.StatusUnreachable, Reason: v.KV.Reason, Err: v.KV.Err,
			})
		case !v.KV.Data.Meta.KnownLeader:
			// A Consul without a leader may answer with an older lease than it holds.
			out = append(out, coord.VoterLease{
				Name: name, Kind: coord.KindKV, Status: coord.StatusUnreachable, Reason: source.ReasonNoLeader, Err: errNoLeader,
			})
		case e == nil:
			out = append(out, coord.VoterLease{Name: name, Kind: coord.KindKV, Status: coord.StatusAbsent})
		default:
			out = append(out, coord.PresentLease(name, coord.KindKV, e.Value, coord.Version(e.ModifyIndex)))
		}
	}

	if a := snap.Arbiter; a != nil {
		switch rec := a.Data.Lease; {
		case !a.Reachable:
			out = append(out, coord.VoterLease{
				Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUnreachable, Reason: a.Reason, Err: a.Err,
			})
		case !a.Data.Initialized:
			out = append(out, coord.VoterLease{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusUninitialized})
		case rec == nil:
			out = append(out, coord.VoterLease{Name: coord.ArbiterName, Kind: coord.KindArbiter, Status: coord.StatusAbsent})
		default:
			out = append(out, coord.PresentLease(coord.ArbiterName, coord.KindArbiter, rec.Value, coord.Version(rec.Etag)))
		}
	}

	return out
}
