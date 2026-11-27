package pgctl

import (
	"context"
	"fmt"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Leader reads the leader of the cluster of cl once, by GET /cluster with its own GET
// /patroni. It fails when the members are not read, when the cluster has no leader and
// when the leader does not answer. It sends nothing and takes no guard.
func (c *Controller) Leader(ctx context.Context, cl Cluster) (patroni.Member, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	return readLeader(ctx, cl)
}

// Divergence reads whether the standby cluster of standby left the history of the
// primary cluster of primary, see [patroni.Diverge]: a standby that diverged follows
// primary only once the WAL past the divergence is discarded. A standby whose leader is
// primary fails with [*PrimaryError], a primary side whose leader is not primary with
// [ErrPrecondition]; it fails too when what it reads does not tell. It sends nothing and
// takes no guard.
func (c *Controller) Divergence(ctx context.Context, standby, primary Cluster) (patroni.Divergence, error) {
	return divergence(ctx, standby, primary, false)
}

// DivergenceOf is [Controller.Divergence] for a cluster whose leader may be primary, as
// an old primary to demote: what it holds past the point it left the history of the
// primary cluster of primary is what following primary discards. A leader that is
// primary on the timeline of primary fails: the histories do not tell its WAL apart.
func (c *Controller) DivergenceOf(ctx context.Context, cl, primary Cluster) (patroni.Divergence, error) {
	return divergence(ctx, cl, primary, true)
}

// divergence reads the divergence of standby from primary; a standby whose leader is
// primary fails with [*PrimaryError] unless leaderPrimary allows it.
func divergence(ctx context.Context, standby, primary Cluster, leaderPrimary bool) (patroni.Divergence, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	sl, err := readLeader(ctx, standby)
	if err != nil {
		return patroni.Divergence{}, err
	}

	if sl.Probe.Role == patroni.RolePrimary && !leaderPrimary {
		return patroni.Divergence{}, &PrimaryError{Datacenter: standby.Datacenter, Where: "leader " + sl.Name}
	}

	pl, err := readLeader(ctx, primary)
	if err != nil {
		return patroni.Divergence{}, err
	}

	switch {
	case pl.Probe.Role != patroni.RolePrimary:
		return patroni.Divergence{}, precondition("leader %s of %s is %s, not primary", pl.Name, primary.Datacenter, pl.Probe.Role)
	case sl.Probe.Timeline == nil || pl.Probe.Timeline == nil:
		return patroni.Divergence{}, fmt.Errorf("timeline of %s or %s unknown", sl.Name, pl.Name)
	case sl.Probe.Role == patroni.RolePrimary && *sl.Probe.Timeline == *pl.Probe.Timeline:
		// Two primaries on one timeline were promoted apart: the histories may match while
		// the WAL past the promotion differs, and a rewind would not be done.
		return patroni.Divergence{}, fmt.Errorf("%s and %s are primary on the same timeline %d, what %s discards is not known; "+
			"rebuild %s by hand", standby.Datacenter, primary.Datacenter, *pl.Probe.Timeline, standby.Datacenter, standby.Datacenter)
	}

	hp, err := primary.Patroni.History(ctx, pl.Endpoint)
	if err != nil {
		return patroni.Divergence{}, fmt.Errorf("history of %s not read: %w", primary.Datacenter, err)
	}

	hs, err := standby.Patroni.History(ctx, sl.Endpoint)
	if err != nil {
		return patroni.Divergence{}, fmt.Errorf("history of %s not read: %w", standby.Datacenter, err)
	}

	d, ok := patroni.Diverge(*pl.Probe.Timeline, hp, *sl.Probe.Timeline, hs, sl.Probe.WALPosition())
	if !ok {
		return d, fmt.Errorf("the histories of %s and %s do not tell whether %s diverged; rebuild %s by hand",
			primary.Datacenter, standby.Datacenter, standby.Datacenter, standby.Datacenter)
	}

	return d, nil
}

// DemoteLoss reads how many bytes of WAL the primary cluster of primary holds beyond what
// the standby cluster of standby received, see [patroni.DemoteLoss]: demoting primary to
// follow standby discards them. It fails when what it reads does not tell. It sends
// nothing and takes no guard.
func (c *Controller) DemoteLoss(ctx context.Context, primary, standby Cluster) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	pl, err := readLeader(ctx, primary)
	if err != nil {
		return 0, err
	}

	sl, err := readLeader(ctx, standby)
	if err != nil {
		return 0, err
	}

	loc, recv := pl.Probe.Xlog.Location, sl.Probe.Xlog.ReceivedLocation

	switch {
	case loc == nil:
		return 0, fmt.Errorf("the WAL location of %s is not known", pl.Name)
	case recv == nil:
		return 0, fmt.Errorf("the WAL %s received is not known", sl.Name)
	case pl.Probe.Timeline == nil || sl.Probe.Timeline == nil:
		return 0, fmt.Errorf("timeline of %s or %s unknown", pl.Name, sl.Name)
	}

	var history []patroni.HistoryEntry

	if *pl.Probe.Timeline != *sl.Probe.Timeline {
		history, err = primary.Patroni.History(ctx, pl.Endpoint)
		if err != nil {
			return 0, fmt.Errorf("history of %s not read: %w", primary.Datacenter, err)
		}
	}

	n, ok := patroni.DemoteLoss(*pl.Probe.Timeline, history, *loc, *sl.Probe.Timeline, *recv)
	if !ok {
		return 0, fmt.Errorf("where %s left timeline %d of %s is not known", primary.Datacenter, *sl.Probe.Timeline, standby.Datacenter)
	}

	return n, nil
}

// Demoted reads whether the cluster of cl is a standby of the cluster of target already:
// its leader is standby_leader and every host of its standby_cluster is an instance of
// the scope in the datacenter of target, as [Controller.Demote] leaves it. It sends
// nothing and takes no guard.
func (c *Controller) Demoted(ctx context.Context, cl, target Cluster) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	l, err := readLeader(ctx, cl)
	if err != nil {
		return false, err
	}

	if l.Probe.Role != patroni.RoleStandbyLeader {
		return false, nil
	}

	cfg, err := cl.Patroni.Config(ctx, l.Endpoint)
	if err != nil {
		return false, fmt.Errorf("config of %s not read: %w", cl.Datacenter, err)
	}

	hosts, err := c.instances(ctx, target.Datacenter)
	if err != nil {
		return false, err
	}

	sc, ok := cfg["standby_cluster"].(map[string]any)
	if !ok {
		return false, nil
	}

	list, ok := sc["host"].(string)
	if !ok || list == "" {
		return false, nil
	}

	for h := range strings.SplitSeq(list, ",") {
		if !hosts[strings.TrimSpace(h)] {
			return false, nil
		}
	}

	return true, nil
}

// readLeader returns the leader of cl with its GET /patroni.
func readLeader(ctx context.Context, cl Cluster) (patroni.Member, error) {
	members, err := cl.Patroni.Members(ctx)
	if err != nil {
		return patroni.Member{}, fmt.Errorf("patroni cluster of %s not read: %w", cl.Datacenter, err)
	}

	l, ok := leaderOf(members)
	if !ok {
		return l, fmt.Errorf("%s: %w", cl.Datacenter, errNoLeader)
	}

	if err := usable(l); err != nil {
		return l, fmt.Errorf("leader of %s: %w", cl.Datacenter, err)
	}

	return l, nil
}
