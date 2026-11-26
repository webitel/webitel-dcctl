package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// opWriteAttempts bounds the re-reads of one copy that keeps changing or
// whose write has an unknown outcome.
const opWriteAttempts = 3

var (
	// ErrOperationFinished is the error of [OpConflict] when the copy holds
	// the operation in a final state: it is never changed again.
	ErrOperationFinished = errors.New("operation is finished")
	// ErrOpSuperseded is the error of [OpConflict] when the copy holds another
	// change of the operation at the revision written or above: another
	// instance wrote it since.
	ErrOpSuperseded = errors.New("copy holds a newer change of the operation")
)

// OperationRunningError means a copy of dcctl/op holds an operation that is
// not finished, or cannot be read and so may hold one. Its gRPC status is
// FailedPrecondition OPERATION_RUNNING with metadata operation.
type OperationRunningError struct {
	// Name is the KV of the copy.
	Name string
	// Operation is nil when the copy cannot be read.
	Operation *dcctlv1.Operation
	// Err is why the copy cannot be read.
	Err error
}

func (e *OperationRunningError) Error() string {
	if e.Operation == nil {
		return fmt.Sprintf("%s in %s cannot be read and may hold an unfinished operation: %v", kv.OpKey, e.Name, e.Err)
	}

	return fmt.Sprintf("operation %s is %s in %s", e.Operation.GetName(), e.Operation.GetState(), e.Name)
}

func (e *OperationRunningError) Unwrap() error { return e.Err }

// GRPCStatus returns the status the API answers with.
func (e *OperationRunningError) GRPCStatus() *status.Status {
	var md map[string]string
	if name := e.Operation.GetName(); name != "" {
		md = map[string]string{"operation": name}
	}

	return apierr.Status(apierr.ReasonOperationRunning, e.Error(), md)
}

// OpCopy is dcctl/op as read from one KV.
type OpCopy struct {
	Name string
	// Status is [StatusPresent], [StatusAbsent], [StatusUnreachable] or
	// [StatusInvalid].
	Status Status
	// Reason is why the KV is unreachable or the copy invalid.
	Reason source.Reason
	Err    error
	// Operation, Raw and Version are set for StatusPresent; Raw and Version
	// also for StatusInvalid. Raw is the value as stored.
	Operation *dcctlv1.Operation
	Raw       []byte
	Version   Version
}

// OpView is dcctl/op of every KV, in name order.
type OpView struct {
	Copies []OpCopy
}

// Latest returns the copy with the newest operation, by create_time, and
// of it the latest update, by update_time; nil when no copy is present.
func (v OpView) Latest() *OpCopy {
	var latest *OpCopy

	for i := range v.Copies {
		c := &v.Copies[i]
		if c.Status == StatusPresent && (latest == nil || newer(c.Operation, latest.Operation)) {
			latest = c
		}
	}

	return latest
}

func newer(a, b *dcctlv1.Operation) bool {
	if c := a.GetCreateTime().AsTime().Compare(b.GetCreateTime().AsTime()); c != 0 {
		return c > 0
	}

	return a.GetUpdateTime().AsTime().After(b.GetUpdateTime().AsTime())
}

// Primary returns the copy of the latest operation in the KV of its leader's
// datacenter, or the only copy in stretch; nil when that KV has no copy of
// the operation.
func (v OpView) Primary() *OpCopy {
	latest := v.Latest()
	if latest == nil || len(v.Copies) == 1 {
		return latest
	}

	leader := datacenterID(latest.Operation.GetLeader().GetDatacenter())

	for i := range v.Copies {
		c := &v.Copies[i]
		if c.Name == leader && c.Status == StatusPresent && c.Operation.GetName() == latest.Operation.GetName() {
			return c
		}
	}

	return nil
}

// Of returns the copy that answers for the operation name: the latest
// updated copy that holds it finished, since an unfinished copy of a
// finished operation is stale; else the copy in the KV of its leader's
// datacenter; else its latest update. It returns nil when no copy holds the
// operation.
func (v OpView) Of(name string) *OpCopy {
	if c := v.final(name); c != nil {
		return c
	}

	var of OpView

	for _, c := range v.Copies {
		if c.Status == StatusPresent && c.Operation.GetName() == name {
			of.Copies = append(of.Copies, c)
		}
	}

	if c := of.Primary(); c != nil {
		return c
	}

	return of.Latest()
}

