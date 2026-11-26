package api

import (
	"context"
	"time"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// StatusSource gives the status model of the last collection cycle.
type StatusSource interface {
	// Status returns the model at now; false before the first cycle finished.
	Status(now time.Time) (status.Status, bool)
}

// Snapshots gives the last collection snapshot, nil before the first;
// [*collect.Collector] implements it.
type Snapshots interface {
	Snapshot() *collect.Snapshot
}

// FromCollector returns the status source that builds the model from the last
// snapshot of c on every call.
func FromCollector(c Snapshots, cfg status.Config) StatusSource {
	return collectorStatus{snaps: c, cfg: cfg}
}

// Observations gives what the last collection cycle observed; [*status.Tracker] is
// one.
type Observations interface {
	// Current returns nil before the first cycle.
	Current() *status.Observed
}

// FromTracker returns the status source of the last cycle src observed, the same cycle
// [TrackerAlarms] reports the alarms of. The status is built once per cycle, at the end
// of it, and not on every call.
func FromTracker(src Observations) StatusSource {
	return trackerStatus{src: src}
}

type trackerStatus struct {
	src Observations
}

func (t trackerStatus) Status(time.Time) (status.Status, bool) {
	obs := t.src.Current()
	if obs == nil {
		return status.Status{}, false
	}

	return obs.Status, true
}

type collectorStatus struct {
	snaps Snapshots
	cfg   status.Config
}

func (c collectorStatus) Status(now time.Time) (status.Status, bool) {
	snap := c.snaps.Snapshot()
	if snap == nil {
		return status.Status{}, false
	}

	return status.Build(snap, c.cfg, now), true
}

// Alarms gives the alarms raised now. The API marshals the returned alarms
// after the call, so they must not be changed afterwards.
type Alarms interface {
	Alarms(ctx context.Context) []*dcctlv1.Status_Alarm
}

// Verifier runs the verify.* checks: the conditions of a completed switch for
// the current active datacenter. The API marshals the returned checks after
// the call, so they must not be changed afterwards.
type Verifier interface {
	Verify(ctx context.Context) ([]*dcctlv1.Check, error)
}

// Operations reads the copies of dcctl/op; [*coord.OpStore] implements it.
type Operations interface {
	Read(ctx context.Context) (coord.OpView, error)
}

// OperationEvents tells which operations this instance runs and streams their
// changes; [FromEngine] gives it.
type OperationEvents interface {
	// Runs reports whether this instance runs the operation name now.
	Runs(name string) bool
	// Subscribe returns the changes of the operation name, see
	// [operation.Subscription].
	Subscribe(name string) OperationSubscription
}

// OperationSubscription is [*operation.Subscription].
type OperationSubscription interface {
	C() <-chan *dcctlv1.Operation
	Close()
}

// FromEngine returns the events of the operations e runs.
func FromEngine(e *operation.Engine) OperationEvents { return engineEvents{e} }

type engineEvents struct{ e *operation.Engine }

func (v engineEvents) Runs(name string) bool { return v.e.Runs(name) }

func (v engineEvents) Subscribe(name string) OperationSubscription { return v.e.Subscribe(name) }

// Journal lists journal entries; [*journal.Journal] implements it.
type Journal interface {
	List(ctx context.Context, req *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error)
}

var (
	_ Snapshots  = (*collect.Collector)(nil)
	_ Operations = (*coord.OpStore)(nil)
	_ Journal    = (*journal.Journal)(nil)
)
