package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func TestListDatacenters(t *testing.T) {
	h := startAPI(t, testOptions())

	example := swaggerExample(t, "/v1/datacenters", "get", "200")

	got, err := dcctlv1.NewDatacenterServiceClient(h.conn).ListDatacenters(callCtx(t),
		&dcctlv1.ListDatacentersRequest{PageSize: 1, PageToken: "ignored"})
	require.NoError(t, err)
	assert.Empty(t, got.GetNextPageToken())
	assertExample(t, example, gatewayJSON(t, got), nil)

	resp := h.get(t, "/api/v1/datacenters")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assertExample(t, example, resp.body, nil)
}

func TestGetDatacenter(t *testing.T) {
	st := exampleStatus()
	st.Datacenters[1].Nomad.JobCounts = status.JobCounts{Stopped: 2}

	opts := testOptions()
	opts.Status = fakeStatusSource{st: st, ok: true}
	h := startAPI(t, opts)

	example := swaggerExample(t, "/v1/datacenters/{datacenter}", "get", "200")

	got, err := dcctlv1.NewDatacenterServiceClient(h.conn).GetDatacenter(callCtx(t),
		&dcctlv1.GetDatacenterRequest{Name: "datacenters/dc_b"})
	require.NoError(t, err)
	assertExample(t, example, gatewayJSON(t, got), nil)

	resp := h.get(t, "/api/v1/datacenters/dc_b")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assertExample(t, example, resp.body, nil)
}

func TestGetDatacenterNotFound(t *testing.T) {
	h := startAPI(t, testOptions())

	_, err := dcctlv1.NewDatacenterServiceClient(h.conn).GetDatacenter(callCtx(t),
		&dcctlv1.GetDatacenterRequest{Name: "datacenters/dc_c"})
	st := assertCode(t, codes.NotFound, err)
	assert.Equal(t, apierr.ReasonDatacenterNotFound, reasonOf(st))

	resp := h.get(t, "/api/v1/datacenters/dc_c")
	assert.Equal(t, http.StatusNotFound, resp.code)
	assert.JSONEq(t, swaggerExample(t, "/v1/datacenters/{datacenter}", "get", "404"), resp.body)
}

func TestGetDatacenterInvalidName(t *testing.T) {
	h := startAPI(t, testOptions())

	resp := h.get(t, "/api/v1/datacenters/dc.c")
	assert.Equal(t, http.StatusBadRequest, resp.code)
	assert.JSONEq(t, swaggerExample(t, "/v1/datacenters/{datacenter}", "get", "400"), resp.body)
}

func TestDatacenterReadsBeforeFirstCycle(t *testing.T) {
	opts := testOptions()
	opts.Status = fakeStatusSource{}
	h := startAPI(t, opts)

	client := dcctlv1.NewDatacenterServiceClient(h.conn)

	_, err := client.ListDatacenters(callCtx(t), &dcctlv1.ListDatacentersRequest{})
	assertCode(t, codes.Unavailable, err)

	_, err = client.GetDatacenter(callCtx(t), &dcctlv1.GetDatacenterRequest{Name: "datacenters/dc_a"})
	assertCode(t, codes.Unavailable, err)

	resp := h.get(t, "/api/v1/datacenters")
	assert.Equal(t, http.StatusServiceUnavailable, resp.code)
	assert.JSONEq(t, swaggerExample(t, "/v1/datacenters", "get", "503"), resp.body)
}

func TestDatacenterActionsUnimplemented(t *testing.T) {
	h := startAPI(t, testOptions())

	_, err := dcctlv1.NewDatacenterServiceClient(h.conn).SwitchDatacenter(callCtx(t),
		&dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b", RequestId: "6f1c2b9e-8d4a-4f3b-9c2e-1a7d5e3f9b40"})
	assertCode(t, codes.Unimplemented, err)
}
