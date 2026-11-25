package fencing

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/collect"
)

const scope = "github.com/webitel/webitel-dcctl/internal/fencing"

// datacenterNameKey is webitel.datacenter.name of the webitel semantic conventions
// registry, which has no generated Go package yet.
const datacenterNameKey = attribute.Key("webitel.datacenter.name")

// errorTypeTimeout is the error.type of a stop that ran out of apps.stop_timeout.
const errorTypeTimeout = "timeout"

func newStopsCounter(mp metric.MeterProvider) (metric.Int64Counter, error) {
	c, err := mp.Meter(scope).Int64Counter("webitel.dcctl.fencing.stops",
		metric.WithUnit("{stop}"),
		metric.WithDescription("Number of fencing stops of the datacenter of the instance."),
	)
	if err != nil {
		return nil, fmt.Errorf("create fencing stops counter: %w", err)
	}

	return c, nil
}

// report counts the stop that ended with err and journals it with res, the stages
// that ran.
func (f *Fencer) report(ctx context.Context, observer collect.Instance, t trigger, res apps.Result, err error) {
	dc := f.cfg.Datacenter.Name
	attrs := []attribute.KeyValue{datacenterNameKey.String(dc)}

	entry := &dcctlv1.JournalEntry{
		Type:   dcctlv1.JournalEntry_SYSTEM,
		Action: &dcctlv1.JournalEntry_SystemAction_{SystemAction: dcctlv1.JournalEntry_FENCE_APPS},
		Initiator: &dcctlv1.Initiator{
			System:   true,
			Instance: &dcctlv1.Instance{Node: observer.Node, Datacenter: observer.Datacenter},
		},
		Datacenter: dc,
		Params:     params(t, res),
	}

	switch {
	case err == nil && hasWarnings(res):
		entry.Result = dcctlv1.JournalEntry_WARNING
		entry.Detail = fmt.Sprintf("managed jobs of %s stopped, some wait for their nodes: %s is active", dc, t.active)
	case err == nil:
		entry.Result = dcctlv1.JournalEntry_OK
		entry.Detail = fmt.Sprintf("managed jobs of %s stopped: %s is active", dc, t.active)
	case errors.Is(err, apps.ErrGuard):
		entry.Result = dcctlv1.JournalEntry_WARNING
		entry.Detail = fmt.Sprintf("stop of the managed jobs of %s ended: fencing is no longer allowed", dc)

		attrs = append(attrs, semconv.ErrorTypeOther)
	case ctx.Err() != nil:
		entry.Result = dcctlv1.JournalEntry_WARNING
		entry.Detail = fmt.Sprintf("stop of the managed jobs of %s interrupted by the shutdown", dc)

		attrs = append(attrs, semconv.ErrorTypeOther)
	case errors.Is(err, apps.ErrTimeout):
		entry.Result = dcctlv1.JournalEntry_ERROR
		entry.Detail = fmt.Sprintf("managed jobs of %s not stopped in time", dc)

		attrs = append(attrs, semconv.ErrorTypeKey.String(errorTypeTimeout))
	default:
		entry.Result = dcctlv1.JournalEntry_ERROR
		entry.Detail = fmt.Sprintf("managed jobs of %s not stopped", dc)

		attrs = append(attrs, semconv.ErrorTypeOther)
	}

	if err != nil {
		entry.Error = &dcctlv1.OperationError{Detail: err.Error()}
	}

	// A stop cut by the shutdown is still counted and journaled.
	ctx = context.WithoutCancel(ctx)

	f.stops.Add(ctx, 1, metric.WithAttributes(attrs...))

	if _, err := f.deps.Journal.Append(ctx, entry); err != nil {
		f.log.ErrorCtx(ctx, "fencing stop not journaled", logging.Str("datacenter", dc), logging.Err(err))
	}
}

func hasWarnings(res apps.Result) bool {
	return slices.ContainsFunc(res.Stages, func(s apps.Stage) bool { return len(s.Warnings()) > 0 })
}

// params are the effective state that the stop followed, the role of the Patroni
// leader of the datacenter, and every job of the stages that ran with the state it
// ended in.
func params(t trigger, res apps.Result) *structpb.Struct {
	var jobs []*structpb.Value

	for _, st := range res.Stages {
		for _, j := range st.Jobs {
			fields := map[string]*structpb.Value{
				"job":   structpb.NewStringValue(j.ID),
				"stage": structpb.NewNumberValue(float64(st.Number)),
				"state": structpb.NewStringValue(string(j.State)),
			}

			if len(j.Warnings) > 0 {
				warnings := make([]*structpb.Value, 0, len(j.Warnings))
				for _, w := range j.Warnings {
					warnings = append(warnings, structpb.NewStringValue(w))
				}

				fields["warnings"] = structpb.NewListValue(&structpb.ListValue{Values: warnings})
			}

			jobs = append(jobs, structpb.NewStructValue(&structpb.Struct{Fields: fields}))
		}
	}

	role := string(t.patroniRole)
	if role == "" {
		role = "unknown"
	}

	return &structpb.Struct{Fields: map[string]*structpb.Value{
		"active_datacenter": structpb.NewStringValue(t.active),
		"generation":        structpb.NewNumberValue(float64(t.generation)),
		"patroni_role":      structpb.NewStringValue(role),
		"jobs":              structpb.NewListValue(&structpb.ListValue{Values: jobs}),
	}}
}
