package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// onward marks every error of fn as one that interrupts the operation.
func onward(fn operation.StepFunc) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		if err := fn(ctx, x); err != nil {
			return interrupt(err)
		}

		return nil
	}
}

const (
	// appsStopUnconfirmed is the warning of a stop step of a failover whose stage of the
	// old active datacenter did not stop in time.
	appsStopUnconfirmed = "the stop of the apps of the old active datacenter is not confirmed"
	// appsNomadSilent is the warning of a stop step of a failover that could not list the
	// jobs of the old active datacenter: its Nomad does not answer.
	appsNomadSilent = "the Nomad of the old active datacenter does not answer, its apps are not stopped"
)

// stopWarned reports whether a stop step of op about a has warning.
func stopWarned(op *dcctlv1.Operation, a, warning string) bool {
	return slices.ContainsFunc(op.GetSteps(), func(st *dcctlv1.Operation_Step) bool {
		return st.GetType() == dcctlv1.Operation_Step_APPS_STOP && datacenterID(st.GetDatacenter()) == a &&
			slices.Contains(st.GetWarnings(), warning)
	})
}

// stopOld tries to stop the apps of a from stage from up, within
// apps.failover_stop_timeout. A stage that does not stop in time is a warning and the
// next stop step goes on. A Nomad that does not list the jobs is a warning too, and the
// later stop steps are skipped: they would wait for it as long. A guard refusal stops
// the operation.
func (f *Failover) stopOld(r route, from int) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		if stopWarned(x.Operation(), r.a, appsNomadSilent) {
			x.Skip(fmt.Sprintf("the Nomad of %s did not answer an earlier stop", r.a))

			return nil
		}

		res, err := f.d.Apps.StopFromFailover(ctx, f.d.apps(r.a), from, appsOptions(x, f.guard(x, r, afterGeneration)))

		switch {
		case err == nil:
			warnApps(x, res)

			return nil
		case stopping(ctx, err):
			return err
		}

		x.Progress(fmt.Sprintf("apps of %s: %v", r.a, err))

		if errors.Is(err, apps.ErrNotListed) {
			x.Warn(appsNomadSilent)
		} else {
			x.Warn(appsStopUnconfirmed)
		}

		return nil
	}
}

// notDemotedError is why demoteSource did not demote the old primary.
type notDemotedError struct{ reason string }

func (e *notDemotedError) Error() string { return e.reason }

// demoteSource makes the cluster of a, read as primary, a standby of b when the run of
// the step confirmed as max_discard_bytes the WAL its demotion discards, measured now,
// see [patroni.DemoteLoss]. It returns *notDemotedError when it does not demote a or the
// demotion fails; any other error ends the step.
func (f *Failover) demoteSource(ctx context.Context, x *operation.Exec, r route) error {
	n, err := f.d.Patroni.DemoteLoss(ctx, f.d.cluster(r.a), f.d.cluster(r.b))

	switch confirmed := x.Confirmed().MaxDiscardBytes; {
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil:
		return &notDemotedError{fmt.Sprintf("%s is primary and not demoted, the WAL its demotion discards is not known: %v", r.a, err)}
	case n > confirmed:
		return &notDemotedError{fmt.Sprintf("%s is primary and not demoted, its demotion discards %d bytes of WAL, %d are confirmed: "+
			"max_discard_bytes of %d demotes it", r.a, n, confirmed, n)}
	}

	_, err = f.d.Patroni.Demote(ctx, f.d.cluster(r.a), f.d.cluster(r.b), patroniOptions(x, f.guard(x, r, afterGeneration)))

	switch {
	case err == nil:
		return nil
	case stopping(ctx, err):
		return err
	}

	return &notDemotedError{fmt.Sprintf("%s is not demoted: %v", r.a, err)}
}

