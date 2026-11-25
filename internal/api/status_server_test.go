package api

import (
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func exampleAlarm(t *testing.T) *dcctlv1.Status_Alarm {
	t.Helper()

	facts, err := structpb.NewStruct(map[string]any{"address": "10.9.0.10:10024", "error": "timeout"})
	require.NoError(t, err)

	return &dcctlv1.Status_Alarm{
		Type: dcctlv1.Status_Alarm_VOTER_UNREACHABLE, Severity: dcctlv1.Status_Alarm_WARNING,
		VoterType: dcctlv1.Status_Voter_ARBITER,
		StartTime: timestamppb.New(time.Date(2026, 11, 23, 10, 12, 40, 0, time.UTC)),
		Detail:    "arbiter does not answer", Facts: facts,
	}
}

func TestGetStatus(t *testing.T) {
	opts := testOptions()
	opts.Alarms = fakeAlarms{exampleAlarm(t)}
	h := startAPI(t, opts)

	example := swaggerExample(t, "/v1/status", "get", "200")
	skip := statusExampleSkips()
	delete(skip, "alarms")

	got, err := dcctlv1.NewStatusServiceClient(h.conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	require.NoError(t, err)
	assertExample(t, example, gatewayJSON(t, got), skip)

	resp := h.get(t, "/api/v1/status")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assertExample(t, example, resp.body, skip)
}

func TestGetStatusWithoutAlarms(t *testing.T) {
	h := startAPI(t, testOptions())

	got, err := dcctlv1.NewStatusServiceClient(h.conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	require.NoError(t, err)
	assert.Empty(t, got.GetAlarms())
}

func TestGetStatusBeforeFirstCycle(t *testing.T) {
	opts := testOptions()
	opts.Status = fakeStatusSource{}
	h := startAPI(t, opts)

	_, err := dcctlv1.NewStatusServiceClient(h.conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	assertCode(t, codes.Unavailable, err)

	resp := h.get(t, "/api/v1/status")
	assert.Equal(t, http.StatusServiceUnavailable, resp.code)
}

func TestGetStatusOverflow(t *testing.T) {
	st := exampleStatus()
	st.Quorum.TotalCount = math.MaxInt32 + 1

	opts := testOptions()
	opts.Status = fakeStatusSource{st: st, ok: true}
	h := startAPI(t, opts)

	_, err := dcctlv1.NewStatusServiceClient(h.conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
	got := assertCode(t, codes.Internal, err)
	assert.NotContains(t, got.Message(), "int32")
	assert.True(t, h.logged("quorum.total_count 2147483648 does not fit int32"))
}

func TestVerifyStatus(t *testing.T) {
	example := swaggerExample(t, "/v1/status:verify", "get", "200")

	want := new(dcctlv1.VerifyStatusResponse)
	require.NoError(t, protojson.Unmarshal([]byte(example), want))

	opts := testOptions()
	opts.Verifier = fakeVerifier{checks: want.GetChecks()}
	h := startAPI(t, opts)

	got, err := dcctlv1.NewStatusServiceClient(h.conn).VerifyStatus(callCtx(t), &dcctlv1.VerifyStatusRequest{})
	require.NoError(t, err)
	assertExample(t, example, gatewayJSON(t, got), nil)

	resp := h.get(t, "/api/v1/status:verify")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assertExample(t, example, resp.body, nil)
}

func TestVerifyStatusNotWired(t *testing.T) {
	h := startAPI(t, testOptions())

	_, err := dcctlv1.NewStatusServiceClient(h.conn).VerifyStatus(callCtx(t), &dcctlv1.VerifyStatusRequest{})
	got := assertCode(t, codes.Unimplemented, err)
	assert.Equal(t, "verify checks are not available yet", got.Message())
}

func TestVerifyStatusError(t *testing.T) {
	opts := testOptions()
	opts.Verifier = fakeVerifier{err: errors.New("dial tcp 10.1.0.89:8500: connection refused")}
	h := startAPI(t, opts)

	_, err := dcctlv1.NewStatusServiceClient(h.conn).VerifyStatus(callCtx(t), &dcctlv1.VerifyStatusRequest{})
	got := assertCode(t, codes.Internal, err)
	assert.NotContains(t, got.Message(), "10.1.0.89")

	resp := h.get(t, "/api/v1/status:verify")
	assert.Equal(t, http.StatusInternalServerError, resp.code)
	assert.NotContains(t, resp.body, "10.1.0.89")
	assert.True(t, h.logged("dial tcp 10.1.0.89:8500: connection refused"))
}

type fakeSnapshots struct{ snap *collect.Snapshot }

func (f fakeSnapshots) Snapshot() *collect.Snapshot { return f.snap }

func TestFromCollector(t *testing.T) {
	cfg := status.Config{Interval: 5 * time.Second}

	_, ok := FromCollector(fakeSnapshots{}, cfg).Status(observeTime)
	assert.False(t, ok, "no status before the first snapshot")

	snap := &collect.Snapshot{
		ObserveTime: observeTime,
		Observer:    collect.Instance{Datacenter: "dc_a", Node: "dcctl-a1"},
		Profile:     config.ProfileWarmStandby,
		Datacenters: []collect.Datacenter{{Name: "dc_a"}},
	}

	got, ok := FromCollector(fakeSnapshots{snap: snap}, cfg).Status(observeTime)
	require.True(t, ok)
	assert.Equal(t, snap.Observer, got.Observer)
	assert.Equal(t, 5*time.Second, got.Interval)
	require.Len(t, got.Datacenters, 1)
	assert.Equal(t, status.StateUnreachable, got.Datacenters[0].State)
}
