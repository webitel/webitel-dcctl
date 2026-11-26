package operation

import (
	"context"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// Events in params.event of the journal entries of an operation.
const (
	eventStart     = "start"
	eventResume    = "resume"
	eventEnd       = "end"
	eventAbandon   = "abandon"
	eventRevert    = "revert"
	eventStep      = "step"
	eventProgress  = "progress"
	eventRollback  = "rollback"
	eventWrite     = "write"
	eventLease     = "lease"
	eventInterrupt = "interrupt"
)

// journal appends e. An entry stored in no KV is still in the log; the
// operation goes on, as its record in dcctl/op tells the same.
func (e *Engine) journal(ctx context.Context, entry *dcctlv1.JournalEntry) {
	a, err := e.cfg.Journal.Append(ctx, entry)

	switch {
	case err != nil:
		e.cfg.Log.WarnCtx(ctx, "operation not journaled", logging.Str("operation", entry.GetOperation()), logging.Err(err))
	case a.Stored() == 0:
		e.cfg.Log.WarnCtx(ctx, "journal entry stored in no kv, it is only in the log",
			logging.Str("operation", entry.GetOperation()), logging.Str("entry", a.Entry.GetName()))
	}
}

// system is the initiator of the entries this instance makes by itself.
func (e *Engine) system() *dcctlv1.Initiator {
	return &dcctlv1.Initiator{Instance: e.cfg.Self.proto(), System: true}
}

// journalStart journals the start or the resumption of op by initiator: the
// request, the mode, the generations, the warnings and the confirmations with
// the facts of the checks that needed them.
func (e *Engine) journalStart(ctx context.Context, op *dcctlv1.Operation, event string, initiator *dcctlv1.Initiator, v check.Verdict) {
	params := map[string]any{
		"event":               event,
		"mode":                op.GetMode().String(),
		"previous_generation": op.GetPreviousGeneration(),
		"new_generation":      op.GetNewGeneration(),
		"revision":            op.GetRevision(),
		"steps":               stepTypes(op.GetSteps()),
	}

	if w := op.GetWarnings(); len(w) > 0 {
		params["warnings"] = anySlice(w)
	}

	if req := requestOf(op); req != nil {
		params["request"] = req
	}

	if facts := confirmationFacts(v); len(facts) > 0 {
		params["confirmations"] = facts
	}

	result := dcctlv1.JournalEntry_OK
	if len(op.GetWarnings()) > 0 {
		result = dcctlv1.JournalEntry_WARNING
	}

	e.journal(ctx, &dcctlv1.JournalEntry{
		Type:         dcctlv1.JournalEntry_OPERATION,
		Initiator:    initiator,
		Datacenter:   op.GetTarget(),
		Operation:    op.GetName(),
		Params:       structOf(ctx, e.cfg.Log, params),
		Confirmation: op.GetConfirmation(),
		Result:       result,
		Action:       &dcctlv1.JournalEntry_OperationType{OperationType: op.GetType()},
		Detail:       "operation " + event,
	})
}

// journalEnd journals the state op reached: an end state, or INTERRUPTED.
func (e *Engine) journalEnd(ctx context.Context, op *dcctlv1.Operation, event string, initiator *dcctlv1.Initiator, extra map[string]any) {
	params := map[string]any{"event": event, "state": op.GetState().String(), "revision": op.GetRevision()}
	for k, v := range extra {
		params[k] = v
	}

	entry := &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_OPERATION,
		Initiator:  initiator,
		Datacenter: op.GetTarget(),
		Operation:  op.GetName(),
		Params:     structOf(ctx, e.cfg.Log, params),
		Result:     dcctlv1.JournalEntry_OK,
		Action:     &dcctlv1.JournalEntry_OperationType{OperationType: op.GetType()},
		Detail:     "operation " + op.GetState().String(),
	}

	switch op.GetState() {
	case dcctlv1.Operation_FAILED, dcctlv1.Operation_INTERRUPTED:
		entry.Result, entry.Error = dcctlv1.JournalEntry_ERROR, op.GetError()
	case dcctlv1.Operation_ABANDONED:
		entry.Result = dcctlv1.JournalEntry_WARNING
	case dcctlv1.Operation_SUCCEEDED:
		if hasWarnings(op) {
			entry.Result = dcctlv1.JournalEntry_WARNING
		}
	case dcctlv1.Operation_STATE_UNSPECIFIED, dcctlv1.Operation_RUNNING:
	}

	e.journal(ctx, entry)
}

