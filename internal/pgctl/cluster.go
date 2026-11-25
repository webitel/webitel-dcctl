package pgctl

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

var (
	errNoLeader = errors.New("cluster has no leader")
	errNoHosts  = errors.New("cluster has no member with a host")
)

// leaderOf returns the only member that leads by GET /cluster.
func leaderOf(members []patroni.Member) (patroni.Member, bool) {
	var (
		out patroni.Member
		n   int
	)

	for _, m := range members {
		if m.Role == patroni.ClusterRoleLeader || m.Role == patroni.ClusterRoleStandbyLeader {
			out = m
			n++
		}
	}

	return out, n == 1
}

func find(members []patroni.Member, name string) (patroni.Member, bool) {
	i := slices.IndexFunc(members, func(m patroni.Member) bool { return m.Name == name })
	if i < 0 {
		return patroni.Member{}, false
	}

	return members[i], true
}

// describe is the observation of members about their leader.
func describe(members []patroni.Member) Observation {
	var obs Observation

	if l, ok := leaderOf(members); ok {
		obs.Leader = l.Name
		obs = obs.about(l)
	}

	return obs
}

// about returns the observation about m, with its own role and timeline.
func (o Observation) about(m patroni.Member) Observation {
	o.Member, o.Role, o.Timeline = m.Name, "", nil

	if m.Probe != nil {
		o.Role, o.Timeline = m.Probe.Role, m.Probe.Timeline
	}

	return o
}

// miss returns the observation of a condition that is not met.
func (o Observation) miss(err error, format string, args ...any) Observation {
	o.Detail, o.Err = fmt.Sprintf(format, args...), err

	return o
}

// unmet is [Observation.miss] as the result of an observe.
func (o Observation) unmet(err error, format string, args ...any) (Observation, bool, error) {
	return o.miss(err, format, args...), false, nil
}

// readFailed is the observation of a cluster that could not be read.
func readFailed(dc string, err error) (Observation, bool, error) {
	return Observation{}.unmet(err, "patroni cluster of %s not read", dc)
}

func precondition(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPrecondition, fmt.Sprintf(format, args...))
}

// instances returns the hosts of the instances of the scope registered in dc.
func (c *Controller) instances(ctx context.Context, dc string) (map[string]bool, error) {
	addrs, err := c.catalog.RegisteredAddrs(ctx, dc, c.cfg.Scope)
	if err != nil {
		return nil, fmt.Errorf("read instances of %s in %s: %w", c.cfg.Scope, dc, err)
	}

	return hostSet(addrs), nil
}

// healthyPrimaries returns the hosts of the passing instances of the scope tagged primary in dc.
func (c *Controller) healthyPrimaries(ctx context.Context, dc string) (map[string]bool, error) {
	addrs, err := c.catalog.HealthyAddrs(ctx, dc, c.cfg.Scope, tagPrimary)
	if err != nil {
		return nil, fmt.Errorf("read primary of %s in %s: %w", c.cfg.Scope, dc, err)
	}

	return hostSet(addrs), nil
}

func hostSet(addrs []string) map[string]bool {
	out := make(map[string]bool, len(addrs))

	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			host = a
		}

		out[host] = true
	}

	return out
}

// inDatacenter checks that m runs in dc: Patroni registers its conn_url host as the
// instance of the scope in the Consul of its datacenter. In warm_standby every datacenter
// runs a cluster of the same scope, and this is what tells them apart.
func (c *Controller) inDatacenter(ctx context.Context, dc string, m patroni.Member) error {
	hosts, err := c.instances(ctx, dc)
	if err != nil {
		return err
	}

	if !hosts[m.Host] {
		return precondition("member %s at %q is not an instance of %s in %s", m.Name, m.Host, c.cfg.Scope, dc)
	}

	return nil
}

// target returns the leader of members to send a request to. It must run in cl, take
// the request at its own host, answer GET /patroni with one of roles and run a supported
// Patroni. A leader that is primary where a standby is required, or that cannot be talked
// to at all, fails the precondition; one that is not read or is in another role is
// retried.
func (c *Controller) target(ctx context.Context, cl Cluster, members []patroni.Member, roles ...patroni.Role) (patroni.Member, error) {
	l, ok := leaderOf(members)
	if !ok {
		return l, errNoLeader
	}

	if err := ownEndpoint(l); err != nil {
		return l, err
	}

	if err := c.inDatacenter(ctx, cl.Datacenter, l); err != nil {
		return l, err
	}

	if err := usable(l); err != nil {
		return l, err
	}

	if !slices.Contains(roles, l.Probe.Role) {
		if l.Probe.Role == patroni.RolePrimary {
			return l, precondition("leader %s is primary", l.Name)
		}

		return l, fmt.Errorf("leader %s is %s", l.Name, l.Probe.Role)
	}

	return l, nil
}

