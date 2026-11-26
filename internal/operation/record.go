package operation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// errNotRecorded means a change of the operation reached no copy of dcctl/op:
// no step runs on a change nobody can see.
var errNotRecorded = errors.New("operation change written to no copy")

// maxText bounds a detail or an error stored with the operation, in bytes.
const maxText = 1 << 10

// record is the operation as one instance changes it. Every change is saved
// to all copies of dcctl/op with the next revision; changes in between, as
// the progress of a step, wait in memory for the next save.
type record struct {
	ops *coord.OpStore
	// saved is called after every save with what was written.
	saved func(ctx context.Context, op *dcctlv1.Operation, writes []coord.OpWrite)

	// saveMu orders the saves, so the copies take the revisions in order.
	saveMu sync.Mutex

	mu sync.Mutex
	op *dcctlv1.Operation
	// changes counts the changes in memory, and written the changes the last
	// recorded save wrote.
	changes, written uint64
}

// change applies fn to the operation in memory; the next save writes it.
func (r *record) change(fn func(op *dcctlv1.Operation)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fn(r.op)
	r.changes++
}

// snapshot returns a copy of the operation.
func (r *record) snapshot() *dcctlv1.Operation {
	r.mu.Lock()
	defer r.mu.Unlock()

	return proto.CloneOf(r.op)
}

// isDirty reports whether a change waits for a save.
func (r *record) isDirty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.changes != r.written
}

// save applies fn, when set, and writes the operation with the next revision
// and update_time now to every copy. It returns the operation as written and
// the result of each copy, and [errNotRecorded] when no copy took it. The
// revision in memory moves on when some copy took the write or may have
// taken it; a write no copy took is written again with the next save. The
// end of ctx does not stop it: each KV call is bounded by the timeout of the
// store.
func (r *record) save(ctx context.Context, fn func(op *dcctlv1.Operation)) (*dcctlv1.Operation, []coord.OpWrite, error) {
	ctx = context.WithoutCancel(ctx)

	r.saveMu.Lock()
	defer r.saveMu.Unlock()

	r.mu.Lock()

	if fn != nil {
		fn(r.op)
		r.changes++
	}

	op := proto.CloneOf(r.op)
	op.Revision++
	op.UpdateTime = timestamppb.Now()
	changes := r.changes

	r.mu.Unlock()

	writes, err := r.ops.Write(ctx, op)
	if err != nil {
		return op, writes, fmt.Errorf("save %s: %w", op.GetName(), err)
	}

	if r.saved != nil {
		r.saved(ctx, op, writes)
	}

	if recorded(writes) || mayBeRecorded(writes) {
		r.mu.Lock()
		r.op.Revision, r.op.UpdateTime = op.GetRevision(), op.GetUpdateTime()
		r.mu.Unlock()
	}

	if !recorded(writes) {
		return op, writes, fmt.Errorf("%s revision %d: %w: %w", op.GetName(), op.GetRevision(), errNotRecorded, writeErrors(writes))
	}

	r.mu.Lock()
	r.written = changes
	r.mu.Unlock()

	return op, writes, nil
}

// mayBeRecorded reports whether a copy may hold what was written: its outcome
// is unknown.
func mayBeRecorded(writes []coord.OpWrite) bool {
	for _, w := range writes {
		if w.Outcome == coord.OpUnknown {
			return true
		}
	}

	return false
}

// recorded reports whether some copy holds what was written.
func recorded(writes []coord.OpWrite) bool {
	for _, w := range writes {
		if w.Outcome == coord.OpWritten {
			return true
		}
	}

	return false
}

func writeErrors(writes []coord.OpWrite) error {
	errs := make([]error, 0, len(writes))

	for _, w := range writes {
		if w.Outcome != coord.OpWritten {
			errs = append(errs, fmt.Errorf("%s: %s: %w", w.Name, w.Outcome, w.Err))
		}
	}

	return errors.Join(errs...)
}

// latest returns the copy of the operation name with the highest revision and
// that revision; nil when no copy holds it. An instance that takes an
// operation over continues from it. Of copies with different changes at that
// revision, as a late write of a leader that lost the lock leaves, a finished
// one wins, then one that is not RUNNING: the leader had stopped.
func latest(view coord.OpView, name string) (*dcctlv1.Operation, int64) {
	var out *dcctlv1.Operation

	for _, c := range view.Copies {
		if c.Status != coord.StatusPresent || c.Operation.GetName() != name {
			continue
		}

		if out == nil || coord.Supersedes(c.Operation, out) {
			out = c.Operation
		}
	}

	if out == nil {
		return nil, 0
	}

	return proto.CloneOf(out), out.GetRevision()
}

// operationName returns the resource name of the operation id.
func operationName(id string) string {
	return "operations/" + id
}

// operationID returns the ID of the operation name.
func operationID(name string) string {
	return strings.TrimPrefix(name, "operations/")
}

// datacenterName returns the resource name of the datacenter dc, empty for no
// datacenter.
func datacenterName(dc string) string {
	if dc == "" {
		return ""
	}

	return "datacenters/" + dc
}

// datacenterID returns the datacenter of a resource name.
func datacenterID(name string) string {
	return strings.TrimPrefix(name, "datacenters/")
}

// truncate returns s cut to [maxText] bytes on a rune boundary.
func truncate(s string) string {
	if len(s) <= maxText {
		return s
	}

	cut := maxText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + "…"
}

// lockHolder is the value of dcctl/lock this instance holds for the operation id.
func (s Instance) lockHolder(id string) kv.Lock {
	return kv.Lock{Instance: s.Node, Datacenter: s.Datacenter, Operation: id}
}
