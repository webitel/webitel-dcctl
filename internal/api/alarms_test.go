package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/status"
)

type fakeObservations struct {
	obs *status.Observed
}

func (f fakeObservations) Current() *status.Observed {
	return f.obs
}

func TestTrackerAlarms(t *testing.T) {
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	src := fakeObservations{obs: &status.Observed{Alarms: []status.Alarm{
		{
			ID: status.AlarmDBDegraded, Severity: status.SeverityWarning, Datacenter: "dc_b", StartTime: start,
			Detail: "Patroni of dc_b is paused", Facts: map[string]any{"paused": true},
		},
		{
			ID: status.AlarmVoterUnreachable, Severity: status.SeverityWarning, Datacenter: "dc_a", Voter: "dc_a",
			VoterType: status.VoterDatacenter, StartTime: start, Detail: "voter dc_a does not answer: timeout",
			Facts: map[string]any{"voter": "dc_a", "reason": "timeout"},
		},
		{
			ID: status.AlarmConflict, Severity: status.SeverityCritical, StartTime: start, Detail: "conflict",
			Facts: map[string]any{"generation": int64(4), "active_datacenters": []any{"dc_a", "dc_b"}},
		},
	}}}

	paused, err := structpb.NewStruct(map[string]any{"paused": true})
	require.NoError(t, err)

	voter, err := structpb.NewStruct(map[string]any{"voter": "dc_a", "reason": "timeout"})
	require.NoError(t, err)

	conflict, err := structpb.NewStruct(map[string]any{"generation": 4, "active_datacenters": []any{"dc_a", "dc_b"}})
	require.NoError(t, err)

	want := []*dcctlv1.Status_Alarm{
		{
			Type: dcctlv1.Status_Alarm_DB_DEGRADED, Severity: dcctlv1.Status_Alarm_WARNING, Datacenter: "datacenters/dc_b",
			StartTime: timestamppb.New(start), Detail: "Patroni of dc_b is paused", Facts: paused,
		},
		{
			Type: dcctlv1.Status_Alarm_VOTER_UNREACHABLE, Severity: dcctlv1.Status_Alarm_WARNING,
			Datacenter: "datacenters/dc_a", VoterType: dcctlv1.Status_Voter_DATACENTER,
			StartTime: timestamppb.New(start), Detail: "voter dc_a does not answer: timeout", Facts: voter,
		},
		{
			Type: dcctlv1.Status_Alarm_CONFLICT, Severity: dcctlv1.Status_Alarm_CRITICAL,
			StartTime: timestamppb.New(start), Detail: "conflict", Facts: conflict,
		},
	}

	got := TrackerAlarms(src, testLogger()).Alarms(t.Context())

	require.Len(t, got, len(want))

	for i := range want {
		assert.True(t, proto.Equal(want[i], got[i]), "alarm %d = %v; want %v", i, got[i], want[i])
	}
}

func TestTrackerAlarms_noCycle(t *testing.T) {
	assert.Nil(t, TrackerAlarms(fakeObservations{}, testLogger()).Alarms(t.Context()))
}

func TestTrackerAlarms_factsNotMapped(t *testing.T) {
	src := fakeObservations{obs: &status.Observed{Alarms: []status.Alarm{
		{ID: status.AlarmQuorumLost, Severity: status.SeverityWarning, Detail: "lost", Facts: map[string]any{"bad": struct{}{}}},
	}}}

	got := TrackerAlarms(src, testLogger()).Alarms(t.Context())

	require.Len(t, got, 1)
	assert.Equal(t, dcctlv1.Status_Alarm_QUORUM_LOST, got[0].GetType())
	assert.Nil(t, got[0].GetFacts(), "the alarm is reported without the facts it cannot hold")
}

// The status, its datacenters and its alarms come from one cycle of the tracker.
func TestGetStatus_fromTracker(t *testing.T) {
	st := exampleStatus()
	src := fakeObservations{obs: &status.Observed{Status: st, Alarms: []status.Alarm{
		{ID: status.AlarmQuorumLost, Severity: status.SeverityWarning, StartTime: st.ObserveTime, Detail: "lost"},
	}}}

	opts := testOptions()
	opts.Status = FromTracker(src)
	opts.Alarms = TrackerAlarms(src, testLogger())
	h := startAPI(t, opts)

	got, err := dcctlv1.NewStatusServiceClient(h.conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, st.ObserveTime, got.GetObserveTime().AsTime())
	assert.Len(t, got.GetDatacenters(), len(st.Datacenters))
	require.Len(t, got.GetAlarms(), 1)
	assert.Equal(t, dcctlv1.Status_Alarm_QUORUM_LOST, got.GetAlarms()[0].GetType())

	dc, err := dcctlv1.NewDatacenterServiceClient(h.conn).GetDatacenter(callCtx(t),
		&dcctlv1.GetDatacenterRequest{Name: "datacenters/" + st.Datacenters[0].Name})
	require.NoError(t, err)
	assert.Equal(t, "datacenters/"+st.Datacenters[0].Name, dc.GetName())
}

func TestGetStatus_fromTrackerBeforeFirstCycle(t *testing.T) {
	opts := testOptions()
	opts.Status = FromTracker(fakeObservations{})
	h := startAPI(t, opts)

	_, err := dcctlv1.NewStatusServiceClient(h.conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	assertCode(t, codes.Unavailable, err)
}