// demoteOld demotes a while it is read as primary, see [Failover.demoteSource]. A
// demotion it does not make is skipped with a warning: the promotion of b tries it
// again. a read as not primary needs nothing.
func (f *Failover) demoteOld(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		la, err := f.d.Patroni.Leader(ctx, f.d.cluster(r.a))

		switch {
		case err != nil && ctx.Err() != nil:
			return err
		case err != nil:
			x.Warn(fmt.Sprintf("%s is not read, it is not demoted: %v", r.a, err))
			x.Skip(r.a + " is not read")

			return nil
		case la.Probe.Role != patroni.RolePrimary:
			x.Skip(fmt.Sprintf("%s is not primary, it is %s", r.a, la.Probe.Role))

			return nil
		}

		err = f.demoteSource(ctx, x, r)

		var nd *notDemotedError
		if errors.As(err, &nd) {
			x.Warn(nd.reason)
			x.Skip(r.a + " is not demoted")

			return nil
		}

		return err
	}
}

// demoteBeforePromote demotes a when the promotion of b reads it as primary, as after a
// demotion step that was skipped, and checks that a is no longer primary. A demotion it
// does not make interrupts the operation, telling the bound to confirm. a not read is
// left to the check for a primary.
func (f *Failover) demoteBeforePromote(ctx context.Context, x *operation.Exec, r route) error {
	la, err := f.d.Patroni.Leader(ctx, f.d.cluster(r.a))

	switch {
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil, la.Probe.Role != patroni.RolePrimary:
		return nil
	}

	if err := f.demoteSource(ctx, x, r); err != nil {
		return interrupt(err)
	}

	la, err = f.d.Patroni.Leader(ctx, f.d.cluster(r.a))

	switch {
	case err != nil:
		return interrupt(fmt.Errorf("%s is not read after its demotion: %w", r.a, err))
	case la.Probe.Role == patroni.RolePrimary:
		return interrupt(fmt.Errorf("%s is still primary after its demotion", r.a))
	}

	x.Warn(fmt.Sprintf("%s is demoted at the promotion of %s", r.a, r.b))

	return nil
}

// unreachableStandby is the warning of a standby that is not read before the promotion.
func unreachableStandby(dc string) string {
	return dc + " is not reachable; it may be primary or run apps"
}

// promoteOver promotes the cluster of b over a lost or isolated a, as the switch does
// once no other cluster is primary. a read as primary is demoted first, see
// [Failover.demoteBeforePromote]. Then a must be read unless the run of the step confirmed
// its isolation, and a primary read anywhere interrupts. While b is not promoted and a
// is not read, b or another standby that still streams from its upstream shows a alive,
// which interrupts too; so does WAL the promotion would discard on another standby
// without not_max_lsn confirmed by the run.
func (f *Failover) promoteOver(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		confirmed := x.Confirmed()

		need, others := []string{r.a}, r.c
		if confirmed.Isolation {
			need, others = nil, slices.Concat([]string{r.a}, r.c)
		}

		if err := f.demoteBeforePromote(ctx, x, r); err != nil {
			return err
		}

		unread, err := f.checkNoPrimary(ctx, x, need, others, func(dc string) string {
			if dc == r.a {
				return notKnownPrimary(dc)
			}

			return unreachableStandby(dc)
		})
		if err != nil {
			return err
		}

		lb, err := f.d.Patroni.Leader(ctx, f.d.cluster(r.b))
		if err != nil {
			return err
		}

		if lb.Probe.Role != patroni.RolePrimary {
			standbys, err := f.standbys(ctx, r, unread)
			if err != nil {
				return err
			}

			if slices.Contains(unread, r.a) {
				if err := sourceAlive(r, lb, standbys); err != nil {
					return err
				}
			}

			if err := checkDiscard(x, r, lb, standbys, confirmed); err != nil {
				return err
			}
		}

		return f.promoteTarget(ctx, x, r)
	}
}

// standby is the leader of a standby of r.c as read before the promotion of b; leader
// is nil when it is not read.
type standby struct {
	dc     string
	leader *patroni.Member
}

// standbys reads the leaders of the standbys of r.c that are not in unread, which the
// check for a primary did not read.
func (f *Failover) standbys(ctx context.Context, r route, unread []string) ([]standby, error) {
	var out []standby

	for _, c := range r.c {
		if slices.Contains(unread, c) {
			continue
		}

		l, err := f.d.Patroni.Leader(ctx, f.d.cluster(c))

		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err != nil:
			out = append(out, standby{dc: c})
		default:
			out = append(out, standby{dc: c, leader: &l})
		}
	}

	return out, nil
}

