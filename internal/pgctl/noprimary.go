package pgctl

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// PrimaryError is a primary found where a primitive needs none. It matches
// [ErrPrecondition].
type PrimaryError struct {
	Datacenter string
	// Where is the member that reports primary, or the Consul instance.
	Where string
}

func (e *PrimaryError) Error() string {
	return fmt.Sprintf("%s: %s of %s is primary", ErrPrecondition, e.Where, e.Datacenter)
}

// Is matches [ErrPrecondition].
func (e *PrimaryError) Is(target error) bool { return target == ErrPrecondition }

// NoPrimary waits, within catchup, until no cluster of clusters and others has a primary.
// A cluster has none when its leader is read and is not primary, no member that was read
// reports primary, and Consul has no healthy primary instance in its datacenter.
//
// Every cluster of clusters must be read; one that is not leaves the condition unmet.
// A cluster of others that is not read does not: [Result.Unread] names it. A primary
// found in any of them ends the primitive at once with [*PrimaryError]. It sends nothing
// and takes no guard.
func (c *Controller) NoPrimary(ctx context.Context, clusters, others []Cluster, opts Options) (Result, error) {
	names := make([]string, len(clusters))
	for i, cl := range clusters {
		names[i] = cl.Datacenter
	}

	w := &noPrimary{c: c, clusters: clusters, others: others}

	res, err := c.run(ctx, primitive{
		action: ActionNoPrimary, dc: strings.Join(names, ","), timeout: c.cfg.Timeouts.Catchup, observe: w.observe,
	}, opts)
	if err == nil {
		res.Unread = w.unread
	}

	return res, err
}

type noPrimary struct {
	c        *Controller
	clusters []Cluster
	others   []Cluster
	// unread are the clusters of others the last poll did not read.
	unread []string
}

func (w *noPrimary) observe(ctx context.Context) (Observation, bool, error) {
	for _, cl := range w.clusters {
		if obs, ok, err := w.read(ctx, cl); !ok || err != nil {
			return obs, false, err
		}
	}

	w.unread = nil

	for _, cl := range w.others {
		obs, ok, err := w.read(ctx, cl)
		if err != nil {
			return obs, false, err
		}

		if !ok {
			w.unread = append(w.unread, cl.Datacenter)
		}
	}

	return Observation{}, true, nil
}

// read reads cl: ok is false when it is not read, the error is the primary found.
func (w *noPrimary) read(ctx context.Context, cl Cluster) (Observation, bool, error) {
	members, err := cl.Patroni.Members(ctx)
	if err != nil {
		return readFailed(cl.Datacenter, err)
	}

	obs := describe(members)

	if i := slices.IndexFunc(members, isPrimary); i >= 0 {
		return obs, false, &PrimaryError{Datacenter: cl.Datacenter, Where: "member " + members[i].Name}
	}

	l, ok := leaderOf(members)

	switch {
	case !ok:
		return obs.unmet(nil, "%s has no leader", cl.Datacenter)
	case l.Probe == nil:
		return obs.unmet(l.Err, "leader %s of %s not read", l.Name, cl.Datacenter)
	}

	primaries, err := w.c.healthyPrimaries(ctx, cl.Datacenter)
	if err != nil {
		return obs.unmet(err, "consul of %s not read", cl.Datacenter)
	}

	if len(primaries) > 0 {
		return obs, false, &PrimaryError{
			Datacenter: cl.Datacenter,
			Where:      fmt.Sprintf("consul instance %v", slices.Sorted(maps.Keys(primaries))),
		}
	}

	return obs, true, nil
}
