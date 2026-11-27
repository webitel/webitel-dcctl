package pgctl

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Placed is a member of the cluster of stretch with the datacenter it runs in.
type Placed struct {
	patroni.Member

	// Datacenter is empty when no datacenter of the read has an instance of the scope
	// on the host of the member.
	Datacenter string
}

// Placement is the cluster of stretch as one read sees it.
type Placement struct {
	Members []Placed
	// Leader is the member that leads by GET /cluster; nil when the cluster has none.
	Leader *Placed
	// Config is GET /config of the leader or, without one, of the first member that
	// answers it; nil when none does.
	Config map[string]any
	// Primaries are the hosts of the healthy primary instances of the scope in Consul,
	// by datacenter; under "" those on a host of no datacenter of the read.
	Primaries map[string][]string
}

// Primary returns the members that report primary by their own GET /patroni.
func (p Placement) Primary() []Placed {
	var out []Placed

	for _, m := range p.Members {
		if isPrimary(m.Member) {
			out = append(out, m)
		}
	}

	return out
}

// Place reads once, through cl, the cluster that spans dcs: its members by GET /cluster
// with their own GET /patroni, the datacenter each one runs in by the instances of the
// scope in the Consul of dcs, the dynamic configuration, and the healthy primary
// instances of the whole cluster in Consul. It fails when the members or Consul are not
// read, when a host is an instance in two datacenters and when more than one member
// leads. It sends nothing and takes no guard.
func (c *Controller) Place(ctx context.Context, cl Cluster, dcs []string) (Placement, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	members, err := cl.Patroni.Members(ctx)
	if err != nil {
		return Placement{}, fmt.Errorf("patroni cluster of %s not read: %w", cl.Datacenter, err)
	}

	where := make(map[string]string)
	p := Placement{Primaries: make(map[string][]string)}

	for _, dc := range dcs {
		hosts, err := c.instances(ctx, dc)
		if err != nil {
			return Placement{}, err
		}

		for h := range hosts {
			if other, ok := where[h]; ok {
				return Placement{}, fmt.Errorf("host %s is an instance of %s in both %s and %s", h, c.cfg.Scope, other, dc)
			}

			where[h] = dc
		}
	}

	addrs, err := c.catalog.ClusterHealthyAddrs(ctx, cl.Datacenter, c.cfg.Scope, tagPrimary)
	if err != nil {
		return Placement{}, fmt.Errorf("read primary of %s: %w", c.cfg.Scope, err)
	}

	for _, h := range slices.Sorted(maps.Keys(hostSet(addrs))) {
		p.Primaries[where[h]] = append(p.Primaries[where[h]], h)
	}

	leaders := 0

	for _, m := range members {
		p.Members = append(p.Members, Placed{Member: m, Datacenter: where[m.Host]})

		if m.Role == patroni.ClusterRoleLeader || m.Role == patroni.ClusterRoleStandbyLeader {
			leaders++
		}
	}

	if leaders > 1 {
		return Placement{}, fmt.Errorf("%d members lead the cluster", leaders)
	}

	if l, ok := leaderOf(members); ok {
		i := slices.IndexFunc(p.Members, func(m Placed) bool { return m.Name == l.Name })
		p.Leader = &p.Members[i]
	}

	p.Config = readConfig(ctx, cl, p)

	return p, nil
}

// readConfig returns GET /config of the leader of p, or of the first member that
// answers when there is no leader; nil when none does.
func readConfig(ctx context.Context, cl Cluster, p Placement) map[string]any {
	order := slices.Clone(p.Members)
	if p.Leader != nil {
		order = []Placed{*p.Leader}
	}

	for _, m := range order {
		if m.Endpoint == "" {
			continue
		}

		if cfg, err := cl.Patroni.Config(ctx, m.Endpoint); err == nil && cfg != nil {
			return cfg
		}
	}

	return nil
}

// Rejoin waits, within rewind, until every member of cl.Datacenter that answered the
// first read is a replica that streams on the timeline of the leader, as the old
// leader of a switchover does once Patroni rewound it. The members that did not answer
// that read are in [Result.Unread], the hosts of the instances of the scope in
// cl.Datacenter that GET /cluster does not list in [Result.Missing]; neither is waited
// for. It sends nothing and takes no guard.
func (c *Controller) Rejoin(ctx context.Context, cl Cluster, opts Options) (Result, error) {
	w := &rejoin{c: c, cl: cl}

	res, err := c.run(ctx, primitive{
		action: ActionRejoin, dc: cl.Datacenter, timeout: c.cfg.Timeouts.Rewind, observe: w.observe,
	}, opts)
	res.Unread, res.Missing = w.unread, w.missing

	return res, err
}

type rejoin struct {
	c  *Controller
	cl Cluster
	// waited are the members of the datacenter the first read found answering; nil
	// until a read succeeds. unread are those it found not answering, missing the hosts
	// of the datacenter it found no member on.
	waited  []string
	unread  []string
	missing []string
}

func (w *rejoin) observe(ctx context.Context) (Observation, bool, error) {
	members, err := w.cl.Patroni.Members(ctx)
	if err != nil {
		return readFailed(w.cl.Datacenter, err)
	}

	obs := describe(members)

	if w.waited == nil {
		hosts, err := w.c.instances(ctx, w.cl.Datacenter)
		if err != nil {
			return obs.unmet(err, "instances of %s not read", w.cl.Datacenter)
		}

		w.waited = make([]string, 0, len(members))
		listed := make(map[string]bool, len(members))

		for _, m := range members {
			listed[m.Host] = true

			switch {
			case !hosts[m.Host]:
			case m.Probe == nil:
				w.unread = append(w.unread, m.Name)
			default:
				w.waited = append(w.waited, m.Name)
			}
		}

		for _, h := range slices.Sorted(maps.Keys(hosts)) {
			if !listed[h] {
				w.missing = append(w.missing, h)
			}
		}
	}

	l, ok := leaderOf(members)

	switch {
	case !ok:
		return obs.unmet(nil, "no leader")
	case slices.Contains(w.waited, l.Name) || slices.Contains(w.unread, l.Name):
		return obs.unmet(nil, "leader %s runs in %s", l.Name, w.cl.Datacenter)
	case l.Probe == nil || l.Probe.Role != patroni.RolePrimary || l.Probe.Timeline == nil:
		return obs.unmet(l.Err, "leader %s is not a primary on a known timeline", l.Name)
	}

	var lagging []string

	for _, name := range w.waited {
		m, ok := find(members, name)
		if !ok || !followsOn(m, *l.Probe.Timeline) {
			lagging = append(lagging, name)
		}
	}

	if len(lagging) > 0 {
		return obs.unmet(nil, "not streaming on timeline %d: %s", *l.Probe.Timeline, strings.Join(lagging, ", "))
	}

	return obs, true, nil
}

// followsOn reports whether m is a replica that streams on timeline.
func followsOn(m patroni.Member, timeline int64) bool {
	p := m.Probe

	return p != nil && p.Role == patroni.RoleReplica && p.ReplicationState == patroni.ReplicationStreaming &&
		p.Timeline != nil && *p.Timeline == timeline
}