// Blocking returns the first copy that keeps a new operation from starting:
// one with an operation that is not finished, or one that cannot be read.
// An operation in a state this dcctl does not know is not finished. A copy
// of an operation that another copy holds finished is stale, see
// [OpStore.Align], and does not block.
func (v OpView) Blocking() *OpCopy {
	for i := range v.Copies {
		c := &v.Copies[i]

		switch c.Status {
		case StatusInvalid:
			return c
		case StatusPresent:
			if !finished(c.Operation.GetState()) && v.final(c.Operation.GetName()) == nil {
				return c
			}
		case StatusAbsent, StatusUnreachable, StatusUninitialized, StatusStale:
		}
	}

	return nil
}

// final returns the copy that holds the operation name finished, the latest
// updated of them; nil when none does.
func (v OpView) final(name string) *OpCopy {
	var out *OpCopy

	for i := range v.Copies {
		c := &v.Copies[i]

		if c.Status != StatusPresent || c.Operation.GetName() != name || !finished(c.Operation.GetState()) {
			continue
		}

		if out == nil || c.Operation.GetUpdateTime().AsTime().After(out.Operation.GetUpdateTime().AsTime()) {
			out = c
		}
	}

	return out
}

// CheckIdle returns [*OperationRunningError] for the [OpView.Blocking]
// copy. The KVs that did not answer are not checked. Call it under the lock,
// before the new operation is written.
func (v OpView) CheckIdle() error {
	c := v.Blocking()
	if c == nil {
		return nil
	}

	return &OperationRunningError{Name: c.Name, Operation: c.Operation, Err: c.Err}
}

func finished(s dcctlv1.Operation_State) bool {
	switch s {
	case dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_FAILED, dcctlv1.Operation_ABANDONED:
		return true
	case dcctlv1.Operation_STATE_UNSPECIFIED, dcctlv1.Operation_RUNNING, dcctlv1.Operation_INTERRUPTED:
	}

	return false
}

// datacenterID returns the datacenter of a name that may be the resource
// name datacenters/{datacenter}.
func datacenterID(name string) string {
	return strings.TrimPrefix(name, "datacenters/")
}

// OpWriteOutcome is what writing dcctl/op to one KV did.
type OpWriteOutcome string

const (
	OpWritten OpWriteOutcome = "written"
	// OpConflict means the copy was left as it is: it holds another
	// operation that is not finished ([*OperationRunningError]), cannot be
	// read, holds this operation finished ([ErrOperationFinished]), or holds
	// another change of it at the same or a higher revision
	// ([ErrOpSuperseded]).
	OpConflict OpWriteOutcome = "conflict"
	// OpMismatch means the copy changed between every read and its write.
	OpMismatch OpWriteOutcome = "mismatch"
	// OpUnknown means a write may have been applied and no later read told
	// whether it was; Err matches [ErrOutcomeUnknown].
	OpUnknown OpWriteOutcome = "unknown"
	OpFailed  OpWriteOutcome = "failed"
)

// OpWrite is the result of writing dcctl/op to one KV.
type OpWrite struct {
	Name string
	// Primary is set for the KV of the leader's datacenter, and for the
	// only KV in stretch.
	Primary bool
	Outcome OpWriteOutcome
	Err     error
}

// OpStore reads and writes dcctl/op in the KVs.
type OpStore struct {
	kvs     []KV
	timeout time.Duration
	log     *logging.Logger
}

// NewOpStore returns the store of kvs; timeout bounds each call to a KV.
func NewOpStore(kvs []KV, timeout time.Duration, log *logging.Logger) (*OpStore, error) {
	sorted, err := sortedKVs(kvs)
	if err != nil {
		return nil, err
	}

	if timeout <= 0 {
		return nil, errors.New("kv timeout must be positive")
	}

	return &OpStore{kvs: sorted, timeout: timeout, log: log}, nil
}

// Read reads dcctl/op from every KV in parallel. It fails only when ctx
// ends.
func (s *OpStore) Read(ctx context.Context) (OpView, error) {
	copies := make([]OpCopy, len(s.kvs))

	var wg sync.WaitGroup

	for i, k := range s.kvs {
		wg.Go(func() { copies[i] = s.readCopy(ctx, k) })
	}

	wg.Wait()

	if err := ctx.Err(); err != nil {
		return OpView{}, fmt.Errorf("read %s: %w", kv.OpKey, err)
	}

	return OpView{Copies: copies}, nil
}

