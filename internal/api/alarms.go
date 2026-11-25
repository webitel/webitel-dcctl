package api

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// TrackerAlarms returns the alarms raised on the last cycle of src.
func TrackerAlarms(src Observations, log *logging.Logger) Alarms {
	return trackerAlarms{src: src, log: log}
}

type trackerAlarms struct {
	src Observations
	log *logging.Logger
}

func (t trackerAlarms) Alarms(ctx context.Context) []*dcctlv1.Status_Alarm {
	obs := t.src.Current()
	if obs == nil {
		return nil
	}

	out := make([]*dcctlv1.Status_Alarm, 0, len(obs.Alarms))

	for _, a := range obs.Alarms {
		pa := &dcctlv1.Status_Alarm{
			Type:       a.ID.Type(),
			Severity:   a.Severity.Proto(),
			Datacenter: datacenterName(a.Datacenter),
			StartTime:  timestamp(a.StartTime),
			Detail:     a.Detail,
			VoterType:  voterTypeOf(a.VoterType),
		}

		if len(a.Facts) > 0 {
			facts, err := structpb.NewStruct(a.Facts)
			if err != nil {
				t.log.WarnCtx(ctx, "alarm facts not shown", logging.Str("alarm", string(a.ID)), logging.Err(err))
			} else {
				pa.Facts = facts
			}
		}

		out = append(out, pa)
	}

	return out
}
