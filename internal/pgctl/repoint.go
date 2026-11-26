package pgctl

import (
	"context"
	"fmt"
	"slices"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Repoint points the standby cluster of standby at the cluster of primary: PATCH /config
// sets standby_cluster to the members of primary. A standby whose leader is primary is
// refused, the change would demote it, and so is a primary side whose leader is
// standby_leader.
func (c *Controller) Repoint(ctx context.Context, standby, primary Cluster, opts Options) (Result, error) {
	f := &follow{c: c, standby: standby, primary: primary}

	return c.run(ctx, primitive{
		action: ActionRepoint, dc: standby.Datacenter, timeout: c.cfg.Timeouts.Repoint,
		prepare: f.prepare, send: f.send, resend: true, observe: f.observe,
	}, opts)
}

// Follow waits for the standby cluster of standby, already pointed at primary, to stream
// from it: the old primary after its demotion, which Patroni rewinds when it diverged.
// The condition is that of [Controller.Repoint], within patroni.timeouts.rewind. It sends
// nothing and takes no guard.
func (c *Controller) Follow(ctx context.Context, standby, primary Cluster, opts Options) (Result, error) {
	f := &follow{c: c, standby: standby, primary: primary}

	return c.run(ctx, primitive{
		action: ActionFollow, dc: standby.Datacenter, timeout: c.cfg.Timeouts.Rewind, observe: f.observe,
	}, opts)
}

type follow struct {
	c                *Controller
	standby, primary Cluster

	endpoint string
	patch    map[string]any
}

func (f *follow) prepare(ctx context.Context, first bool) (Observation, error) {
	if f.standby.Datacenter == f.primary.Datacenter {
		return Observation{}, precondition("%s cannot follow itself", f.standby.Datacenter)
	}

	members, err := f.standby.Patroni.Members(ctx)
	if err != nil {
		return Observation{Err: err}, err
	}

	obs := describe(members)

	l, err := f.c.target(ctx, f.standby, members, patroni.RoleStandbyLeader)
	if err != nil {
		return obs, err
	}

	primaries, err := f.primary.Patroni.Members(ctx)
	if err != nil {
		return obs, fmt.Errorf("patroni cluster of %s not read: %w", f.primary.Datacenter, err)
	}

	if err := primaryLeader(f.primary.Datacenter, primaries); err != nil {
		return obs, err
	}

	if first {
		sc, err := f.c.standbyCluster(ctx, f.primary.Datacenter, primaries)
		if err != nil {
			return obs, err
		}

		f.patch = map[string]any{"standby_cluster": sc}
	}

	f.endpoint = l.Endpoint

	return obs, nil
}

func (f *follow) send(ctx context.Context) error {
	_, err := f.standby.Patroni.PatchConfig(ctx, f.endpoint, f.patch)

	return err
}

// primaryLeader checks that the leader of members, the cluster of dc, is primary. A
// standby_leader there fails the precondition; a leader not read or in transition is
// retried.
func primaryLeader(dc string, members []patroni.Member) error {
	l, ok := leaderOf(members)

	switch {
	case !ok:
		return fmt.Errorf("%s: %w", dc, errNoLeader)
	case l.Probe == nil:
		return usable(l)
	case l.Probe.Role == patroni.RoleStandbyLeader:
		return precondition("leader %s of %s is standby_leader, not primary", l.Name, dc)
	case l.Probe.Role != patroni.RolePrimary:
		return fmt.Errorf("leader %s of %s is %s", l.Name, dc, l.Probe.Role)
	}

	return nil
}

// observe checks that the standby leader streams on the timeline of the primary leader,
// and that the primary streams to it.
func (f *follow) observe(ctx context.Context) (Observation, bool, error) {
	members, err := f.standby.Patroni.Members(ctx)
	if err != nil {
		return readFailed(f.standby.Datacenter, err)
	}

	obs := describe(members)

	sl, ok := leaderOf(members)
	if !ok {
		return obs.unmet(nil, "%s has no leader", f.standby.Datacenter)
	}

	if sl.Probe == nil {
		return obs.unmet(sl.Err, "standby leader %s not read", sl.Name)
	}

	primaries, err := f.primary.Patroni.Members(ctx)
	if err != nil {
		return obs.unmet(err, "patroni cluster of %s not read", f.primary.Datacenter)
	}

	pl, ok := leaderOf(primaries)
	if !ok {
		return obs.unmet(nil, "%s has no leader", f.primary.Datacenter)
	}

	if pl.Probe == nil {
		return obs.unmet(pl.Err, "primary %s not read", pl.Name)
	}

	s, p := sl.Probe, pl.Probe
	if p.Xlog.Location != nil && s.Xlog.ReceivedLocation != nil {
		obs.Lag = new(max(0, *p.Xlog.Location-*s.Xlog.ReceivedLocation))
	}

	switch {
	case s.Role != patroni.RoleStandbyLeader:
		return obs.unmet(nil, "leader %s is %s", sl.Name, s.Role)
	case s.ReplicationState != patroni.ReplicationStreaming:
		return obs.unmet(nil, "standby leader %s replication is %q", sl.Name, s.ReplicationState)
	case p.Role != patroni.RolePrimary:
		return obs.unmet(nil, "leader %s of %s is %s", pl.Name, f.primary.Datacenter, p.Role)
	case s.Timeline == nil || p.Timeline == nil:
		return obs.unmet(nil, "timeline of %s or %s unknown", sl.Name, pl.Name)
	case *s.Timeline != *p.Timeline:
		return obs.unmet(nil, "standby leader %s on timeline %d, primary %s on %d", sl.Name, *s.Timeline, pl.Name, *p.Timeline)
	}

	streams := slices.ContainsFunc(p.Replication, func(r patroni.Replica) bool {
		return r.ClientAddr == sl.Host && r.State == patroni.ReplicationStreaming
	})
	if !streams {
		return obs.unmet(nil, "primary %s does not stream to %s", pl.Name, sl.Host)
	}

	return obs, true, nil
}