func (s *OpStore) readCopy(ctx context.Context, k KV) OpCopy {
	c := OpCopy{Name: k.Name}

	e, err := withTimeout(ctx, s.timeout, func(ctx context.Context) (*consul.KVEntry, error) {
		e, _, err := k.Client.KVGet(ctx, kv.OpKey)

		return e, err
	})

	switch {
	case errors.Is(err, consul.ErrKeyNotFound):
		c.Status = StatusAbsent

		return c
	case err != nil:
		c.Status, c.Err = StatusUnreachable, err
		c.Reason, _ = source.ReasonOf(err)

		return c
	}

	c.Raw, c.Version = e.Value, Version(e.ModifyIndex)

	op := new(dcctlv1.Operation)
	if err := kv.DecodeProto(e.Value, op); err != nil {
		c.Status, c.Reason, c.Err = StatusInvalid, source.ReasonInvalid, fmt.Errorf("decode %s: %w", kv.OpKey, err)

		return c
	}

	c.Status, c.Operation = StatusPresent, op

	return c
}

// Write writes op to every KV in parallel and returns what happened on
// each, in name order. Every write uses the version of a read made right
// before it, and an unknown outcome is read again. A copy of the same
// operation keeps the fields this dcctl does not know. A copy holding
// another operation that is not finished, or that cannot be read, is never
// overwritten, and neither is a copy of the same operation at the revision of
// op or above, which another instance wrote since; an op without a revision,
// as an older dcctl writes it, is not compared. Write fails only for an op
// without a name or when ctx ends.
func (s *OpStore) Write(ctx context.Context, op *dcctlv1.Operation) ([]OpWrite, error) {
	if op.GetName() == "" {
		return nil, errors.New("operation has no name")
	}

	value, err := kv.EncodeProto(op)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", kv.OpKey, err)
	}

	primary := datacenterID(op.GetLeader().GetDatacenter())
	if len(s.kvs) == 1 {
		primary = s.kvs[0].Name
	}

	out := make([]OpWrite, len(s.kvs))

	var wg sync.WaitGroup

	for i, k := range s.kvs {
		wg.Go(func() {
			out[i] = s.writeCopy(ctx, k, op, value)
			out[i].Primary = k.Name == primary
		})
	}

	wg.Wait()

	for _, w := range out {
		if w.Outcome != OpWritten {
			s.log.WarnCtx(ctx, "operation record not written",
				logging.Str("kv", w.Name), logging.Str("operation", op.GetName()),
				logging.Str("outcome", string(w.Outcome)), logging.Bool("primary", w.Primary), logging.Err(w.Err))
		}
	}

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("write %s: %w", kv.OpKey, err)
	}

	return out, nil
}

func (s *OpStore) writeCopy(ctx context.Context, k KV, op *dcctlv1.Operation, value []byte) OpWrite {
	// unknown is set once a write of this call may have been applied: a
	// later read that fails or finds the copy changed does not tell
	// whether it was.
	var unknown error

	w := OpWrite{Name: k.Name}

	for range opWriteAttempts {
		var retry bool

		w, retry = s.attempt(ctx, k, op, value)
		if w.Outcome == OpUnknown {
			unknown = w.Err
		}

		if !retry || ctx.Err() != nil {
			break
		}
	}

	if unknown != nil && w.Outcome != OpWritten && w.Outcome != OpUnknown {
		w.Outcome, w.Err = OpUnknown, errors.Join(unknown, w.Err)
	}

	return w
}

// attempt reads the copy in k and writes op over it, and reports whether
// to read and try again.
func (s *OpStore) attempt(ctx context.Context, k KV, op *dcctlv1.Operation, value []byte) (OpWrite, bool) {
	w := OpWrite{Name: k.Name}
	cur := s.readCopy(ctx, k)

	next, done := nextValue(cur, op, value, &w)
	if done {
		return w, false
	}

	ok, err := withTimeout(ctx, s.timeout, func(ctx context.Context) (bool, error) {
		return k.Client.KVCAS(ctx, kv.OpKey, next, uint64(cur.Version))
	})

	switch {
	case err == nil && ok:
		w.Outcome = OpWritten

		return w, false
	case err == nil:
		w.Outcome, w.Err = OpMismatch, fmt.Errorf("%s %s at index %d: %w", k.Name, kv.OpKey, cur.Version, ErrVersionMismatch)

		return w, true
	case errors.Is(err, consul.ErrOutcomeUnknown):
		w.Outcome, w.Err = OpUnknown, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)

		return w, true
	}

	w.Outcome, w.Err = OpFailed, err

	return w, false
}

