package pgctl

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Demote makes the cluster of cl a standby of the cluster of target: PATCH /config sets
// standby_cluster to the members of target. The leader of cl may be primary or already
// standby_leader, then pointed at target.
func (c *Controller) Demote(ctx context.Context, cl, target Cluster, opts Options) (Result, error) {
	d := &demote{c: c, cl: cl, target: target}

	return c.run(ctx, primitive{
		action: ActionDemote, dc: cl.Datacenter, timeout: c.cfg.Timeouts.Demote,
		prepare: d.prepare, send: d.send, resend: true, observe: d.observe,
	}, opts)
}

type demote struct {
	c          *Controller
	cl, target Cluster

	endpoint string
	// standby is the standby_cluster the request writes.
	standby map[string]any
	// oldLeader is the leader before the request: it has to be seen not primary.
	oldLeader string
}

func (d *demote) prepare(ctx context.Context, first bool) (Observation, error) {
	if d.cl.Datacenter == d.target.Datacenter {
		return Observation{}, precondition("%s cannot follow itself", d.cl.Datacenter)
	}

	members, err := d.cl.Patroni.Members(ctx)
	if err != nil {
		return Observation{Err: err}, err
	}

	obs := describe(members)

	l, err := d.c.target(ctx, d.cl, members, patroni.RolePrimary, patroni.RoleStandbyLeader)
	if err != nil {
		return obs, err
	}

	if first {
		targets, err := d.target.Patroni.Members(ctx)
		if err != nil {
			return obs, fmt.Errorf("patroni cluster of %s not read: %w", d.target.Datacenter, err)
		}

		d.standby, err = d.c.standbyCluster(ctx, d.target.Datacenter, targets)
		if err != nil {
			return obs, err
		}

		d.oldLeader = l.Name
	}

	d.endpoint = l.Endpoint

	return obs, nil
}

func (d *demote) send(ctx context.Context) error {
	_, err := d.cl.Patroni.PatchConfig(ctx, d.endpoint, map[string]any{"standby_cluster": d.standby})

	return err
}

func (d *demote) observe(ctx context.Context) (Observation, bool, error) {
	members, err := d.cl.Patroni.Members(ctx)
	if err != nil {
		return readFailed(d.cl.Datacenter, err)
	}

	obs := describe(members)

	l, ok := leaderOf(members)

	switch {
	case !ok:
		return obs.unmet(nil, "no leader")
	case l.Role != patroni.ClusterRoleStandbyLeader:
		return obs.unmet(nil, "leader %s is %s in the cluster", l.Name, l.Role)
	case l.Probe == nil:
		return obs.unmet(l.Err, "leader %s not read", l.Name)
	case l.Probe.Role != patroni.RoleStandbyLeader:
		return obs.unmet(nil, "leader %s is %s", l.Name, l.Probe.Role)
	}

	if old, ok := find(members, d.oldLeader); !ok || old.Probe == nil {
		return obs.unmet(old.Err, "old leader %s not read", d.oldLeader)
	}

	if i := slices.IndexFunc(members, isPrimary); i >= 0 {
		return obs.unmet(nil, "member %s is primary", members[i].Name)
	}

	// A leader that was standby_leader already meets the rest before the request lands.
	cfg, err := d.cl.Patroni.Config(ctx, l.Endpoint)
	if err != nil {
		return obs.unmet(err, "config not read")
	}

	if !pointsAt(cfg["standby_cluster"], d.standby) {
		return obs.unmet(nil, "standby_cluster does not point at %s yet", d.target.Datacenter)
	}

	primaries, err := d.c.healthyPrimaries(ctx, d.cl.Datacenter)
	if err != nil {
		return obs.unmet(err, "consul not read")
	}

	if len(primaries) > 0 {
		return obs.unmet(nil, "consul has a healthy primary at %v", slices.Sorted(maps.Keys(primaries)))
	}

	return obs, true, nil
}

// Promote makes the cluster of cl a primary: PATCH /config removes standby_cluster. The
// leader may be standby_leader, which then has to come up on a newer timeline, or a
// primary that still has standby_cluster after a demotion that did not take, which then
// has to stay primary; [Result.FromPrimary] tells the latter.
func (c *Controller) Promote(ctx context.Context, cl Cluster, opts Options) (Result, error) {
	p := &promote{c: c, cl: cl}

	res, err := c.run(ctx, primitive{
		action: ActionPromote, dc: cl.Datacenter, timeout: c.cfg.Timeouts.Promote,
		prepare: p.prepare, send: p.send, resend: true, observe: p.observe,
	}, opts)
	res.FromPrimary = p.fromPrimary

	return res, err
}

type promote struct {
	c  *Controller
	cl Cluster

	endpoint      string
	fromPrimary   bool
	startTimeline int64
}

func (p *promote) prepare(ctx context.Context, first bool) (Observation, error) {
	members, err := p.cl.Patroni.Members(ctx)
	if err != nil {
		return Observation{Err: err}, err
	}

	obs := describe(members)

	l, err := p.c.target(ctx, p.cl, members, patroni.RoleStandbyLeader, patroni.RolePrimary)
	if err != nil {
		return obs, err
	}

	if first {
		if l.Probe.Timeline == nil {
			return obs, fmt.Errorf("timeline of leader %s unknown", l.Name)
		}

		p.fromPrimary, p.startTimeline = l.Probe.Role == patroni.RolePrimary, *l.Probe.Timeline
	}

	p.endpoint = l.Endpoint

	return obs, nil
}

func (p *promote) send(ctx context.Context) error {
	_, err := p.cl.Patroni.PatchConfig(ctx, p.endpoint, map[string]any{"standby_cluster": nil})

	return err
}

func (p *promote) observe(ctx context.Context) (Observation, bool, error) {
	members, err := p.cl.Patroni.Members(ctx)
	if err != nil {
		return readFailed(p.cl.Datacenter, err)
	}

	obs := describe(members)

	l, ok := leaderOf(members)

	switch {
	case !ok:
		return obs.unmet(nil, "no leader")
	case l.Probe == nil:
		return obs.unmet(l.Err, "leader %s not read", l.Name)
	case l.Probe.Role != patroni.RolePrimary:
		return obs.unmet(nil, "leader %s is %s", l.Name, l.Probe.Role)
	case l.Probe.Timeline == nil:
		return obs.unmet(nil, "timeline of leader %s unknown", l.Name)
	case !p.fromPrimary && *l.Probe.Timeline <= p.startTimeline:
		return obs.unmet(nil, "leader %s still on timeline %d", l.Name, *l.Probe.Timeline)
	case *l.Probe.Timeline < p.startTimeline:
		return obs.unmet(nil, "leader %s on timeline %d, older than %d", l.Name, *l.Probe.Timeline, p.startTimeline)
	}

	if isPrim, err := p.cl.Patroni.Primary(ctx, l.Endpoint); err != nil || !isPrim {
		return obs.unmet(err, "GET /primary of %s is not 200", l.Name)
	}

	cfg, err := p.cl.Patroni.Config(ctx, l.Endpoint)
	if err != nil {
		return obs.unmet(err, "config not read")
	}

	if cfg["standby_cluster"] != nil {
		return obs.unmet(nil, "standby_cluster still set")
	}

	if detail, err := p.c.soloPrimary(ctx, p.cl.Datacenter, members, l); detail != "" {
		return obs.unmet(err, "%s", detail)
	}

	return obs, true, nil
}
