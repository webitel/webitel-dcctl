package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// phase tells which record a step acts under: the one the switch starts from, or the
// new generation it writes.
type phase int

const (
	beforeGeneration phase = iota
	afterGeneration
)

// records are the record the switch of op starts from and the one it writes.
func records(op *dcctlv1.Operation, r route) (base, next coord.RecordKey) {
	return coord.RecordKey{Generation: op.GetPreviousGeneration(), ActiveDatacenter: r.a},
		coord.RecordKey{Generation: op.GetNewGeneration(), ActiveDatacenter: r.b}
}

// guard returns the guard of the primitives of a step of x: the lock is held, and the
// resolved effective record is the one of p, written by this operation after the
// generation step. A record that changed interrupts the operation.
func (s *Switch) guard(x *operation.Exec, r route, p phase) func(context.Context) error {
	op := x.Operation()
	want, next := records(op, r)
	opID := ""

	if p == afterGeneration {
		want, opID = next, operationID(op.GetName())
	}

	return func(ctx context.Context) error {
		if err := x.Guard(ctx); err != nil {
			return err
		}

		view, err := s.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		base := coord.Base(view, false)

		switch {
		case base == nil:
			return interrupt(fmt.Errorf("the voters do not tell generation %d of %s is effective", want.Generation, want.ActiveDatacenter))
		case coord.KeyOf(base.State) != want || opID != "" && base.State.OpID != opID:
			return interrupt(fmt.Errorf("the voters hold generation %d of %s, the step acts under generation %d of %s",
				base.State.Generation, base.State.ActiveDatacenter, want.Generation, want.ActiveDatacenter))
		}

		return nil
	}
}

func appsOptions(x *operation.Exec, guard apps.Guard) apps.Options {
	return apps.Options{Guard: guard, Progress: func(p apps.Progress) { x.Progress(appsDetail(p)) }}
}

func patroniOptions(x *operation.Exec, guard pgctl.Guard) pgctl.Options {
	return pgctl.Options{Guard: guard, Progress: func(p pgctl.Progress) { x.Progress(patroniDetail(p)) }}
}

// warnApps adds the warnings of the stages of res to the step.
func warnApps(x *operation.Exec, res apps.Result) {
	for _, st := range res.Stages {
		for _, w := range st.Warnings() {
			x.Warn(w)
		}
	}
}

// stopApps stops the apps of a from stage from up; from 0 stops every stage.
func (s *Switch) stopApps(r route, from int) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		res, err := s.d.Apps.StopFrom(ctx, s.d.apps(r.a), from, appsOptions(x, s.guard(x, r, beforeGeneration)))
		if err == nil {
			warnApps(x, res)
		}

		return err
	}
}

// startApps starts the apps of dc up to stage to; math.MaxInt starts every stage.
func (s *Switch) startApps(r route, dc string, to int, p phase) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		res, err := s.d.Apps.StartUpTo(ctx, s.d.apps(dc), to, appsOptions(x, s.guard(x, r, p)))
		if err == nil {
			warnApps(x, res)
		}

		return err
	}
}

// demote makes the cluster of a a standby of b. When it fails with a still primary,
// the rollback undoes whatever the request did; any other state of a is left to the
// operator.
func (s *Switch) demote(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		res, err := s.d.Patroni.Demote(ctx, s.d.cluster(r.a), s.d.cluster(r.b),
			patroniOptions(x, s.guard(x, r, beforeGeneration)))

		switch {
		case err == nil, stopping(ctx, err):
			return err
		case res.Role == patroni.RolePrimary:
			return fmt.Errorf("%s is still primary: %v", r.a, err) //nolint:errorlint // the outcome is known: a is primary
		}

		return interrupt(fmt.Errorf("leader of %s is %q after the demotion, the operator decides: %w", r.a, res.Role, err))
	}
}

// checkNoPrimary checks that no cluster of need and others is primary: the clusters of
// need must be read, one of others that is not read is a warning. A primary found
// interrupts the operation.
func (s *Switch) checkNoPrimary(ctx context.Context, x *operation.Exec, need, others []string) error {
	res, err := s.d.Patroni.NoPrimary(ctx, s.d.clusters(need...), s.d.clusters(others...), patroniOptions(x, nil))

	var pe *pgctl.PrimaryError
	if errors.As(err, &pe) {
		return interrupt(err)
	}

	for _, dc := range res.Unread {
		x.Warn(fmt.Sprintf("%s is not read, it is not known whether it is primary", dc))
	}

	return err
}