// journalOperation journals an event of op by initiator other than its start
// and end, as a revert, with its result and error.
func (e *Engine) journalOperation(ctx context.Context, op *dcctlv1.Operation, event string, initiator *dcctlv1.Initiator,
	detail string, result dcctlv1.JournalEntry_Result, err error, extra map[string]any,
) {
	params := map[string]any{"event": event, "revision": op.GetRevision()}
	for k, v := range extra {
		params[k] = v
	}

	entry := &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_OPERATION,
		Initiator:  initiator,
		Datacenter: op.GetTarget(),
		Operation:  op.GetName(),
		Params:     structOf(ctx, e.cfg.Log, params),
		Result:     result,
		Action:     &dcctlv1.JournalEntry_OperationType{OperationType: op.GetType()},
		Detail:     truncate(detail),
	}

	if err != nil {
		entry.Error = operationError(err)
	}

	e.journal(ctx, entry)
}

// journalStep journals an event of step index of op. A failed step, and an
// event with an error, is ERROR; a step with warnings is WARNING.
func (e *Engine) journalStep(ctx context.Context, op *dcctlv1.Operation, index int, event, detail string, err error, extra map[string]any) {
	st := op.GetSteps()[index]
	params := map[string]any{"event": event, "index": index, "state": st.GetState().String(), "revision": op.GetRevision()}

	if st.GetStage() > 0 {
		params["stage"] = st.GetStage()
	}

	if st.GetDatacenter() != "" {
		params["datacenter"] = st.GetDatacenter()
	}

	if w := st.GetWarnings(); len(w) > 0 {
		params["warnings"] = anySlice(w)
	}

	for k, v := range extra {
		params[k] = v
	}

	entry := &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_STEP,
		Initiator:  e.system(),
		Datacenter: op.GetTarget(),
		Operation:  op.GetName(),
		Params:     structOf(ctx, e.cfg.Log, params),
		Result:     dcctlv1.JournalEntry_OK,
		Action:     &dcctlv1.JournalEntry_StepType{StepType: st.GetType()},
		Detail:     truncate(detail),
	}

	switch {
	case err != nil:
		entry.Result, entry.Error = dcctlv1.JournalEntry_ERROR, operationError(err)
	case event == eventStep && st.GetState() == dcctlv1.Operation_Step_FAILED:
		entry.Result = dcctlv1.JournalEntry_ERROR
	case len(st.GetWarnings()) > 0 && event == eventStep:
		entry.Result = dcctlv1.JournalEntry_WARNING
	}

	e.journal(ctx, entry)
}

// journalSystem journals a coordination event of this instance.
func (e *Engine) journalSystem(ctx context.Context, action dcctlv1.JournalEntry_SystemAction, operation, datacenter string,
	result dcctlv1.JournalEntry_Result, detail string, params map[string]any,
) {
	e.journal(ctx, &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_SYSTEM,
		Initiator:  e.system(),
		Datacenter: datacenterName(datacenter),
		Operation:  operation,
		Params:     structOf(ctx, e.cfg.Log, params),
		Result:     result,
		Action:     &dcctlv1.JournalEntry_SystemAction_{SystemAction: action},
		Detail:     truncate(detail),
	})
}

