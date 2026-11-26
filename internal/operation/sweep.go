package operation

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

// sweep interrupts the RUNNING operations no live instance runs, judged by the
// copy with the highest revision. An operation of this instance that its
// engine does not run was left by a restart and is interrupted at once. One of
// another instance is interrupted only when this instance takes dcctl/lock for
// it: no live session holds the lock in the KVs that answered. A leader cut off
// from them loses its sessions there and stops on its own.
func (e *Engine) sweep(ctx context.Context) {
	view, err := e.cfg.Ops.Read(ctx)
	if err != nil {
		return
	}

	for _, name := range runningNames(view) {
		if e.Runs(name) {
			continue
		}

		op, _ := latest(view, name)

		if e.cfg.Self.leads(op) {
			e.interruptDead(ctx, name, errRestarted, nil)

			continue
		}

		e.interruptOrphan(ctx, name)
	}

	for _, name := range divergentNames(view) {
		if !e.Runs(name) {
			e.reconcile(ctx, name)
		}
	}
}

// reconcile writes the change of the operation name that [latest] picks, at
// the next revision, over copies that hold another change of its highest
// revision. It does so under dcctl/lock, which a live leader holds.
func (e *Engine) reconcile(ctx context.Context, name string) {
	lk, err := e.cfg.Locker.Acquire(ctx, e.cfg.Self.lockHolder(operationID(name)))
	if err != nil {
		e.cfg.Log.DebugCtx(ctx, "copies of the operation are not reconciled", logging.Str("operation", name), logging.Err(err))

		return
	}

	defer e.release(ctx, lk, name)

	view, err := e.cfg.Ops.Read(ctx)
	if err != nil || !diverges(view, name) || e.Runs(name) {
		return
	}

	op, rev := latest(view, name)

	saved, _, err := e.newRecord(op).save(ctx, nil)
	if err != nil {
		e.cfg.Log.WarnCtx(ctx, "copies of the operation are not reconciled", logging.Str("operation", name), logging.Err(err))

		return
	}

	e.journalSystem(ctx, dcctlv1.JournalEntry_ALIGN_OPERATION, name, "", dcctlv1.JournalEntry_OK,
		fmt.Sprintf("copies held different changes at revision %d; the %s one is written at revision %d",
			rev, saved.GetState(), saved.GetRevision()),
		map[string]any{"revision_from": rev, "revision": saved.GetRevision()})
}

// interruptOrphan interrupts the operation name under dcctl/lock.
func (e *Engine) interruptOrphan(ctx context.Context, name string) {
	lk, err := e.cfg.Locker.Acquire(ctx, e.cfg.Self.lockHolder(operationID(name)))
	if err != nil {
		e.cfg.Log.DebugCtx(ctx, "running operation keeps its lock", logging.Str("operation", name), logging.Err(err))

		return
	}

	defer e.release(ctx, lk, name)

	params := map[string]any{"held": anySlice(lk.Held())}

	if u := lk.Unreachable(); len(u) > 0 {
		names := make([]any, 0, len(u))
		for _, k := range u {
			names = append(names, k.Name)
		}

		params["unreachable"] = names
	}

	if e.interruptDead(ctx, name, errOrphaned, params) {
		e.journalSystem(ctx, dcctlv1.JournalEntry_LOCK_LOST, name, "", dcctlv1.JournalEntry_ERROR,
			errOrphaned.Error()+"; the operation is interrupted", params)
	}
}

// interruptDead interrupts the operation name with cause when a fresh read
// still finds it RUNNING, this engine does not run it and, after a restart,
// this instance still leads it; it reports whether it did. params go to the
// journal entry.
func (e *Engine) interruptDead(ctx context.Context, name string, cause error, params map[string]any) bool {
	view, err := e.cfg.Ops.Read(ctx)
	if err != nil {
		return false
	}

	op, _ := latest(view, name)

	switch {
	case op.GetState() != dcctlv1.Operation_RUNNING, e.Runs(name):
		return false
	case errors.Is(cause, errRestarted) && !e.cfg.Self.leads(op):
		// Another instance took the operation over: it runs it under the lock.
		return false
	}

	saved, _, err := e.newRecord(op).save(ctx, func(op *dcctlv1.Operation) { markInterrupted(op, cause) })
	if err != nil {
		e.cfg.Log.WarnCtx(ctx, "operation not interrupted", logging.Str("operation", name), logging.Err(err))

		return false
	}

	e.journalEnd(ctx, saved, eventInterrupt, e.system(), params)
	e.cfg.Log.WarnCtx(ctx, "operation interrupted", logging.Str("operation", name), logging.Err(cause))

	return true
}

// divergentNames returns the unfinished operations whose copies hold
// different changes at their highest revision, in the order of the copies.
func divergentNames(view coord.OpView) []string {
	var out []string

	seen := make(map[string]bool)

	for _, c := range view.Copies {
		if c.Status != coord.StatusPresent || seen[c.Operation.GetName()] {
			continue
		}

		seen[c.Operation.GetName()] = true

		if diverges(view, c.Operation.GetName()) {
			out = append(out, c.Operation.GetName())
		}
	}

	return out
}

// diverges reports whether the copies of the unfinished operation name hold
// different changes at its highest revision. A finished copy is copied over
// the others by [coord.OpStore.Align].
func diverges(view coord.OpView, name string) bool {
	op, rev := latest(view, name)
	if op == nil || finishedState(op.GetState()) {
		return false
	}

	for _, c := range view.Copies {
		if c.Status == coord.StatusPresent && c.Operation.GetName() == name &&
			c.Operation.GetRevision() == rev && !proto.Equal(c.Operation, op) {
			return true
		}
	}

	return false
}

// runningNames returns the operations whose copy with the highest revision
// holds them RUNNING, in the order of the copies.
func runningNames(view coord.OpView) []string {
	var out []string

	seen := make(map[string]bool)

	for _, c := range view.Copies {
		if c.Status != coord.StatusPresent || seen[c.Operation.GetName()] {
			continue
		}

		name := c.Operation.GetName()
		seen[name] = true

		if op, _ := latest(view, name); op.GetState() == dcctlv1.Operation_RUNNING {
			out = append(out, name)
		}
	}

	return out
}
