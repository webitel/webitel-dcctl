package operation

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
)

// Start starts the operation of p and returns it as first written, RUNNING at
// revision 1; its steps run in the background.
//
// The preflight runs first and refuses the action with the error of
// [answer.Refusal]. Then the lock is taken, failing with
// [*coord.LockBusyError] or [*coord.LockUnavailableError], and every copy of
// dcctl/op is read. An operation with the request_id of p that the same
// request started is returned as it is, and nothing starts; one another
// request started fails with REQUEST_ID_MISMATCH. An operation that is not
// finished refuses the action with [*coord.OperationRunningError]. An operation whose record reaches no copy
// of dcctl/op, or finds another unfinished operation in a copy that was not
// read, ends FAILED before its first step and Start returns the error.
func (e *Engine) Start(ctx context.Context, p Plan) (*dcctlv1.Operation, error) {
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

	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("operation plan: %w", err)
	}

	v, err := e.cfg.Preflight.Run(ctx, p.Preflight)
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}

	if err := answer.Refusal(v, p.Pending(), p.Mode); err != nil {
		return nil, err
	}

	id := strings.ToLower(rand.Text())

	op, err := e.newOperation(operationName(id), p, v)
	if err != nil {
		return nil, err
	}

	lk, err := e.cfg.Locker.Acquire(ctx, e.cfg.Self.lockHolder(id))
	if err != nil {
		return nil, err
	}

	view, err := e.cfg.Ops.Read(ctx)
	if err == nil {
		var prev *dcctlv1.Operation
		if prev, err = Replay(view, p.RequestID, p.Request); prev != nil {
			e.release(ctx, lk, op.GetName())

			return prev, nil
		}
	}

	if err == nil {
		err = view.CheckIdle()
	}

	if err == nil {
		err = e.checkBase(ctx, p)
	}

	if err != nil {
		e.release(ctx, lk, op.GetName())

		return nil, err
	}

	e.claim(op.GetName())

	rec := e.newRecord(op)

	first, writes, err := rec.save(ctx, nil)

	switch {
	case errors.Is(err, errNotRecorded):
		err = &NotRecordedError{Name: first.GetName(), Datacenters: notWritten(writes), Err: err}
	case err == nil:
		err = otherOperation(writes)
	}

	e.journalStart(ctx, first, eventStart, p.Initiator, v)

	if err != nil {
		e.abort(ctx, rec, lk, err)

		return nil, err
	}

	r := &run{e: e, rec: rec, lock: lk, steps: p.Steps, given: p.Preflight.Confirmations, confirmed: v.Confirmed}
	started = true

	e.cfg.Log.InfoCtx(ctx, "operation started", logging.Str("operation", first.GetName()),
		logging.Str("type", first.GetType().String()), logging.Any("lock", lk.Held()))

	go r.execute(base, 0)

	return first, nil
}

// checkBase checks under the lock that the voters still hold the record p
// starts from, see [coord.Base], and refuses with coord.base_unchanged
// otherwise. The preflight ran before the lock was taken, so another
// operation may have changed the state since.
func (e *Engine) checkBase(ctx context.Context, p Plan) error {
	if p.PreviousGeneration == 0 && p.PreviousActive == "" {
		return nil
	}

	view, err := e.cfg.Coordinator.Read(ctx)
	if err != nil {
		return err
	}

	c := preflight.BaseUnchanged(view, p.Type == dcctlv1.Operation_FAILOVER, p.PreviousGeneration, p.PreviousActive)
	if refused := answer.Refusal(check.Decide([]check.Check{c}, check.Confirmations{}), p.Pending(), p.Mode); refused != nil {
		return fmt.Errorf("%w: %w", ErrStateChanged, refused)
	}

	return nil
}

// abort ends an operation that cannot run its first step FAILED, as nothing
// changed, and releases its lock.
func (e *Engine) abort(ctx context.Context, rec *record, lk *coord.Lock, cause error) {
	op, _, err := rec.save(ctx, func(op *dcctlv1.Operation) {
		op.State, op.Error, op.EndTime = dcctlv1.Operation_FAILED, operationError(cause), timestamppb.Now()
	})
	if err != nil {
		e.cfg.Log.ErrorCtx(ctx, "operation not started and its end not recorded",
			logging.Str("operation", op.GetName()), logging.Err(err))
	}

	e.journalEnd(ctx, op, eventEnd, e.system(), nil)
	e.release(ctx, lk, op.GetName())
	e.unclaim(op.GetName())
}

// notWritten returns the KVs that did not take a write.
func notWritten(writes []coord.OpWrite) []string {
	var out []string

	for _, w := range writes {
		if w.Outcome != coord.OpWritten {
			out = append(out, w.Name)
		}
	}

	return out
}

// otherOperation returns the error of a copy that holds another unfinished
// operation, nil when none does.
func otherOperation(writes []coord.OpWrite) error {
	for _, w := range writes {
		var running *coord.OperationRunningError
		if w.Outcome == coord.OpConflict && errors.As(w.Err, &running) {
			return running
		}
	}

	return nil
}

func (e *Engine) newRecord(op *dcctlv1.Operation) *record {
	return &record{ops: e.cfg.Ops, saved: e.saved, op: op}
}

// newOperation returns the operation of p as it starts, at revision 0.
func (e *Engine) newOperation(name string, p Plan, v check.Verdict) (*dcctlv1.Operation, error) {
	checks, err := answer.Checks(v.Checks)
	if err != nil {
		return nil, fmt.Errorf("operation checks: %w", err)
	}

	op := &dcctlv1.Operation{
		Name:               name,
		Type:               p.Type,
		State:              dcctlv1.Operation_RUNNING,
		Target:             datacenterName(p.Target),
		Confirmation:       answer.Confirmation(v.Confirmed),
		Initiator:          p.Initiator,
		Leader:             e.cfg.Self.proto(),
		Mode:               p.Mode,
		PreviousGeneration: p.PreviousGeneration,
		NewGeneration:      p.NewGeneration,
		Checks:             checks,
		Steps:              p.Pending(),
		Warnings:           answer.Warnings(v),
		RequestId:          p.RequestID,
		CreateTime:         timestamppb.Now(),
	}

	if err := setRequest(op, StoredRequest(p.Request)); err != nil {
		return nil, err
	}

	return op, nil
}
