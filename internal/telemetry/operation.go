package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

var operationTypes = map[dcctlv1.Operation_Type]string{
	dcctlv1.Operation_SWITCH:             "switch",
	dcctlv1.Operation_FAILOVER:           "failover",
	dcctlv1.Operation_DEMOTE:             "demote",
	dcctlv1.Operation_REPOINT:            "repoint",
	dcctlv1.Operation_MARK_RECOVERED:     "mark_recovered",
	dcctlv1.Operation_RESOLVE_CONFLICT:   "resolve_conflict",
	dcctlv1.Operation_INITIALIZE:         "initialize",
	dcctlv1.Operation_INITIALIZE_ARBITER: "initialize_arbiter",
	dcctlv1.Operation_SYNC_APPS:          "sync_apps",
}

// operationStates are the end states an operation duration is recorded in.
var operationStates = map[dcctlv1.Operation_State]string{
	dcctlv1.Operation_SUCCEEDED: "succeeded",
	dcctlv1.Operation_FAILED:    "failed",
	dcctlv1.Operation_ABANDONED: "abandoned",
}

var operationModes = map[dcctlv1.Operation_Mode]string{
	dcctlv1.Operation_QUORUM: "quorum",
	dcctlv1.Operation_FORCED: "forced",
}

// stepTypes are the step types of the registry. PATRONI_FAILOVER has no value there
// yet.
var stepTypes = map[dcctlv1.Operation_Step_Type]string{
	dcctlv1.Operation_Step_APPS_PREFLIGHT:     "apps_preflight",
	dcctlv1.Operation_Step_APPS_STOP:          "apps_stop",
	dcctlv1.Operation_Step_APPS_START:         "apps_start",
	dcctlv1.Operation_Step_GENERATION:         "generation",
	dcctlv1.Operation_Step_VERIFY:             "verify",
	dcctlv1.Operation_Step_PATRONI_DEMOTE:     "patroni_demote",
	dcctlv1.Operation_Step_PATRONI_CATCHUP:    "patroni_catchup",
	dcctlv1.Operation_Step_PATRONI_NO_PRIMARY: "patroni_no_primary",
	dcctlv1.Operation_Step_PATRONI_PROMOTE:    "patroni_promote",
	dcctlv1.Operation_Step_PATRONI_REPOINT:    "patroni_repoint",
	dcctlv1.Operation_Step_PATRONI_FOLLOW:     "patroni_follow",
	dcctlv1.Operation_Step_PATRONI_SWITCHOVER: "patroni_switchover",
}

// stepStates are the end states a step duration is recorded in.
var stepStates = map[dcctlv1.Operation_Step_State]string{
	dcctlv1.Operation_Step_SUCCEEDED: "succeeded",
	dcctlv1.Operation_Step_FAILED:    "failed",
	dcctlv1.Operation_Step_SKIPPED:   "skipped",
}

// OperationRecorder records how long operations and their steps took. The instance
// that leads an operation calls it once per operation and once per step, when they
// end.
type OperationRecorder struct {
	operation metric.Float64Histogram
	step      metric.Float64Histogram
}

// NewOperationRecorder returns a recorder on the instruments of mp.
func NewOperationRecorder(mp metric.MeterProvider) (*OperationRecorder, error) {
	m := mp.Meter(scope)

	op, err := m.Float64Histogram("webitel.dcctl.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of operations, from creation to the end state."),
	)
	if err != nil {
		return nil, fmt.Errorf("create operation duration histogram: %w", err)
	}

	step, err := m.Float64Histogram("webitel.dcctl.step.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of the steps of operations."),
	)
	if err != nil {
		return nil, fmt.Errorf("create step duration histogram: %w", err)
	}

	return &OperationRecorder{operation: op, step: step}, nil
}

// RecordOperation records op, which has reached succeeded, failed or abandoned, as
// lasting from its create time to its end time. An operation that has not ended, or
// a value the metric has no attribute value for, is an error and records nothing.
func (r *OperationRecorder) RecordOperation(ctx context.Context, op *dcctlv1.Operation) error {
	typ, okType := operationTypes[op.GetType()]
	state, okState := operationStates[op.GetState()]
	mode, okMode := operationModes[op.GetMode()]

	switch {
	case !okType:
		return fmt.Errorf("operation type %s has no duration", op.GetType())
	case !okState:
		return fmt.Errorf("operation in state %s has not ended", op.GetState())
	case !okMode:
		return fmt.Errorf("operation mode %s has no duration", op.GetMode())
	}

	d, err := elapsed(op.GetCreateTime(), op.GetEndTime())
	if err != nil {
		return fmt.Errorf("operation %s: %w", op.GetName(), err)
	}

	r.operation.Record(ctx, d.Seconds(), metric.WithAttributes(
		operationTypeKey.String(typ), operationStateKey.String(state), operationModeKey.String(mode),
	))

	return nil
}

// RecordStep records step of an operation of type opType, which has reached
// succeeded, failed or skipped, as lasting from its start time to its end time; a
// skipped step that never started lasts 0. A step that stops or starts managed jobs
// must have its stage. A step that has not ended, or a value the metric has no
// attribute value for, is an error and records nothing.
func (r *OperationRecorder) RecordStep(ctx context.Context, opType dcctlv1.Operation_Type, step *dcctlv1.Operation_Step) error {
	op, okOp := operationTypes[opType]
	typ, okType := stepTypes[step.GetType()]
	state, okState := stepStates[step.GetState()]

	switch {
	case !okOp:
		return fmt.Errorf("operation type %s has no step duration", opType)
	case !okType:
		return fmt.Errorf("step type %s has no duration", step.GetType())
	case !okState:
		return fmt.Errorf("step in state %s has not ended", step.GetState())
	}

	attrs := []attribute.KeyValue{operationTypeKey.String(op), stepTypeKey.String(typ), stepStateKey.String(state)}

	if t := step.GetType(); t == dcctlv1.Operation_Step_APPS_STOP || t == dcctlv1.Operation_Step_APPS_START {
		if step.GetStage() <= 0 {
			return fmt.Errorf("step %s has no stage", t)
		}

		attrs = append(attrs, jobStageKey.Int(int(step.GetStage())))
	}

	start := step.GetStartTime()
	if start == nil && step.GetState() == dcctlv1.Operation_Step_SKIPPED {
		start = step.GetEndTime()
	}

	d, err := elapsed(start, step.GetEndTime())
	if err != nil {
		return fmt.Errorf("step %s: %w", step.GetType(), err)
	}

	r.step.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))

	return nil
}

func elapsed(start, end *timestamppb.Timestamp) (time.Duration, error) {
	if start == nil || end == nil {
		return 0, errors.New("no start or end time")
	}

	d := end.AsTime().Sub(start.AsTime())
	if d < 0 {
		return 0, fmt.Errorf("ends %s before it starts", -d)
	}

	return d, nil
}