// ownEndpoint checks that the REST API of m, where a request goes, is on the host the
// datacenter check is made for.
func ownEndpoint(m patroni.Member) error {
	host, _, err := net.SplitHostPort(m.Endpoint)
	if err != nil || host != m.Host {
		return precondition("member %s has no https api_url on its host %q", m.Name, m.Host)
	}

	return nil
}

// usable checks that m answers GET /patroni and runs a supported Patroni.
func usable(m patroni.Member) error {
	switch {
	case m.UnusableReason == patroni.UnusableVersion:
		return precondition("member %s runs an unsupported Patroni, %s or newer is required", m.Name, patroni.MinVersion)
	case m.Probe == nil && m.Err != nil:
		return fmt.Errorf("member %s not read: %w", m.Name, m.Err)
	case m.Probe == nil:
		return fmt.Errorf("member %s not read", m.Name)
	}

	return nil
}

// soloPrimary returns what keeps m from being the only primary: another member that
// reports primary, or any other healthy primary instance of the scope in dc. It is empty
// when m is the only one.
func (c *Controller) soloPrimary(ctx context.Context, dc string, members []patroni.Member, m patroni.Member) (string, error) {
	if i := slices.IndexFunc(members, func(o patroni.Member) bool { return o.Name != m.Name && isPrimary(o) }); i >= 0 {
		return fmt.Sprintf("member %s is primary too", members[i].Name), nil
	}

	primaries, err := c.healthyPrimaries(ctx, dc)
	if err != nil {
		return "consul not read", err
	}

	if len(primaries) != 1 || !primaries[m.Host] {
		return fmt.Sprintf("consul has healthy primary at %v, want only %s", slices.Sorted(maps.Keys(primaries)), m.Host), nil
	}

	return "", nil
}

func isPrimary(m patroni.Member) bool {
	return m.Probe != nil && m.Probe.Role == patroni.RolePrimary
}

// standbyCluster returns standby_cluster pointing at members, the cluster of dc. PATCH
// /config is a merge patch, so every key is written. Without replication slots Patroni
// does not ask for a read-write server, and the standby leader streams from the first
// host that accepts it: the current leader goes first, then the others by
// failover_priority, highest first.
func (c *Controller) standbyCluster(ctx context.Context, dc string, members []patroni.Member) (map[string]any, error) {
	hosts, err := c.instances(ctx, dc)
	if err != nil {
		return nil, err
	}

	leader, _ := leaderOf(members)

	members = slices.Clone(members)
	slices.SortFunc(members, func(a, b patroni.Member) int {
		return cmp.Or(
			cmp.Compare(b2i(b.Name == leader.Name), b2i(a.Name == leader.Name)),
			cmp.Compare(b.FailoverPriority, a.FailoverPriority),
			strings.Compare(a.Name, b.Name),
		)
	})

	var list []string

	for _, m := range members {
		if !hosts[m.Host] {
			return nil, precondition("member %s at %q is not an instance of %s in %s", m.Name, m.Host, c.cfg.Scope, dc)
		}

		if !slices.Contains(list, m.Host) {
			list = append(list, m.Host)
		}
	}

	if len(list) == 0 {
		return nil, fmt.Errorf("%s: %w", dc, errNoHosts)
	}

	return map[string]any{
		"host":                   strings.Join(list, ","),
		"port":                   postgresPort,
		"create_replica_methods": []string{"basebackup"},
	}, nil
}

// pointsAt reports whether got, standby_cluster read from GET /config, has the host and
// the port of want.
func pointsAt(got any, want map[string]any) bool {
	sc, ok := got.(map[string]any)
	if !ok {
		return false
	}

	host, hostOK := sc["host"].(string)
	port, portOK := asInt(sc["port"])

	return hostOK && portOK && host == want["host"] && port == want["port"]
}

// asInt reads a port from decoded JSON, a float64, or from a value as it was written.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), n == float64(int(n))
	}

	return 0, false
}

func b2i(b bool) int {
	if b {
		return 1
	}

	return 0
}
