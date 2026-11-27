package scenario

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// place reads the cluster of stretch through the Patroni of b, the datacenter that
// takes over, with the datacenter each member runs in.
func (s *core) place(ctx context.Context, r route) (pgctl.Placement, error) {
	return s.d.Patroni.Place(ctx, s.d.cluster(r.b), s.d.names())
}

// modelMembers returns the members of p as the preflight sees them, so that a step
// picks the candidate the preflight picks.
func modelMembers(p pgctl.Placement) []status.Member {
	out := make([]status.Member, 0, len(p.Members))
	for _, m := range p.Members {
		out = append(out, status.MemberOf(collect.Member{Member: m.Member, Datacenter: m.Datacenter}))
	}

	return out
}

// where names the datacenter of a member, which is empty when it is not known.
func where(dc string) string {
	if dc == "" {
		return "a datacenter that is not known"
	}

	return dc
}

// strayPrimaries returns what reports primary besides leader, which is nil for a
// cluster without a leader: a member by its own GET /patroni, or a healthy primary
// instance in Consul on another host.
func strayPrimaries(p pgctl.Placement, leader *pgctl.Placed) []string {
	var out []string

	for _, m := range p.Primary() {
		if leader == nil || m.Name != leader.Name {
			out = append(out, fmt.Sprintf("member %s of %s", m.Name, where(m.Datacenter)))
		}
	}

	for _, dc := range slices.Sorted(maps.Keys(p.Primaries)) {
		for _, host := range p.Primaries[dc] {
			if leader == nil || host != leader.Host {
				out = append(out, fmt.Sprintf("consul instance %s of %s", host, where(dc)))
			}
		}
	}

	return out
}

// leadsIn handles a leader of p that runs in b already: the step has nothing to change.
// It is skipped, or succeeds when it is resumed, as an earlier run may have made that
// leader. The timeline of the leader goes into the generation.
func (s *core) leadsIn(ctx context.Context, x *operation.Exec, r route, p pgctl.Placement) error {
	l := p.Leader

	if l.Probe == nil || l.Probe.Role != patroni.RolePrimary || l.Probe.Timeline == nil {
		return fmt.Errorf("the leader %s of %s is not read as a primary on a known timeline", l.Name, r.b)
	}

	if stray := strayPrimaries(p, l); len(stray) > 0 {
		return interrupt(fmt.Errorf("%s leads in %s and %s is primary too", l.Name, r.b, strings.Join(stray, ", ")))
	}

	if err := s.writeTimeline(ctx, x, r, *l.Probe.Timeline); err != nil {
		return err
	}

	if x.Resumed() {
		x.Progress(fmt.Sprintf("%s of %s leads on timeline %d", l.Name, r.b, *l.Probe.Timeline))

		return nil
	}

	x.Skip(fmt.Sprintf("the leader %s already runs in %s", l.Name, r.b))

	return nil
}

// handOver switches the leadership of the cluster over from the leader in a to the
// member of b the preflight picks too, see [preflight.SwitchCandidate], and writes the
// new timeline into the generation. A leader in b already needs nothing, see
// [core.leadsIn]. A cluster without a leader, a leader in another datacenter, another
// primary, no candidate, or a switchover that does not end with the candidate leading
// interrupts the operation: the operator decides. The switchover is pinned to the leader
// read here, so that it does not hand over from another one.
func (s *core) handOver(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		p, err := s.place(ctx, r)
		if err != nil {
			return err
		}

		l := p.Leader

		switch {
		case l == nil:
			return interrupt(errors.New("the cluster has no leader, a switch does not fail it over"))
		case l.Datacenter == r.b:
			return s.leadsIn(ctx, x, r, p)
		case l.Datacenter != r.a:
			return interrupt(fmt.Errorf("the leader %s runs in %s, not in %s: the operator decides", l.Name, where(l.Datacenter), r.a))
		}

		if stray := strayPrimaries(p, l); len(stray) > 0 {
			return interrupt(fmt.Errorf("%s leads in %s and %s is primary too", l.Name, r.a, strings.Join(stray, ", ")))
		}

		limit, ok := preflight.MaximumLagOnFailover(p.Config)
		if !ok {
			return interrupt(errors.New("maximum_lag_on_failover is not known, the config of the cluster is not read"))
		}

		cand, ok := preflight.SwitchCandidate(modelMembers(p), r.b, limit)
		if !ok {
			return interrupt(fmt.Errorf("no member of %s streams within %d bytes of lag", r.b, limit))
		}

		x.Progress(fmt.Sprintf("switching over from %s to %s", l.Name, cand.Name))

		res, err := s.d.Patroni.Switchover(ctx, s.d.cluster(r.b), l.Name, cand.Name, patroniOptions(x, s.guard(x, r, afterGeneration)))

		switch {
		case err != nil && stopping(ctx, err):
			return err
		case err != nil:
			return interrupt(fmt.Errorf("switchover to %s ended with the leader %q, the operator decides: %w", cand.Name, res.Leader, err))
		case res.Timeline == nil:
			return interrupt(fmt.Errorf("timeline of %s unknown after the switchover", cand.Name))
		}

		return s.writeTimeline(ctx, x, r, *res.Timeline)
	}
}