// promoteBack removes standby_cluster from a, as the rollback of its demotion, unless b
// or another standby is read as primary. One that is not read is a warning: the
// generation still names a and the switch did not promote b.
func (s *Switch) promoteBack(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		if err := s.checkNoPrimary(ctx, x, nil, slices.Concat([]string{r.b}, r.c)); err != nil {
			return err
		}

		_, err := s.d.Patroni.Promote(ctx, s.d.cluster(r.a), patroniOptions(x, s.guard(x, r, beforeGeneration)))

		return err
	}
}

// catchup waits for b to catch up with a; b or another datacenter found primary
// interrupts the operation.
func (s *Switch) catchup(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		_, err := s.d.Patroni.Catchup(ctx, s.d.cluster(r.a), s.d.cluster(r.b), patroniOptions(x, nil))

		return primaryElsewhere(err, r.a)
	}
}

// noPrimary checks that neither a nor b is primary before b is promoted, nor a standby
// of r.c that is read. A primary other than a interrupts the operation.
func (s *Switch) noPrimary(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		res, err := s.d.Patroni.NoPrimary(ctx, s.d.clusters(r.a, r.b), s.d.clusters(r.c...), patroniOptions(x, nil))

		for _, dc := range res.Unread {
			x.Warn(fmt.Sprintf("%s is not read, it is not known whether it is primary", dc))
		}

		return primaryElsewhere(err, r.a)
	}
}

// generation writes the new generation with b active and takes the lease of b. The
// record is built from what the voters hold now: a resumed step finishes the record an
// earlier run wrote. A failure that leaves a still active, with nothing written, rolls
// back; one that finds a record of another writer, or that comes after the record is
// committed, interrupts.
func (s *Switch) generation(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		op := x.Operation()
		_, next := records(op, r)

		view, err := s.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		from, to, held, done, err := advanceRecords(view, op, r)
		if err != nil {
			return err
		}

		if !done {
			opts := coord.AdvanceOptions{Mode: coord.WriteQuorum}
			if to.Forced {
				opts = coord.AdvanceOptions{Mode: coord.WriteForced, IsolationConfirmed: op.GetConfirmation().GetIsolation()}
			}

			x.Progress(fmt.Sprintf("writing generation %d with %s active", next.Generation, r.b))

			_, err := x.Write(ctx, "advance generation", func(ctx context.Context) (coord.WriteResult, error) {
				return s.d.Coordinator.Advance(ctx, from, to, opts)
			})

			switch {
			case err == nil:
			case held || x.Resumed(),
				errors.Is(err, coord.ErrSuperseded), errors.Is(err, coord.ErrConflict), errors.Is(err, coord.ErrBaseNotFound):
				return interrupt(err)
			default:
				return err
			}
		}

		err = x.AcquireLease(ctx, coord.LeaseHolder{Datacenter: r.b, Generation: next.Generation})

		switch {
		case err == nil:
			return nil
		case stopping(ctx, err):
			return interrupt(err)
		}

		x.Warn(fmt.Sprintf("lease of %s not taken, its renewal takes it: %v", r.b, err))

		return nil
	}
}

// advanceRecords returns the records [coord.Coordinator.Advance] writes the generation
// of op with, or done when the voters hold it committed already. A record held by some
// voters from an earlier run is written as it is, forced when the operation now is; held
// tells it. The base is the record the voters hold, effective or undetermined, and only
// when none holds it the generation and active datacenter the switch was planned from.
func advanceRecords(view coord.View, op *dcctlv1.Operation, r route) (from, to kv.State, held, done bool, err error) {
	base, next := records(op, r)
	opID := operationID(op.GetName())
	forced := op.GetMode() == dcctlv1.Operation_FORCED

	if next.Generation != base.Generation+1 {
		return from, to, false, false, fmt.Errorf("generation %d does not follow %d", next.Generation, base.Generation)
	}

	if eff := view.Effective; eff != nil && coord.KeyOf(eff.State) == next && eff.State.OpID == opID {
		return from, to, false, true, nil
	}

	to, held = heldRecord(view, next, opID)
	baseRec := holdingBase(view, base)

	switch eff := view.Effective; {
	case baseRec != nil:
		from = baseRec.State
	case held:
		from = kv.State{Generation: base.Generation, ActiveDatacenter: base.ActiveDatacenter}
	case eff == nil:
		return from, to, held, false, interrupt(fmt.Errorf("the voters give no effective record, generation %d of %s was planned from",
			base.Generation, base.ActiveDatacenter))
	default:
		return from, to, held, false, interrupt(fmt.Errorf("the voters hold generation %d of %s, the switch was planned from generation %d of %s",
			eff.State.Generation, eff.State.ActiveDatacenter, base.Generation, base.ActiveDatacenter))
	}

	if held {
		if to.Revision != 0 {
			return from, to, held, false, interrupt(fmt.Errorf("generation %d of %s is held at revision %d without being committed",
				next.Generation, next.ActiveDatacenter, to.Revision))
		}

		to.Forced = to.Forced || forced

		return from, to, held, false, nil
	}

	return from, kv.State{
		Generation:       next.Generation,
		ActiveDatacenter: next.ActiveDatacenter,
		OpID:             opID,
		Forced:           forced,
		PatroniTimeline:  from.PatroniTimeline,
		NeedsRecovery:    from.NeedsRecovery,
	}, false, false, nil
}

