package pgctl

import (
	"context"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Catchup waits, after demoted was demoted, for the standby leader of target to receive
// all WAL of demoted and replay all it received: received_location of target reaches
// replayed_location of the demoted leader, the end of its WAL, and replayed_location of
// target equals its received_location. Both leaders are read at every poll; the end is
// the highest replayed_location seen. Positions on different timelines are not comparable:
// the two leaders on different timelines fail the precondition. It sends nothing and
// takes no guard.
func (c *Controller) Catchup(ctx context.Context, demoted, target Cluster, opts Options) (Result, error) {
	w := &catchup{demoted: demoted, target: target}

	return c.run(ctx, primitive{
		action: ActionCatchup, dc: target.Datacenter, timeout: c.cfg.Timeouts.Catchup, observe: w.observe,
	}, opts)
}

type catchup struct {
	demoted, target Cluster
	// end is the highest replayed_location of the demoted leader seen.
	end *int64
	// timeline is the timeline of the demoted leader at the last poll.
	timeline int64
}

func (w *catchup) observe(ctx context.Context) (Observation, bool, error) {
	if obs, ok, err := w.readEnd(ctx); !ok || err != nil {
		return obs, false, err
	}

	members, err := w.target.Patroni.Members(ctx)
	if err != nil {
		return readFailed(w.target.Datacenter, err)
	}

	obs := describe(members)

	l, ok := leaderOf(members)
	if !ok {
		return obs.unmet(nil, "%s has no leader", w.target.Datacenter)
	}

	if l.Probe == nil {
		return obs.unmet(l.Err, "leader %s not read", l.Name)
	}

	x := l.Probe.Xlog
	if x.ReplayedLocation != nil {
		obs.Lag = new(max(0, *w.end-*x.ReplayedLocation))
	}

	switch {
	case l.Probe.Role == patroni.RolePrimary:
		return obs, false, precondition("leader %s of %s is primary before the catchup", l.Name, w.target.Datacenter)
	case l.Probe.Role != patroni.RoleStandbyLeader:
		return obs.unmet(nil, "leader %s is %s", l.Name, l.Probe.Role)
	case l.Probe.Timeline == nil:
		return obs.unmet(nil, "timeline of %s unknown", l.Name)
	case *l.Probe.Timeline != w.timeline:
		return obs, false, precondition("leader %s on timeline %d, the demoted leader on %d", l.Name, *l.Probe.Timeline, w.timeline)
	case x.ReceivedLocation == nil || x.ReplayedLocation == nil:
		return obs.unmet(nil, "WAL positions of %s unknown", l.Name)
	case *x.ReceivedLocation < *w.end:
		return obs.unmet(nil, "%s received %d of %d", l.Name, *x.ReceivedLocation, *w.end)
	case *x.ReplayedLocation != *x.ReceivedLocation:
		return obs.unmet(nil, "%s replayed %d of %d received", l.Name, *x.ReplayedLocation, *x.ReceivedLocation)
	}

	return obs, true, nil
}

// readEnd reads the replayed_location of the demoted leader into w.end. ok is false when
// it is not known at this poll.
func (w *catchup) readEnd(ctx context.Context) (Observation, bool, error) {
	members, err := w.demoted.Patroni.Members(ctx)
	if err != nil {
		return readFailed(w.demoted.Datacenter, err)
	}

	obs := describe(members)

	l, ok := leaderOf(members)
	if !ok {
		return obs.unmet(nil, "%s has no leader", w.demoted.Datacenter)
	}

	switch {
	case l.Probe == nil:
		return obs.unmet(l.Err, "demoted leader %s not read", l.Name)
	case l.Probe.Role == patroni.RolePrimary:
		return obs, false, precondition("leader %s of %s is still primary", l.Name, w.demoted.Datacenter)
	case l.Probe.Role != patroni.RoleStandbyLeader:
		return obs.unmet(nil, "demoted leader %s is %s", l.Name, l.Probe.Role)
	case l.Probe.Xlog.ReplayedLocation == nil:
		return obs.unmet(nil, "replayed_location of demoted leader %s unknown", l.Name)
	case l.Probe.Timeline == nil:
		return obs.unmet(nil, "timeline of demoted leader %s unknown", l.Name)
	}

	w.timeline = *l.Probe.Timeline

	if end := *l.Probe.Xlog.ReplayedLocation; w.end == nil || end > *w.end {
		w.end = &end
	}

	return obs, true, nil
}