// failOver promotes, in a cluster without a leader, the member of b the preflight picks
// too by a manual failover, and writes its timeline into the generation. The WAL the
// failover discards is measured now, see [preflight.FailoverLoss], and must be within the
// max_discard_bytes the run of the step confirmed; a loss that is not known interrupts
// the operation. A leader in b already needs nothing, see [core.leadsIn]; one elsewhere,
// a member or a Consul instance read as primary, a member of no known datacenter that is
// not read, or no candidate interrupts the operation too.
func (s *core) failOver(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		p, err := s.place(ctx, r)
		if err != nil {
			return err
		}

		if l := p.Leader; l != nil {
			if l.Datacenter == r.b {
				return s.leadsIn(ctx, x, r, p)
			}

			return interrupt(fmt.Errorf("the leader %s runs in %s: the cluster has a leader, the operator decides", l.Name, where(l.Datacenter)))
		}

		if stray := strayPrimaries(p, nil); len(stray) > 0 {
			return interrupt(fmt.Errorf("the cluster has no leader and %s is primary", strings.Join(stray, ", ")))
		}

		if err := checkUnread(x, r, p); err != nil {
			return err
		}

		l, err := preflight.FailoverLoss(modelMembers(p), r.b)
		if err != nil {
			return interrupt(fmt.Errorf("the cluster has no leader and %w", err))
		}

		cand, loss := l.Candidate, l.Bytes

		if confirmed := x.Confirmed().MaxDiscardBytes; loss > confirmed {
			return interrupt(fmt.Errorf("%s is not failed over to: the failover discards %d bytes of WAL, %d are confirmed: "+
				"max_discard_bytes of %d fails it over", cand.Name, loss, confirmed, loss))
		}

		for _, name := range l.NotCounted {
			x.Warn(fmt.Sprintf("member %s reports no WAL position, the WAL it holds is not counted", name))
		}

		if loss > 0 {
			x.Warn(fmt.Sprintf("the failover to %s discards %d bytes of WAL; %s", cand.Name, loss, l.Uncounted()))
		}

		x.Progress(fmt.Sprintf("failing over to %s, %d bytes of WAL discarded; %s", cand.Name, loss, l.Uncounted()))

		res, err := s.d.Patroni.Failover(ctx, s.d.cluster(r.b), cand.Name, patroniOptions(x, s.guard(x, r, afterGeneration)))

		switch {
		case err != nil && stopping(ctx, err):
			return err
		case err != nil:
			return interrupt(fmt.Errorf("failover to %s ended with the leader %q, the operator decides: %w", cand.Name, res.Leader, err))
		case res.Timeline == nil:
			return interrupt(fmt.Errorf("timeline of %s unknown after the failover", cand.Name))
		}

		return s.writeTimeline(ctx, x, r, *res.Timeline)
	}
}

// checkUnread warns of the members of p outside b that are not read: they may be
// primary, which the isolation every failover confirms accepts. One of no known
// datacenter interrupts the operation: whose isolation it is, is not known.
func checkUnread(x *operation.Exec, r route, p pgctl.Placement) error {
	for _, m := range p.Members {
		switch {
		case m.Probe != nil || m.Datacenter == r.b:
		case m.Datacenter == "":
			return interrupt(fmt.Errorf("member %s of a datacenter that is not known is not read, it may be primary", m.Name))
		default:
			x.Warn(fmt.Sprintf("member %s of %s is not read; it may be primary", m.Name, m.Datacenter))
		}
	}

	return nil
}

// rejoin waits for the members of a to stream from the leader in b, see
// [pgctl.Controller.Rejoin]; a failure is a warning.
func (s *core) rejoin(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		// The cluster spans every datacenter: the Patroni of b, where it leads, reads the
		// members of a too.
		cl := pgctl.Cluster{Datacenter: r.a, Patroni: s.d.Datacenters[r.b].Patroni}

		res, err := s.d.Patroni.Rejoin(ctx, cl, patroniOptions(x, nil))

		for _, m := range res.Unread {
			x.Warn(fmt.Sprintf("member %s of %s is not read, whether it follows the leader is not known", m, r.a))
		}

		for _, host := range res.Missing {
			x.Warn(fmt.Sprintf("instance on host %s of %s is not in the Patroni cluster", host, r.a))
		}

		if err == nil || ctx.Err() != nil {
			return err
		}

		x.Warn(fmt.Sprintf("%s needs attention, it does not follow the leader in %s: %v", r.a, r.b, err))

		return nil
	}
}
