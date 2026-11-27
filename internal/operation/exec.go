package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

// Exec is what a running step, or its rollback, works with. Its methods are
// safe for concurrent use.
type Exec struct {
	r     *run
	index int
	// resumed is set for a step that had started before the operation was
	// interrupted.
	resumed  bool
	rollback bool
	skipped  bool
}

// Guard returns nil while the operation may change the infrastructure: its
// context is live and the lock is held. Pass it as the guard of the apps and
// pgctl primitives. An ended context is returned as it is.
func (x *Exec) Guard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return x.r.lock.Err()
}

// Progress sets the detail of the step. The operation record takes it within
// the progress interval, the journal within the journal progress interval;
// Progress itself does not wait for either.
func (x *Exec) Progress(detail string) {
	x.r.rec.change(func(op *dcctlv1.Operation) { op.GetSteps()[x.index].Detail = truncate(detail) })
}

// Warn adds a warning to the step: it succeeds with warnings.
func (x *Exec) Warn(warning string) {
	x.r.rec.change(func(op *dcctlv1.Operation) {
		st := op.GetSteps()[x.index]
		st.Warnings = append(st.Warnings, truncate(warning))
	})
}

// Skip marks the step as not needed in this situation, with why; the step
// returns nil after it.
func (x *Exec) Skip(detail string) {
	x.r.rec.change(func(op *dcctlv1.Operation) {
		op.GetSteps()[x.index].Detail = truncate(detail)
		x.skipped = true
	})
}

// Operation returns a copy of the operation as it is now.
func (x *Exec) Operation() *dcctlv1.Operation { return x.r.rec.snapshot() }

// Confirmed returns the confirmations the preflight of the start or resume that runs
// the step accepted: those a check of that preflight needed, as the isolation of a resume
// that writes forced. A step that acts on them reads them here, not through
// [Step.Recheck], whose refusal would roll the operation back.
func (x *Exec) Confirmed() check.Confirmations { return x.r.confirmed }

// SetMode records mode as the mode of the operation, as the step that writes the
// generation does once it knows the mode it writes in. Nothing is saved when the
// operation has that mode already.
func (x *Exec) SetMode(ctx context.Context, mode dcctlv1.Operation_Mode) error {
	if x.Operation().GetMode() == mode {
		return nil
	}

	_, _, err := x.r.save(ctx, func(op *dcctlv1.Operation) { op.Mode = mode })
	if err != nil {
		return fmt.Errorf("record mode %s: %w", mode, err)
	}

	return nil
}

// Resumed reports whether the step had started before the operation was
// interrupted: what it does may already be done, in part or in full.
func (x *Exec) Resumed() bool { return x.resumed }

// Write writes dcctl/state with fn, one of the writes of [coord.Coordinator]
// such as Advance or SetTimeline, after checking that the lock is held, and
// journals the result as action. Its error is that of fn: a record that is
// [coord.Undetermined] or [coord.Uncommitted] comes with
// [coord.ErrUndetermined] or [coord.ErrUncommitted], and a step that returns
// it interrupts the operation, to be resumed on what the voters hold then.
func (x *Exec) Write(ctx context.Context, action string, fn func(ctx context.Context) (coord.WriteResult, error)) (coord.WriteResult, error) {
	if err := x.Guard(ctx); err != nil {
		return coord.WriteResult{}, fmt.Errorf("%s: %w", action, err)
	}

	res, err := fn(ctx)

	extra := map[string]any{
		"write":      action,
		"mode":       string(res.Mode),
		"commitment": string(res.Commitment),
		"accepted":   anySlice(res.Accepted()),
		"majority":   res.Majority,
	}

	if unknown := writesOf(res, coord.WriteUnknown); len(unknown) > 0 {
		extra["unknown"] = anySlice(unknown)
	}

	x.journal(ctx, eventWrite, action+": "+string(res.Commitment), err, extra)

	return res, err
}

// AcquireLease takes the lease of h, as the operation that makes h active
// does, after checking that the lock is held, and journals the result. A
// lease written on fewer than a majority of voters is a warning of the step,
// not an error: the renewal keeps trying.
func (x *Exec) AcquireLease(ctx context.Context, h coord.LeaseHolder) error {
	if err := x.Guard(ctx); err != nil {
		return fmt.Errorf("acquire lease: %w", err)
	}

	w, err := x.r.e.cfg.Coordinator.AcquireLease(ctx, h)
	extra := map[string]any{"holder": h.Datacenter, "generation": h.Generation, "written": w.Written(), "majority": w.Majority}

	if errors.Is(err, coord.ErrLeaseMinority) {
		x.Warn(err.Error())
		x.journal(ctx, eventLease, "lease taken without a majority: "+err.Error(), nil, extra)

		return nil
	}

	x.journal(ctx, eventLease, "lease taken", err, extra)

	return err
}

func (x *Exec) journal(ctx context.Context, event, detail string, err error, extra map[string]any) {
	if x.rollback {
		extra["rollback"] = true
	}

	x.r.e.journalStep(ctx, x.r.rec.snapshot(), x.index, event, detail, err, extra)
}

func writesOf(res coord.WriteResult, outcome coord.WriteOutcome) []string {
	var out []string

	for _, w := range res.Voters {
		if w.Outcome == outcome {
			out = append(out, w.Voter)
		}
	}

	return slices.Clip(out)
}