// saved publishes op after a save some copy took and journals the copies
// whose write has an unknown outcome.
func (e *Engine) saved(ctx context.Context, op *dcctlv1.Operation, writes []coord.OpWrite) {
	if recorded(writes) {
		e.bus.publish(op)
	}

	for _, w := range writes {
		if w.Outcome == coord.OpUnknown {
			e.journalSystem(ctx, dcctlv1.JournalEntry_OUTCOME_UNKNOWN, op.GetName(), w.Name, dcctlv1.JournalEntry_WARNING,
				"writing the operation record may have been applied: "+w.Err.Error(),
				map[string]any{"key": kv.OpKey, "revision": op.GetRevision()})
		}
	}
}

// release releases lk and journals the KVs where the release outcome is
// unknown.
func (e *Engine) release(ctx context.Context, lk *coord.Lock, operation string) {
	for _, r := range lk.Release(context.WithoutCancel(ctx)) {
		if r.Outcome == coord.ReleaseUnknown {
			e.journalSystem(ctx, dcctlv1.JournalEntry_OUTCOME_UNKNOWN, operation, r.Name, dcctlv1.JournalEntry_WARNING,
				"releasing the lock may have been applied: "+r.Err.Error(),
				map[string]any{"key": kv.LockKey, "session_destroyed": r.SessionDestroyed})
		}
	}
}

// journalLockLost journals that the lock of operation was lost.
func (e *Engine) journalLockLost(ctx context.Context, operation, datacenter string, err error) {
	e.journalSystem(ctx, dcctlv1.JournalEntry_LOCK_LOST, operation, datacenter, dcctlv1.JournalEntry_ERROR,
		err.Error()+"; the operation is interrupted", nil)
}

func confirmationFacts(v check.Verdict) []any {
	var out []any

	for _, c := range v.Checks {
		if c.Effect != check.Confirm || c.Result != check.Fail {
			continue
		}

		f := map[string]any{"check": string(c.ID), "confirmation": string(c.Confirmation)}
		if c.Datacenter != "" {
			f["datacenter"] = c.Datacenter
		}

		if len(c.Facts) > 0 {
			f["facts"] = c.Facts
		}

		out = append(out, f)
	}

	return out
}

// requestOf returns the request of op as a JSON object, nil when it has none.
func requestOf(op *dcctlv1.Operation) map[string]any {
	var req proto.Message

	switch r := op.GetRequest().(type) {
	case *dcctlv1.Operation_SwitchDatacenter:
		req = r.SwitchDatacenter
	case *dcctlv1.Operation_FailoverDatacenter:
		req = r.FailoverDatacenter
	case *dcctlv1.Operation_DemoteDatacenter:
		req = r.DemoteDatacenter
	case *dcctlv1.Operation_RepointDatacenter:
		req = r.RepointDatacenter
	case *dcctlv1.Operation_MarkDatacenterRecovered:
		req = r.MarkDatacenterRecovered
	case *dcctlv1.Operation_ResolveConflict:
		req = r.ResolveConflict
	case *dcctlv1.Operation_InitializeDatacenters:
		req = r.InitializeDatacenters
	case *dcctlv1.Operation_InitializeArbiter:
		req = r.InitializeArbiter
	case *dcctlv1.Operation_SyncApps:
		req = r.SyncApps
	default:
		return nil
	}

	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		return nil
	}

	var s structpb.Struct
	if err := protojson.Unmarshal(b, &s); err != nil {
		return nil
	}

	return s.AsMap()
}

func stepTypes(steps []*dcctlv1.Operation_Step) []any {
	out := make([]any, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.GetType().String())
	}

	return out
}

func hasWarnings(op *dcctlv1.Operation) bool {
	if len(op.GetWarnings()) > 0 {
		return true
	}

	for _, s := range op.GetSteps() {
		if len(s.GetWarnings()) > 0 {
			return true
		}
	}

	return false
}

func anySlice[T any](in []T) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}

	return out
}

// structOf returns params as a Struct; params it cannot hold are logged and
// left out.
func structOf(ctx context.Context, log *logging.Logger, params map[string]any) *structpb.Struct {
	if len(params) == 0 {
		return nil
	}

	s, err := structpb.NewStruct(params)
	if err != nil {
		log.WarnCtx(ctx, "journal params left out", logging.Err(err))

		return nil
	}

	return s
}
