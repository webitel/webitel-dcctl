package operation

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// Resumer rebuilds the steps of an interrupted operation of one type.
type Resumer interface {
	// Resume returns the steps of op, one per step of op, in its order and
	// of its types. The steps before the first one to run are needed for
	// their rollback only. A step that builds a record from the voters, as the
	// new generation, builds it from what the voters hold when it runs.
	Resume(ctx context.Context, op *dcctlv1.Operation) ([]Step, error)
}

// ResumeRequest resumes an interrupted operation.
type ResumeRequest struct {
	// Operation is the ID of the operation, without operations/.
	Operation     string
	Confirmations check.Confirmations
	// ToForced is set when the resumed operation writes its generation
	// forced, without a majority of voters.
	ToForced  bool
	Initiator *dcctlv1.Initiator
}

// Resume runs an interrupted operation again from its first step that did not
// succeed, re-running a step whose effect may have happened, and returns the
// operation RUNNING as first written. Its revision continues from the highest
// copy, and this instance becomes its leader.
//
// The preflight runs first, as in [Engine.Start], then the lock is taken. An
// operation found in no copy is [*NotFoundError], one in another state
// [*NotInterruptedError]. Another unfinished operation in a copy refuses the
// resume with [*coord.OperationRunningError]. An operation with a step of a
// type this dcctl does not know is refused with [ErrUnknownStepType], one
// interrupted during or after its rollback with [ErrRolledBack], both with
// the refusal of their preflight check; one whose type has no [Resumer] with
// [ErrNoResumer].
func (e *Engine) Resume(ctx context.Context, req ResumeRequest) (*dcctlv1.Operation, error) {
	base, err := e.enter()
	if err != nil {
		return nil, err
	}

	started := false

	defer func() {
		if !started {
			e.leave()
		}
	}()

	v, err := e.cfg.Preflight.Run(ctx, preflight.Request{
		Action:        preflight.Resume,
		Params:        preflight.Params{Operation: req.Operation, ToForced: req.ToForced},
		Confirmations: req.Confirmations,
	})
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}

	mode := dcctlv1.Operation_MODE_UNSPECIFIED
	if req.ToForced {
		mode = dcctlv1.Operation_FORCED
	}

	if err := answer.Refusal(v, nil, mode); err != nil {
		return nil, err
	}

	name := operationName(req.Operation)

	lk, err := e.cfg.Locker.Acquire(ctx, e.cfg.Self.lockHolder(req.Operation))
	if err != nil {
		return nil, err
	}

	r, from, err := e.resumable(ctx, name, lk, req)
	if err != nil {
		e.release(ctx, lk, name)

		return nil, err
	}

	first, writes, err := r.rec.save(ctx, func(op *dcctlv1.Operation) {
		op.State, op.Error, op.EndTime = dcctlv1.Operation_RUNNING, nil, nil
		op.Leader = e.cfg.Self.proto()
		op.Confirmation = mergeConfirmation(op.GetConfirmation(), v.Confirmed)

		if req.ToForced {
			op.Mode = dcctlv1.Operation_FORCED
		}
	})

	switch {
	case errors.Is(err, errNotRecorded):
		err = &NotRecordedError{Name: name, Datacenters: notWritten(writes), Err: err}
	case err == nil:
		// A copy that did not answer the read may hold another operation.
		err = otherOperation(writes)
	}

	e.journalStart(ctx, first, eventResume, req.Initiator, v)

	if err != nil {
		op, _, serr := r.rec.save(ctx, func(op *dcctlv1.Operation) { markInterrupted(op, err) })
		if serr != nil {
			e.cfg.Log.ErrorCtx(ctx, "resumed operation not recorded", logging.Str("operation", name), logging.Err(serr))
		}

		e.journalEnd(ctx, op, eventEnd, e.system(), nil)
		e.release(ctx, lk, name)
		e.unclaim(name)

		return nil, err
	}

	started = true

	e.cfg.Log.InfoCtx(ctx, "operation resumed", logging.Str("operation", name), logging.Int("from_step", from),
		logging.Int64("revision", first.GetRevision()))

	go r.execute(base, from)

	return first, nil
}