// Align writes the final copy of an operation over the copies that still
// hold it unfinished, as a KV that missed the last writes has, and returns
// what happened on each. A copy is read again right before its write, and
// the write uses the version of that read; the value written is the final
// copy as stored. Copies of other operations are left as they are. Align
// fails only when ctx ends.
func (s *OpStore) Align(ctx context.Context) ([]Alignment, error) {
	view, err := s.Read(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]*Alignment, len(s.kvs))

	var wg sync.WaitGroup

	for i, c := range view.Copies {
		if c.Status != StatusPresent || finished(c.Operation.GetState()) {
			continue
		}

		final := view.final(c.Operation.GetName())
		if final == nil {
			continue
		}

		wg.Go(func() {
			a := s.alignCopy(ctx, s.kvs[i], final)
			results[i] = &a
		})
	}

	wg.Wait()

	var out []Alignment

	for _, a := range results {
		if a != nil {
			out = append(out, *a)
		}
	}

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("align %s: %w", kv.OpKey, err)
	}

	return out, nil
}

func (s *OpStore) alignCopy(ctx context.Context, k KV, final *OpCopy) Alignment {
	a := Alignment{Voter: k.Name}
	fresh := s.readCopy(ctx, k)

	switch {
	case fresh.Status == StatusUnreachable:
		a.Outcome, a.Err = AlignFailed, fresh.Err

		return a
	case fresh.Status != StatusPresent,
		fresh.Operation.GetName() != final.Operation.GetName(),
		finished(fresh.Operation.GetState()):
		a.Outcome = AlignSkipped

		return a
	}

	ok, err := withTimeout(ctx, s.timeout, func(ctx context.Context) (bool, error) {
		return k.Client.KVCAS(ctx, kv.OpKey, final.Raw, uint64(fresh.Version))
	})

	switch {
	case err == nil && ok:
		a.Outcome = AlignWritten

		s.log.InfoCtx(ctx, "operation record aligned",
			logging.Str("kv", k.Name), logging.Str("operation", final.Operation.GetName()),
			logging.Str("from", final.Name), logging.Str("state", final.Operation.GetState().String()))
	case err == nil:
		a.Outcome, a.Err = AlignMismatch, fmt.Errorf("%s %s at index %d: %w", k.Name, kv.OpKey, fresh.Version, ErrVersionMismatch)
	case errors.Is(err, consul.ErrOutcomeUnknown):
		a.Outcome, a.Err = AlignUnknown, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	default:
		a.Outcome, a.Err = AlignFailed, err
	}

	return a
}

// nextValue returns the value to write over cur, or done with the outcome set in
// w when nothing is to be written.
func nextValue(cur OpCopy, op *dcctlv1.Operation, value []byte, w *OpWrite) ([]byte, bool) {
	switch cur.Status {
	case StatusAbsent:
		return value, false
	case StatusUnreachable, StatusStale:
		w.Outcome, w.Err = OpFailed, cur.Err

		return nil, true
	case StatusInvalid:
		w.Outcome, w.Err = OpConflict, &OperationRunningError{Name: cur.Name, Err: cur.Err}

		return nil, true
	case StatusPresent, StatusUninitialized:
	}

	switch {
	case cur.Operation.GetName() != op.GetName():
		if !finished(cur.Operation.GetState()) {
			w.Outcome, w.Err = OpConflict, &OperationRunningError{Name: cur.Name, Operation: cur.Operation}

			return nil, true
		}

		return value, false
	case proto.Equal(cur.Operation, op):
		// Before the revision check: a retry after an unknown outcome finds its own write.
		w.Outcome, w.Err = OpWritten, nil

		return nil, true
	case finished(cur.Operation.GetState()):
		w.Outcome = OpConflict
		w.Err = fmt.Errorf("%s in %s: %s is %s: %w", kv.OpKey, cur.Name, op.GetName(), cur.Operation.GetState(), ErrOperationFinished)

		return nil, true
	case op.GetRevision() > 0 && cur.Operation.GetRevision() >= op.GetRevision():
		w.Outcome = OpConflict
		w.Err = fmt.Errorf("%s in %s: %s is at revision %d, not below %d: %w",
			kv.OpKey, cur.Name, op.GetName(), cur.Operation.GetRevision(), op.GetRevision(), ErrOpSuperseded)

		return nil, true
	}

	merged, err := kv.PreserveUnknown(op.ProtoReflect().Descriptor(), cur.Raw, value)
	if err != nil {
		w.Outcome, w.Err = OpFailed, fmt.Errorf("%s in %s: %w", kv.OpKey, cur.Name, err)

		return nil, true
	}

	return merged, false
}