// sourceAlive interrupts when b, led by lb, or a standby still streams from its
// upstream while a is not read: then a is alive and not isolated.
func sourceAlive(r route, lb patroni.Member, standbys []standby) error {
	streaming := func(m *patroni.Member) bool { return m.Probe.ReplicationState == patroni.ReplicationStreaming }

	if streaming(&lb) {
		return interrupt(fmt.Errorf("%s is not read and %s still streams from its upstream: %s may be alive", r.a, r.b, r.a))
	}

	for _, s := range standbys {
		if s.leader != nil && streaming(s.leader) {
			return interrupt(fmt.Errorf("%s is not read and %s still streams from its upstream: %s may be alive", r.a, s.dc, r.a))
		}
	}

	return nil
}

// checkDiscard checks the WAL the promotion of b, whose leader lb received what it
// shows, discards on standbys: one that received more than b, or what b or one received
// that is not known, needs not_max_lsn in confirmed. As in the preflight, a standby that
// is not read is not compared, and without other standbys there is nothing to compare.
func checkDiscard(x *operation.Exec, r route, lb patroni.Member, standbys []standby, confirmed check.Confirmations) error {
	if len(r.c) == 0 {
		return nil
	}

	rb := lb.Probe.Xlog.ReceivedLocation

	var (
		unknown, more []string
		discard       int64
	)

	if rb == nil {
		unknown = append(unknown, r.b)
	}

	for _, s := range standbys {
		switch {
		case s.leader == nil || s.leader.Probe.Xlog.ReceivedLocation == nil:
			unknown = append(unknown, s.dc)
		case rb != nil && *s.leader.Probe.Xlog.ReceivedLocation > *rb:
			more = append(more, s.dc)
			discard = max(discard, *s.leader.Probe.Xlog.ReceivedLocation-*rb)
		}
	}

	var why []string

	if len(more) > 0 {
		why = append(why, fmt.Sprintf("%s received up to %d bytes more WAL than %s", strings.Join(more, ", "), discard, r.b))
	}

	if len(unknown) > 0 {
		why = append(why, "the WAL received is not known of "+strings.Join(unknown, ", "))
	}

	detail := strings.Join(why, "; ")

	switch {
	case len(why) == 0:
		return nil
	case !confirmed.NotMaxLSN:
		return interrupt(fmt.Errorf("%s is not promoted, not_max_lsn is not confirmed: %s", r.b, detail))
	}

	x.Warn("the promotion of " + r.b + " discards WAL: " + detail)

	return nil
}

// repointUnlessDiverged points the standby c at b unless c diverged from it: a diverged
// standby, or one whose divergence is not known, is a warning and waits for repoint
// with the discard confirmed. c read as primary interrupts the operation: b is primary.
func (f *Failover) repointUnlessDiverged(r route, c string) operation.StepFunc {
	repoint := f.repoint(r, c)

	return func(ctx context.Context, x *operation.Exec) error {
		d, err := f.d.Patroni.Divergence(ctx, f.d.cluster(c), f.d.cluster(r.b))

		var pe *pgctl.PrimaryError

		switch {
		case errors.As(err, &pe):
			return interrupt(err)
		case err != nil && ctx.Err() != nil:
			return err
		case err != nil:
			x.Warn(fmt.Sprintf("%s is not repointed to %s, whether it diverged is not known: %v", c, r.b, err))

			return nil
		case d.Diverged:
			x.Warn(fmt.Sprintf("%s is not repointed to %s, it diverged by %d bytes: repoint it with the discard confirmed",
				c, r.b, d.Bytes))

			return nil
		}

		return repoint(ctx, x)
	}
}

// verifyFailover verifies the failover as the switch does, and warns again when the stop
// of the apps of a is not confirmed.
func (f *Failover) verifyFailover(r route) operation.StepFunc {
	verify := f.verify()

	return func(ctx context.Context, x *operation.Exec) error {
		if op := x.Operation(); stopWarned(op, r.a, appsStopUnconfirmed) || stopWarned(op, r.a, appsNomadSilent) {
			x.Warn(appsStopUnconfirmed)
		}

		return verify(ctx, x)
	}
}
