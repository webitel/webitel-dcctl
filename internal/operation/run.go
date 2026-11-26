package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// run is one operation as this engine runs its steps.
type run struct {
	e     *Engine
	rec   *record
	lock  *coord.Lock
	steps []Step
	// given are the confirmations the steps are rechecked with.
	given check.Confirmations
	// confirmed are the confirmations the preflight of this start or resume accepted.
	confirmed check.Confirmations
	// resumed are the steps that had started before the operation was
	// interrupted.
	resumed map[int]bool
}

// outcome is the state the steps leave the operation in, with why.
type outcome struct {
	state dcctlv1.Operation_State
	err   error
}

func interrupted(err error) outcome { return outcome{state: dcctlv1.Operation_INTERRUPTED, err: err} }

// execute runs the steps from index from, writes the state they leave the
// operation in and releases the lock. It is the goroutine of the operation:
// it leaves the engine when done.
func (r *run) execute(ctx context.Context, from int) {
	name := r.rec.snapshot().GetName()

	defer r.e.leave()
	defer r.e.unclaim(name)

	ctx, cancel := context.WithCancelCause(ctx)

	var wg sync.WaitGroup

	// A lost lock stops the primitives of the running step at their next
	// guard or request.
	wg.Go(func() {
		select {
		case <-r.lock.Lost():
			cancel(r.lock.Err())
		case <-ctx.Done():
		}
	})

	out := r.runSteps(ctx, from)
	r.finish(ctx, out)
	r.e.release(ctx, r.lock, name)

	cancel(nil)
	wg.Wait()
}

// save saves the operation, see [record.save]. Once the lock is lost another
// instance may interrupt the operation and resume it. The copies refuse a
// change below the revision they hold; save also leaves alone a copy that
// shows another leader, and returns [errSuperseded] with the change kept in
// memory only.
func (r *run) save(ctx context.Context, fn func(op *dcctlv1.Operation)) (*dcctlv1.Operation, []coord.OpWrite, error) {
	if r.lock.Err() == nil {
		return r.rec.save(ctx, fn)
	}

	cur := r.rec.snapshot()

	view, err := r.e.cfg.Ops.Read(context.WithoutCancel(ctx))
	if err != nil {
		return cur, nil, err
	}

	if op, rev := latest(view, cur.GetName()); op != nil && (rev > cur.GetRevision() || !r.e.cfg.Self.leads(op)) {
		if fn != nil {
			r.rec.change(fn)
		}

		return r.rec.snapshot(), nil, fmt.Errorf("%s revision %d after revision %d: %w", cur.GetName(), rev, cur.GetRevision(), errSuperseded)
	}

	op, writes, err := r.rec.save(ctx, fn)
	if err != nil && slices.ContainsFunc(writes, func(w coord.OpWrite) bool { return errors.Is(w.Err, coord.ErrOpSuperseded) }) {
		return op, writes, fmt.Errorf("%w: %w", errSuperseded, err)
	}

	return op, writes, err
}

// stopped returns why the operation must stop before its next step: the lock
// is lost or the instance stops.
func (r *run) stopped(ctx context.Context) error {
	if err := r.lock.Err(); err != nil {
		return err
	}

	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ErrStopping, context.Cause(ctx))
	}

	return nil
}

func (r *run) runSteps(ctx context.Context, from int) outcome {
	for i := from; i < len(r.steps); i++ {
		if err := r.stopped(ctx); err != nil {
			return interrupted(err)
		}

		if err := r.recheck(ctx, i); err != nil {
			if rerr := r.refuseStep(ctx, i, err); rerr != nil {
				return r.interrupted(ctx, rerr)
			}

			return r.failure(ctx, i, err, false)
		}

		ran, err, rerr := r.runStep(ctx, i)

		switch {
		case rerr != nil:
			return r.interrupted(ctx, rerr)
		case !ran:
			return r.interrupted(ctx, err)
		case err != nil:
			return r.failure(ctx, i, err, true)
		}
	}

	return outcome{state: dcctlv1.Operation_SUCCEEDED}
}

// interrupted interrupts the operation for err, or for why it must stop when
// it must: a change that reached no copy after the lock was lost is the loss
// of the lock.
func (r *run) interrupted(ctx context.Context, err error) outcome {
	if cause := r.stopped(ctx); cause != nil {
		return interrupted(cause)
	}

	return interrupted(err)
}

// recheck decides the confirmations of step i on the checks it returns now.
func (r *run) recheck(ctx context.Context, i int) error {
	s := r.steps[i]
	if s.Recheck == nil {
		return nil
	}

	checks, err := s.Recheck(ctx)
	if err != nil {
		return fmt.Errorf("recheck the confirmations of step %d (%s): %w", i, s.Type, err)
	}

	v := check.Decide(checks, r.given)
	if v.Outcome == check.Allowed {
		return nil
	}

	if err := answer.Refusal(v, nil, r.rec.snapshot().GetMode()); err != nil {
		return fmt.Errorf("step %d (%s): %w", i, s.Type, err)
	}

	return fmt.Errorf("step %d (%s): confirmations not allowed: %s", i, s.Type, v.Outcome)
}

