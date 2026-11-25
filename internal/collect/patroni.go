package collect

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// errNoLeader is a cluster without a leader to read /config and /history from.
var errNoLeader = fmt.Errorf("patroni cluster has no leader: %w", source.ErrNoEndpoints)

// readPatroni reads the members, then /config and /history of the leader at once. prev
// holds the last known /config and /history.
func readPatroni(ctx context.Context, r PatroniReader, prev Patroni) (Patroni, error) {
	members, err := r.Members(ctx)
	if err != nil {
		return Patroni{}, err
	}

	p := Patroni{Members: make([]Member, len(members))}
	for i, m := range members {
		p.Members[i] = Member{Member: m}
	}

	endpoint, leaderErr := "", errNoLeader

	if i := slices.IndexFunc(members, isLeader); i >= 0 {
		p.Leader = members[i].Name
		endpoint, leaderErr = leaderEndpoint(members[i])
	}

	var (
		config     map[string]any
		history    []patroni.HistoryEntry
		configErr  = leaderErr
		historyErr = leaderErr
	)

	if leaderErr == nil {
		var wg sync.WaitGroup

		wg.Go(func() { config, configErr = r.Config(ctx, endpoint) })
		wg.Go(func() { history, historyErr = r.History(ctx, endpoint) })
		wg.Wait()
	}

	now := time.Now()
	p.Config = observe(prev.Config, config, configErr, now)
	p.History = observe(prev.History, history, historyErr, now)

	return p, nil
}

// observePatroni is [observe] for Patroni: when the members cannot be read, /config and
// /history fail with them, keeping their last known data.
func observePatroni(prev Source[Patroni], data Patroni, err error, now time.Time) Source[Patroni] {
	s := observe(prev, data, err, now)
	if err != nil {
		s.Data.Config = observe(prev.Data.Config, nil, err, now)
		s.Data.History = observe(prev.Data.History, nil, err, now)
	}

	return s
}

func isLeader(m patroni.Member) bool {
	return m.Role == patroni.ClusterRoleLeader || m.Role == patroni.ClusterRoleStandbyLeader
}

func leaderEndpoint(m patroni.Member) (string, error) {
	switch {
	case m.Endpoint != "":
		return m.Endpoint, nil
	case m.Err != nil:
		return "", m.Err
	default:
		return "", fmt.Errorf("patroni leader %q has no https api_url: %w", m.Name, source.ErrNoEndpoints)
	}
}

// placeMembers sets the datacenter of every member of cl. A cluster of one datacenter runs
// there; in a cluster over several, a member runs where the node of the scope instance on
// its host is, by node_meta.
func placeMembers(cl *Cluster, nodeMetaKey string) {
	src := cl.Patroni.Data.Members
	if len(src) == 0 {
		return
	}

	// Last known members are shared with the previous snapshot, which must not change.
	members := slices.Clone(src)

	if len(cl.Datacenters) == 1 {
		for i := range members {
			members[i].Datacenter = cl.Datacenters[0]
		}
	} else {
		byHost := hostDatacenters(cl.Scope.Data.Nodes, nodeMetaKey)
		for i := range members {
			members[i].Datacenter = byHost[members[i].Host]
		}
	}

	cl.Patroni.Data.Members = members
}

// hostDatacenters maps the addresses of the instances to the datacenter of their node.
func hostDatacenters(instances []consul.ServiceNode, nodeMetaKey string) map[string]string {
	out := make(map[string]string, len(instances))

	for _, n := range instances {
		dc := n.NodeMeta[nodeMetaKey]
		if dc == "" {
			continue
		}

		for _, host := range []string{n.Address, n.NodeAddress} {
			if host != "" {
				out[host] = dc
			}
		}
	}

	return out
}
