package telemetry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

func newRecorder(t *testing.T) (*OperationRecorder, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, mp.Shutdown(t.Context())) })

	r, err := NewOperationRecorder(mp)
	require.NoError(t, err)

	return r, reader
}

func at(d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(observeTime.Add(d))
}

func TestRecordOperation(t *testing.T) {
	r, reader := newRecorder(t)

	require.NoError(t, r.RecordOperation(t.Context(), &dcctlv1.Operation{
		Type: dcctlv1.Operation_SWITCH, State: dcctlv1.Operation_SUCCEEDED, Mode: dcctlv1.Operation_QUORUM,
		CreateTime: at(0), EndTime: at(90 * time.Second),
	}))
	require.NoError(t, r.RecordOperation(t.Context(), &dcctlv1.Operation{
		Type: dcctlv1.Operation_RESOLVE_CONFLICT, State: dcctlv1.Operation_ABANDONED, Mode: dcctlv1.Operation_FORCED,
		CreateTime: at(0), EndTime: at(1500 * time.Millisecond),
	}))

	got := collectSeries(t, reader)

	assert.Equal(t, series{kind: kindHistogram, unit: "s", points: map[string]float64{
		"webitel.dcctl.operation.mode=quorum,webitel.dcctl.operation.state=succeeded,webitel.dcctl.operation.type=switch":           90,
		"webitel.dcctl.operation.mode=forced,webitel.dcctl.operation.state=abandoned,webitel.dcctl.operation.type=resolve_conflict": 1.5,
	}}, got["webitel.dcctl.operation.duration"])
}

func TestRecordOperation_rejected(t *testing.T) {
	ended := func() *dcctlv1.Operation {
		return &dcctlv1.Operation{
			Type: dcctlv1.Operation_FAILOVER, State: dcctlv1.Operation_FAILED, Mode: dcctlv1.Operation_FORCED,
			CreateTime: at(0), EndTime: at(time.Minute),
		}
	}

	tests := []struct {
		name   string
		modify func(op *dcctlv1.Operation)
	}{
		{name: "running", modify: func(op *dcctlv1.Operation) { op.State = dcctlv1.Operation_RUNNING }},
		{name: "interrupted", modify: func(op *dcctlv1.Operation) { op.State = dcctlv1.Operation_INTERRUPTED }},
		{name: "no_type", modify: func(op *dcctlv1.Operation) { op.Type = dcctlv1.Operation_TYPE_UNSPECIFIED }},
		{name: "no_mode", modify: func(op *dcctlv1.Operation) { op.Mode = dcctlv1.Operation_MODE_UNSPECIFIED }},
		{name: "no_end_time", modify: func(op *dcctlv1.Operation) { op.EndTime = nil }},
		{name: "no_create_time", modify: func(op *dcctlv1.Operation) { op.CreateTime = nil }},
		{name: "end_before_create", modify: func(op *dcctlv1.Operation) { op.EndTime = at(-time.Second) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, reader := newRecorder(t)
			op := ended()
			tt.modify(op)

			require.Error(t, r.RecordOperation(t.Context(), op))
			assert.Empty(t, collectSeries(t, reader)["webitel.dcctl.operation.duration"].points)
		})
	}
}

func TestRecordStep(t *testing.T) {
	r, reader := newRecorder(t)

	require.NoError(t, r.RecordStep(t.Context(), dcctlv1.Operation_SWITCH, &dcctlv1.Operation_Step{
		Type: dcctlv1.Operation_Step_APPS_STOP, Stage: 2, State: dcctlv1.Operation_Step_SUCCEEDED,
		StartTime: at(0), EndTime: at(20 * time.Second),
	}))
	require.NoError(t, r.RecordStep(t.Context(), dcctlv1.Operation_SWITCH, &dcctlv1.Operation_Step{
		Type: dcctlv1.Operation_Step_PATRONI_CATCHUP, State: dcctlv1.Operation_Step_FAILED,
		StartTime: at(0), EndTime: at(3 * time.Second),
	}))
	require.NoError(t, r.RecordStep(t.Context(), dcctlv1.Operation_SWITCH, &dcctlv1.Operation_Step{
		Type: dcctlv1.Operation_Step_VERIFY, State: dcctlv1.Operation_Step_SKIPPED, EndTime: at(0),
	}))
	require.NoError(t, r.RecordStep(t.Context(), dcctlv1.Operation_FAILOVER, &dcctlv1.Operation_Step{
		Type: dcctlv1.Operation_Step_PATRONI_FAILOVER, State: dcctlv1.Operation_Step_SUCCEEDED,
		StartTime: at(0), EndTime: at(5 * time.Second),
	}))

	got := collectSeries(t, reader)

	assert.Equal(t, series{kind: kindHistogram, unit: "s", points: map[string]float64{
		"webitel.dcctl.job.stage=2,webitel.dcctl.operation.type=switch,webitel.dcctl.step.state=succeeded,webitel.dcctl.step.type=apps_stop": 20,
		"webitel.dcctl.operation.type=switch,webitel.dcctl.step.state=failed,webitel.dcctl.step.type=patroni_catchup":                        3,
		"webitel.dcctl.operation.type=switch,webitel.dcctl.step.state=skipped,webitel.dcctl.step.type=verify":                                0,
		"webitel.dcctl.operation.type=failover,webitel.dcctl.step.state=succeeded,webitel.dcctl.step.type=patroni_failover":                  5,
	}}, got["webitel.dcctl.step.duration"])
}

func TestRecordStep_rejected(t *testing.T) {
	done := func() *dcctlv1.Operation_Step {
		return &dcctlv1.Operation_Step{
			Type: dcctlv1.Operation_Step_APPS_START, Stage: 1, State: dcctlv1.Operation_Step_SUCCEEDED,
			StartTime: at(0), EndTime: at(time.Second),
		}
	}

	tests := []struct {
		name   string
		opType dcctlv1.Operation_Type
		modify func(s *dcctlv1.Operation_Step)
	}{
		{name: "running", opType: dcctlv1.Operation_SWITCH, modify: func(s *dcctlv1.Operation_Step) { s.State = dcctlv1.Operation_Step_RUNNING }},
		{name: "no_operation_type", modify: func(*dcctlv1.Operation_Step) {}},
		{name: "no_start_time", opType: dcctlv1.Operation_SWITCH, modify: func(s *dcctlv1.Operation_Step) { s.StartTime = nil }},
		{name: "no_end_time", opType: dcctlv1.Operation_SWITCH, modify: func(s *dcctlv1.Operation_Step) { s.EndTime = nil }},
		{name: "stage_missing", opType: dcctlv1.Operation_SWITCH, modify: func(s *dcctlv1.Operation_Step) { s.Stage = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, reader := newRecorder(t)
			s := done()
			tt.modify(s)

			require.Error(t, r.RecordStep(t.Context(), tt.opType, s))
			assert.Empty(t, collectSeries(t, reader)["webitel.dcctl.step.duration"].points)
		})
	}
}