// refuseStep records step i as failed without running, as its confirmations
// no longer hold.
func (r *run) refuseStep(ctx context.Context, i int, err error) error {
	op, _, serr := r.save(ctx, func(op *dcctlv1.Operation) {
		st := op.GetSteps()[i]
		st.State, st.StartTime, st.EndTime = dcctlv1.Operation_Step_FAILED, timestamppb.Now(), timestamppb.Now()
		st.Detail = truncate("not run: " + err.Error())
	})

	r.e.journalStep(ctx, op, i, eventStep, op.GetSteps()[i].GetDetail(), err, nil)
	r.recordStep(ctx, op, i)

	return serr
}

// runStep runs step i and records its end. ran is false when the step did
// not start because its start reached no copy; recordErr is set when a change
// of the step reached no copy.
func (r *run) runStep(ctx context.Context, i int) (ran bool, stepErr, recordErr error) {
	op, _, err := r.save(ctx, func(op *dcctlv1.Operation) {
		st := op.GetSteps()[i]
		st.State, st.StartTime, st.EndTime = dcctlv1.Operation_Step_RUNNING, timestamppb.Now(), nil
		st.Detail, st.Warnings = "", nil
	})
	if err != nil {
		r.rec.change(func(op *dcctlv1.Operation) {
			st := op.GetSteps()[i]
			st.State, st.StartTime = dcctlv1.Operation_Step_PENDING, nil
		})

		return false, err, nil
	}

	r.e.journalStep(ctx, op, i, eventStep, "started", nil, nil)

	x := &Exec{r: r, index: i, resumed: r.resumed[i]}
	stop := r.flushProgress(ctx, i, false)
	stepErr = r.steps[i].Run(ctx, x)

	stop()

	op, _, recordErr = r.save(ctx, func(op *dcctlv1.Operation) {
		st := op.GetSteps()[i]
		st.EndTime = timestamppb.Now()

		switch {
		case stepErr != nil:
			st.State, st.Detail = dcctlv1.Operation_Step_FAILED, truncate(stepErr.Error())
		case x.skipped:
			st.State = dcctlv1.Operation_Step_SKIPPED
		default:
			st.State = dcctlv1.Operation_Step_SUCCEEDED
		}
	})

	r.e.journalStep(ctx, op, i, eventStep, op.GetSteps()[i].GetDetail(), stepErr, nil)
	r.recordStep(ctx, op, i)

	return true, stepErr, recordErr
}

// failure decides what the failure err of step i leaves: the operation is
// rolled back and FAILED while the point of no return is ahead and the
// outcome of the step is known; otherwise it is INTERRUPTED. ran tells
// whether the step ran.
func (r *run) failure(ctx context.Context, i int, err error, ran bool) outcome {
	if interrupting(err) || r.passed(i, ran) || r.stopped(ctx) != nil {
		return r.interrupted(ctx, err)
	}

	if rerr := r.rollback(ctx, i, ran); rerr != nil {
		return interrupted(fmt.Errorf("%w; rollback failed: %w", err, rerr))
	}

	return outcome{state: dcctlv1.Operation_FAILED, err: err}
}

// passed reports whether a failure of step i is at or past the point of no
// return: an earlier step is marked so, or step i is and it ran.
func (r *run) passed(i int, ran bool) bool {
	steps := r.rec.snapshot().GetSteps()

	for j := range i {
		if steps[j].GetPointOfNoReturn() {
			return true
		}
	}

	return ran && steps[i].GetPointOfNoReturn()
}

// rollback rolls back, in reverse, step i when it ran and the steps before it
// that succeeded.
func (r *run) rollback(ctx context.Context, i int, ran bool) error {
	start := i
	if !ran {
		start = i - 1
	}

	for j := start; j >= 0; j-- {
		st := r.rec.snapshot().GetSteps()[j]
		if j != i && st.GetState() != dcctlv1.Operation_Step_SUCCEEDED || r.steps[j].Rollback == nil {
			continue
		}

		if err := r.stopped(ctx); err != nil {
			return err
		}

		x := &Exec{r: r, index: j, resumed: r.resumed[j], rollback: true}
		stop := r.flushProgress(ctx, j, true)
		err := r.steps[j].Rollback(ctx, x)

		stop()

		op, _, serr := r.save(ctx, func(op *dcctlv1.Operation) {
			st := op.GetSteps()[j]
			st.Detail = rollbackDetail(st.GetDetail(), err)
		})
		r.e.journalStep(ctx, op, j, eventRollback, op.GetSteps()[j].GetDetail(), err, map[string]any{"rollback": true})

		switch {
		case err != nil:
			return fmt.Errorf("roll back step %d (%s): %w", j, r.steps[j].Type, err)
		case serr != nil:
			return serr
		}
	}

	return nil
}

