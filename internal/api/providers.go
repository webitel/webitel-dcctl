package api

import (
	"context"
	"time"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
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
	Alarms() []*dcctlv1.Status_Alarm
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

// Journal lists journal entries; [*journal.Journal] implements it.
type Journal interface {
	List(ctx context.Context, req *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error)
}

var (
	_ Snapshots  = (*collect.Collector)(nil)
	_ Operations = (*coord.OpStore)(nil)
	_ Journal    = (*journal.Journal)(nil)
)