// holdingBase returns the record base as the voters hold it, effective or undetermined;
// nil when no voter that answered holds it.
func holdingBase(view coord.View, base coord.RecordKey) *coord.Record {
	if eff := view.Effective; eff != nil && coord.KeyOf(eff.State) == base {
		return eff
	}

	for i, u := range view.Undetermined {
		if coord.KeyOf(u.State) == base {
			return &view.Undetermined[i]
		}
	}

	return nil
}

// heldRecord returns the record next of the operation opID as a voter that answered
// holds it.
func heldRecord(view coord.View, next coord.RecordKey, opID string) (kv.State, bool) {
	for _, vs := range view.Voters {
		if vs.Status == coord.StatusPresent && coord.KeyOf(vs.State) == next && vs.State.OpID == opID {
			return vs.State, true
		}
	}

	return kv.State{}, false
}

// keepGeneration refuses to roll back past a generation this operation wrote: the
// voters name b active, so a must not start again.
func (s *Switch) keepGeneration(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		op := x.Operation()
		_, next := records(op, r)

		view, err := s.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		if _, ok := heldRecord(view, next, operationID(op.GetName())); ok {
			return fmt.Errorf("generation %d of %s is written by the operation and is not rolled back", next.Generation, r.b)
		}

		return nil
	}
}

// promote promotes the cluster of b, once neither a nor another standby is primary, and
// writes its new timeline into the generation. a must be read, unless the run of the
// step confirmed the isolation of a: the switch saw it a standby before.
func (s *Switch) promote(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		need, others := []string{r.a}, r.c
		if x.Confirmed().Isolation {
			need, others = nil, slices.Concat([]string{r.a}, r.c)
		}

		if err := s.checkNoPrimary(ctx, x, need, others); err != nil {
			return err
		}

		res, err := s.d.Patroni.Promote(ctx, s.d.cluster(r.b), patroniOptions(x, s.guard(x, r, afterGeneration)))
		if err != nil {
			return err
		}

		if res.Timeline == nil {
			return fmt.Errorf("timeline of %s unknown after the promotion", r.b)
		}

		_, next := records(x.Operation(), r)

		view, err := s.d.Coordinator.Read(ctx)
		if err != nil {
			return err
		}

		if eff := view.Effective; eff != nil && coord.KeyOf(eff.State) == next && eff.State.PatroniTimeline == *res.Timeline {
			return nil
		}

		_, err = x.Write(ctx, "set patroni timeline", func(ctx context.Context) (coord.WriteResult, error) {
			return s.d.Coordinator.SetTimeline(ctx, next, *res.Timeline)
		})

		return err
	}
}

// repoint points the standby c at b; a failure is a warning, c is repointed later.
func (s *Switch) repoint(r route, c string) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		_, err := s.d.Patroni.Repoint(ctx, s.d.cluster(c), s.d.cluster(r.b), patroniOptions(x, s.guard(x, r, afterGeneration)))
		if err == nil || stopping(ctx, err) {
			return err
		}

		x.Warn(fmt.Sprintf("%s is not repointed to %s: %v", c, r.b, err))

		return nil
	}
}

// follow waits for a, already pointed at b, to stream from it; a failure is a warning.
func (s *Switch) follow(r route) operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		_, err := s.d.Patroni.Follow(ctx, s.d.cluster(r.a), s.d.cluster(r.b), patroniOptions(x, nil))
		if err == nil || ctx.Err() != nil {
			return err
		}

		x.Warn(fmt.Sprintf("%s needs attention, it does not follow %s: %v", r.a, r.b, err))

		return nil
	}
}

// verify checks that the switch is complete; what is not is a warning.
func (s *Switch) verify() operation.StepFunc {
	return func(ctx context.Context, x *operation.Exec) error {
		v, err := s.d.Verifier.Run(ctx, preflight.Request{Action: preflight.Verify})

		switch {
		case err != nil && ctx.Err() != nil:
			return err
		case err != nil:
			x.Warn("the result is not verified: " + err.Error())

			return nil
		}

		for _, c := range v.Checks {
			if c.Result != check.Pass && c.Result != check.Skip {
				x.Warn(fmt.Sprintf("%s: %s", c.ID, c.Detail))
			}
		}

		return nil
	}
}
