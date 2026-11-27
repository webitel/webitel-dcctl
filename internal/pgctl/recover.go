package pgctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// ErrNotRecovered means a standby cluster does not follow its primary in full: a member
// or an instance of the scope does not stream on the timeline of the primary.
var ErrNotRecovered = errors.New("standby cluster does not follow in full")

// Recover waits, within rewind, until the standby cluster of standby follows the primary
// cluster of primary in full: its standby leader as [Controller.Follow] waits for, and
// every other member a replica that streams on the timeline of the primary. A member that
// does not answer, and an instance of the scope in the datacenter of standby that GET
// /cluster does not list, does not stream. It sends nothing and takes no guard.
func (c *Controller) Recover(ctx context.Context, standby, primary Cluster, opts Options) (Result, error) {
	r := &recovery{c: c, follow: follow{c: c, standby: standby, primary: primary}}

	return c.run(ctx, primitive{
		action: ActionFollow, dc: standby.Datacenter, timeout: c.cfg.Timeouts.Rewind, observe: r.observe,
	}, opts)
}

// Recovered reads once what [Controller.Recover] waits for. It fails with
// [ErrNotRecovered] and what is missing when the standby cluster of standby does not
// follow the primary cluster of primary in full, and with the error of a read that
// fails. It sends nothing and takes no guard.
func (c *Controller) Recovered(ctx context.Context, standby, primary Cluster) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	r := &recovery{c: c, follow: follow{c: c, standby: standby, primary: primary}}

	obs, done, err := r.observe(ctx)

	switch {
	case err != nil:
		return err
	case done:
		return nil
	case obs.Err != nil:
		return fmt.Errorf("%s: %s: %w", standby.Datacenter, obs.Detail, obs.Err)
	}

	return fmt.Errorf("%s: %s: %w", standby.Datacenter, obs.Detail, ErrNotRecovered)
}

type recovery struct {
	c      *Controller
	follow follow
}

func (r *recovery) observe(ctx context.Context) (Observation, bool, error) {
	dc := r.follow.standby.Datacenter

	members, err := r.follow.standby.Patroni.Members(ctx)
	if err != nil {
		return readFailed(dc, err)
	}

	obs, done, err := r.follow.streams(ctx, members)
	if err != nil || !done {
		return obs, done, err
	}

	hosts, err := r.c.instances(ctx, dc)
	if err != nil {
		return obs.unmet(err, "instances of %s not read", dc)
	}

	// The standby leader streams on the timeline of the primary: streams checked it.
	tl := *obs.Timeline
	lagging := patroni.NotFollowing(members, obs.Member, tl)
	listed := make(map[string]bool, len(members))

	for _, m := range members {
		listed[m.Host] = true
	}

	for _, h := range slices.Sorted(maps.Keys(hosts)) {
		if !listed[h] {
			lagging = append(lagging, "instance "+h+" is not in the cluster")
		}
	}

	if len(lagging) > 0 {
		return obs.unmet(nil, "not streaming on timeline %d: %s", tl, strings.Join(lagging, ", "))
	}

	return obs, true, nil
}