// flushProgress saves the progress of step i within the progress interval and
// journals it within the journal progress interval, until the returned stop.
func (r *run) flushProgress(ctx context.Context, i int, rollback bool) (stop func()) {
	done := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		t := time.NewTicker(r.e.cfg.ProgressInterval)
		defer t.Stop()

		var journaled time.Time

		for {
			select {
			case <-done:
				return
			case <-t.C:
			}

			if !r.rec.isDirty() {
				continue
			}

			op, _, err := r.save(ctx, nil)
			if err != nil {
				r.e.cfg.Log.WarnCtx(ctx, "step progress not saved", logging.Str("operation", op.GetName()), logging.Err(err))
			}

			if time.Since(journaled) >= r.e.cfg.JournalProgressInterval {
				journaled = time.Now()

				r.e.journalStep(ctx, op, i, eventProgress, op.GetSteps()[i].GetDetail(), nil, map[string]any{"rollback": rollback})
			}
		}
	})

	return func() {
		close(done)
		wg.Wait()
	}
}

// finish writes the state out leaves the operation in, journals it and
// records its duration when it ended.
func (r *run) finish(ctx context.Context, out outcome) {
	op, _, err := r.save(ctx, func(op *dcctlv1.Operation) {
		switch out.state {
		case dcctlv1.Operation_INTERRUPTED:
			markInterrupted(op, out.err)
		case dcctlv1.Operation_FAILED:
			op.State, op.Error, op.EndTime = out.state, operationError(out.err), timestamppb.Now()
		case dcctlv1.Operation_SUCCEEDED:
			op.State, op.EndTime = out.state, timestamppb.Now()
		case dcctlv1.Operation_STATE_UNSPECIFIED, dcctlv1.Operation_RUNNING, dcctlv1.Operation_ABANDONED:
		}
	})

	switch {
	case errors.Is(err, errSuperseded):
		// The instance that wrote the newer change journaled the operation
		// and the lost lock.
		r.e.cfg.Log.WarnCtx(ctx, "operation changed by another instance after the lock was lost, its record is kept",
			logging.Str("operation", op.GetName()), logging.Err(err))

		return
	case err != nil:
		r.e.cfg.Log.ErrorCtx(ctx, "operation end not recorded", logging.Str("operation", op.GetName()),
			logging.Str("state", op.GetState().String()), logging.Err(err))
	}

	if errors.Is(out.err, coord.ErrLockLost) {
		r.e.journalLockLost(ctx, op.GetName(), "", out.err)
	}

	r.e.journalEnd(ctx, op, eventEnd, r.e.system(), nil)

	if coord.Finished(op.GetState()) {
		if err := r.e.cfg.Recorder.RecordOperation(ctx, op); err != nil {
			r.e.cfg.Log.WarnCtx(ctx, "operation duration not recorded", logging.Str("operation", op.GetName()), logging.Err(err))
		}
	}

	r.e.cfg.Log.InfoCtx(ctx, "operation ended", logging.Str("operation", op.GetName()),
		logging.Str("state", op.GetState().String()), logging.Int64("revision", op.GetRevision()))
}

func (r *run) recordStep(ctx context.Context, op *dcctlv1.Operation, i int) {
	if err := r.e.cfg.Recorder.RecordStep(ctx, op.GetType(), op.GetSteps()[i]); err != nil {
		r.e.cfg.Log.DebugCtx(ctx, "step duration not recorded", logging.Str("operation", op.GetName()), logging.Err(err))
	}
}

// rollbackDetail returns the detail of a step whose rollback ended with err,
// after detail; [kv.StepRolledBack] tells it.
func rollbackDetail(detail string, err error) string {
	if err != nil {
		return kv.RollbackDetail(truncate(detail), errors.New(truncate(err.Error())))
	}

	return kv.RollbackDetail(truncate(detail), nil)
}

// markInterrupted interrupts op with err. A step left RUNNING failed as far
// as the record tells; what it did is not known, so resume runs it again.
func markInterrupted(op *dcctlv1.Operation, err error) {
	op.State, op.Error = dcctlv1.Operation_INTERRUPTED, operationError(err)

	for _, st := range op.GetSteps() {
		if st.GetState() == dcctlv1.Operation_Step_RUNNING {
			st.State, st.EndTime = dcctlv1.Operation_Step_FAILED, timestamppb.Now()
			st.Detail = truncate("interrupted while running, its effect may have happened: " + err.Error())
		}
	}
}