// resumable reads the operation name under the lock and returns the run that
// resumes it and the step to resume from. The operation is claimed by this
// engine when it returns no error.
func (e *Engine) resumable(ctx context.Context, name string, lk *coord.Lock, req ResumeRequest) (*run, int, error) {
	op, view, err := e.interrupted(ctx, name)
	if err != nil {
		return nil, 0, err
	}

	if err := otherUnfinished(view, name); err != nil {
		return nil, 0, err
	}

	if err := refusal(preflight.StepsKnown(op), req.Confirmations); err != nil {
		return nil, 0, fmt.Errorf("%s: %w: %w", name, ErrUnknownStepType, err)
	}

	if err := refusal(preflight.NotRolledBack(op), req.Confirmations); err != nil {
		return nil, 0, fmt.Errorf("%s: %w: %w", name, ErrRolledBack, err)
	}

	resumer := e.cfg.Resumers[op.GetType()]
	if resumer == nil {
		return nil, 0, fmt.Errorf("%s of type %s: %w", name, op.GetType(), ErrNoResumer)
	}

	steps, err := resumer.Resume(ctx, proto.CloneOf(op))
	if err != nil {
		return nil, 0, fmt.Errorf("rebuild the steps of %s: %w", name, err)
	}

	if err := matchSteps(op.GetSteps(), steps); err != nil {
		return nil, 0, fmt.Errorf("rebuild the steps of %s: %w", name, err)
	}

	if !e.claim(name) {
		return nil, 0, &coord.OperationRunningError{Name: copyName(view, op), Operation: op}
	}

	from := len(op.GetSteps())
	resumed := make(map[int]bool)

	for i, st := range op.GetSteps() {
		done := st.GetState() == dcctlv1.Operation_Step_SUCCEEDED || st.GetState() == dcctlv1.Operation_Step_SKIPPED
		if !done && from == len(op.GetSteps()) {
			from = i
		}

		if i >= from && st.GetState() != dcctlv1.Operation_Step_PENDING {
			resumed[i] = true
		}
	}

	given := mergeConfirmations(confirmationsOf(op.GetConfirmation()), req.Confirmations)

	return &run{e: e, rec: e.newRecord(op), lock: lk, steps: steps, given: given, resumed: resumed}, from, nil
}

// interrupted returns the operation name from the copy with the highest
// revision, which must hold it INTERRUPTED, and the copies it read.
func (e *Engine) interrupted(ctx context.Context, name string) (*dcctlv1.Operation, coord.OpView, error) {
	view, err := e.cfg.Ops.Read(ctx)
	if err != nil {
		return nil, view, err
	}

	if c := view.Of(name); c != nil && finishedState(c.Operation.GetState()) {
		return nil, view, &NotInterruptedError{Name: name, State: c.Operation.GetState()}
	}

	op, _ := latest(view, name)
	if op == nil {
		var unread []string

		for _, c := range view.Copies {
			if c.Status == coord.StatusUnreachable || c.Status == coord.StatusInvalid {
				unread = append(unread, c.Name)
			}
		}

		return nil, view, &NotFoundError{Name: name, Unreachable: unread}
	}

	if op.GetState() != dcctlv1.Operation_INTERRUPTED {
		return nil, view, &NotInterruptedError{Name: name, State: op.GetState()}
	}

	return op, view, nil
}

// copyName returns the KV whose copy holds op at its revision.
func copyName(view coord.OpView, op *dcctlv1.Operation) string {
	for _, c := range view.Copies {
		if c.Status == coord.StatusPresent && c.Operation.GetName() == op.GetName() && c.Operation.GetRevision() == op.GetRevision() {
			return c.Name
		}
	}

	return ""
}

// otherUnfinished returns [*coord.OperationRunningError] for a copy that holds
// another operation that is not finished, or that cannot be read: the rule of
// [coord.OpView.Blocking] for a new operation.
func otherUnfinished(view coord.OpView, name string) error {
	for _, c := range view.Copies {
		switch {
		case c.Status == coord.StatusInvalid:
			return &coord.OperationRunningError{Name: c.Name, Err: c.Err}
		case c.Status != coord.StatusPresent || c.Operation.GetName() == name:
			continue
		}

		if of := view.Of(c.Operation.GetName()); !finishedState(of.Operation.GetState()) {
			return &coord.OperationRunningError{Name: c.Name, Operation: c.Operation}
		}
	}

	return nil
}

// refusal returns the refusal of the action by c, decided with given; nil when
// c allows it. It is the refusal the preflight gives for the same check.
func refusal(c check.Check, given check.Confirmations) error {
	return answer.Refusal(check.Decide([]check.Check{c}, given), nil, dcctlv1.Operation_MODE_UNSPECIFIED)
}

// matchSteps checks that steps rebuild the recorded steps one to one.
func matchSteps(recorded []*dcctlv1.Operation_Step, steps []Step) error {
	if len(recorded) != len(steps) {
		return fmt.Errorf("%d steps for an operation of %d", len(steps), len(recorded))
	}

	for i, st := range recorded {
		if steps[i].Type != st.GetType() || steps[i].Stage != st.GetStage() {
			return fmt.Errorf("step %d is %s stage %d, recorded %s stage %d",
				i, steps[i].Type, steps[i].Stage, st.GetType(), st.GetStage())
		}
	}

	return validateSteps(steps)
}

func confirmationsOf(c *dcctlv1.Confirmation) check.Confirmations {
	return check.Confirmations{
		Isolation: c.GetIsolation(), NotMaxLSN: c.GetNotMaxLsn(), MaxDiscardBytes: c.GetMaxDiscardBytes(),
	}
}

func mergeConfirmations(a, b check.Confirmations) check.Confirmations {
	return check.Confirmations{
		Isolation:       a.Isolation || b.Isolation,
		NotMaxLSN:       a.NotMaxLSN || b.NotMaxLSN,
		MaxDiscardBytes: max(a.MaxDiscardBytes, b.MaxDiscardBytes),
	}
}

// mergeConfirmation adds the confirmations a resume took to those the
// operation records.
func mergeConfirmation(c *dcctlv1.Confirmation, taken check.Confirmations) *dcctlv1.Confirmation {
	return answer.Confirmation(mergeConfirmations(confirmationsOf(c), taken))
}
