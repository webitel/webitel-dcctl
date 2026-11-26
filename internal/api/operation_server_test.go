package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// exampleOperations returns the operations of the ListOperations example,
// newest first.
func exampleOperations(t *testing.T) []*dcctlv1.Operation {
	t.Helper()

	resp := new(dcctlv1.ListOperationsResponse)
	require.NoError(t, protojson.Unmarshal([]byte(swaggerExample(t, "/v1/operations", "get", "200")), resp))
	require.Len(t, resp.GetOperations(), 2)

	return resp.GetOperations()
}

func present(name string, op *dcctlv1.Operation) coord.OpCopy {
	return coord.OpCopy{Name: name, Status: coord.StatusPresent, Operation: op}
}

func withOperations(view coord.OpView) Options {
	opts := testOptions()
	opts.Operations = fakeOperations{view: view}

	return opts
}

func TestListOperations(t *testing.T) {
	ops := exampleOperations(t)
	// The older operation finished in dc_a; the newer one runs with dc_a
	// unreachable, so only dc_b holds it.
	h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{
		present("dc_a", ops[1]),
		present("dc_b", ops[0]),
		{Name: "dc_c", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
	}}))

	example := swaggerExample(t, "/v1/operations", "get", "200")

	got, err := dcctlv1.NewOperationServiceClient(h.conn).ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{})
	require.NoError(t, err)
	assert.JSONEq(t, example, gatewayJSON(t, got))

	resp := h.get(t, "/api/v1/operations")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.JSONEq(t, example, resp.body)
}

func TestListOperationsOneCopyPerOperation(t *testing.T) {
	op := exampleOperations(t)[0]
	h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{
		present("dc_a", op), present("dc_b", op), {Name: "dc_c", Status: coord.StatusInvalid},
	}}))

	got, err := dcctlv1.NewOperationServiceClient(h.conn).ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{})
	require.NoError(t, err)
	require.Len(t, got.GetOperations(), 1)
	assert.Equal(t, op.GetName(), got.GetOperations()[0].GetName())
}

func pagedOperation(i int) *dcctlv1.Operation {
	return &dcctlv1.Operation{
		Name:       fmt.Sprintf("operations/op-%d", i),
		State:      dcctlv1.Operation_SUCCEEDED,
		CreateTime: timestamppb.New(observeTime.Add(time.Duration(i) * time.Minute)),
	}
}

func TestListOperationsPaging(t *testing.T) {
	h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{
		present("dc_a", pagedOperation(1)), present("dc_b", pagedOperation(3)), present("dc_c", pagedOperation(2)),
	}}))
	client := dcctlv1.NewOperationServiceClient(h.conn)

	first, err := client.ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{PageSize: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"operations/op-3", "operations/op-2"}, operationNames(first))
	require.NotEmpty(t, first.GetNextPageToken())

	last, err := client.ListOperations(callCtx(t),
		&dcctlv1.ListOperationsRequest{PageSize: 2, PageToken: first.GetNextPageToken()})
	require.NoError(t, err)
	assert.Equal(t, []string{"operations/op-1"}, operationNames(last))
	assert.Empty(t, last.GetNextPageToken())

	exact, err := client.ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{PageSize: 3})
	require.NoError(t, err)
	assert.Len(t, exact.GetOperations(), 3)
	assert.Empty(t, exact.GetNextPageToken(), "no token when nothing follows")

	resp := h.get(t, "/api/v1/operations?page_size=2&page_token="+first.GetNextPageToken())
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.Contains(t, resp.body, "operations/op-1")
}

func operationNames(resp *dcctlv1.ListOperationsResponse) []string {
	out := make([]string, 0, len(resp.GetOperations()))
	for _, op := range resp.GetOperations() {
		out = append(out, op.GetName())
	}

	return out
}

func TestListOperationsInvalidPage(t *testing.T) {
	h := startAPI(t, withOperations(coord.OpView{}))
	client := dcctlv1.NewOperationServiceClient(h.conn)

	for _, token := range []string{"%%%", "bm90LWEtdG9rZW4", "eHxvcGVyYXRpb25zL3g"} {
		_, err := client.ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{PageToken: token})
		st := assertCode(t, codes.InvalidArgument, err)
		assert.Equal(t, apierr.ReasonRequestInvalid, reasonOf(st), "token %q", token)
		assert.Equal(t, "page_token", badField(st), "token %q", token)
	}

	_, err := client.ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{PageSize: 501})
	assertCode(t, codes.InvalidArgument, err)
}

func badField(st *status.Status) string {
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok && len(br.GetFieldViolations()) > 0 {
			return br.GetFieldViolations()[0].GetField()
		}
	}

	return ""
}

func TestGetOperation(t *testing.T) {
	op := exampleOperations(t)[0]
	h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{present("dc_b", op)}}))

	example := swaggerExample(t, "/v1/operations/{operation}", "get", "200")

	got, err := dcctlv1.NewOperationServiceClient(h.conn).GetOperation(callCtx(t),
		&dcctlv1.GetOperationRequest{Name: op.GetName()})
	require.NoError(t, err)
	assert.JSONEq(t, example, gatewayJSON(t, got))

	resp := h.get(t, "/api/v1/"+op.GetName())
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.JSONEq(t, example, resp.body)
}

func TestGetOperationCopy(t *testing.T) {
	stale := exampleOperations(t)[0]

	fresh, ok := proto.Clone(stale).(*dcctlv1.Operation)
	require.True(t, ok)

	fresh.UpdateTime = timestamppb.New(stale.GetUpdateTime().AsTime().Add(time.Second))
	fresh.Steps[1].Detail = "stopping 2 of 3 jobs"

	finished, ok := proto.Clone(stale).(*dcctlv1.Operation)
	require.True(t, ok)

	finished.State = dcctlv1.Operation_SUCCEEDED
	finished.Steps[1].Detail = "done"

	tests := []struct {
		name   string
		copies []coord.OpCopy
		want   string
	}{
		{
			// The leader of the operation is in dc_b: its copy is the
			// primary one even when another copy is newer.
			name:   "leader_copy",
			copies: []coord.OpCopy{present("dc_a", fresh), present("dc_b", stale)},
			want:   "stopping 1 of 3 jobs",
		},
		{
			name: "latest_without_leader_copy",
			copies: []coord.OpCopy{
				present("dc_a", fresh), {Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
			},
			want: "stopping 2 of 3 jobs",
		},
		{
			// A finished copy wins over the leader's unfinished one, as in
			// the idle check of a new operation.
			name:   "finished_copy",
			copies: []coord.OpCopy{present("dc_a", finished), present("dc_b", stale)},
			want:   "done",
		},
		{
			name:   "other_operation_in_leader_kv",
			copies: []coord.OpCopy{present("dc_a", fresh), present("dc_b", pagedOperation(9))},
			want:   "stopping 2 of 3 jobs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewOperationServer(fakeOperations{view: coord.OpView{Copies: tt.copies}}, testLogger())

			got, err := s.GetOperation(t.Context(), &dcctlv1.GetOperationRequest{Name: stale.GetName()})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.GetSteps()[1].GetDetail())
		})
	}
}

func TestGetOperationNotFound(t *testing.T) {
	h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{present("dc_a", pagedOperation(1))}}))

	name := "operations/0b9f3c2e-5d1a-4c55-9a7e-2f4d8c1b6a10"

	_, err := dcctlv1.NewOperationServiceClient(h.conn).GetOperation(callCtx(t), &dcctlv1.GetOperationRequest{Name: name})
	st := assertCode(t, codes.NotFound, err)
	assert.Equal(t, apierr.ReasonOperationNotFound, reasonOf(st))

	resp := h.get(t, "/api/v1/"+name)
	assert.Equal(t, http.StatusNotFound, resp.code)
	assert.JSONEq(t, swaggerExample(t, "/v1/operations/{operation}", "get", "404"), resp.body)
}

func TestGetOperationUnavailable(t *testing.T) {
	name := "operations/0b9f3c2e-5d1a-4c55-9a7e-2f4d8c1b6a10"

	for _, missing := range []coord.OpCopy{
		{Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
		{Name: "dc_b", Status: coord.StatusInvalid, Reason: source.ReasonInvalid},
	} {
		t.Run(string(missing.Status), func(t *testing.T) {
			h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{
				present("dc_a", pagedOperation(1)), missing,
			}}))

			_, err := dcctlv1.NewOperationServiceClient(h.conn).GetOperation(callCtx(t),
				&dcctlv1.GetOperationRequest{Name: name})
			st := assertCode(t, codes.Unavailable, err)
			assert.Equal(t, "the operation may be in a copy of dcctl/op that is not readable", st.Message())

			resp := h.get(t, "/api/v1/"+name)
			assert.Equal(t, http.StatusServiceUnavailable, resp.code)
			assert.JSONEq(t, swaggerExample(t, "/v1/operations/{operation}", "get", "503"), resp.body)
		})
	}
}

func TestListOperationsUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		copies []coord.OpCopy
	}{
		{name: "no_copy", copies: nil},
		{name: "unreadable", copies: []coord.OpCopy{
			{Name: "dc_a", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
			{Name: "dc_b", Status: coord.StatusInvalid, Reason: source.ReasonInvalid},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewOperationServer(fakeOperations{view: coord.OpView{Copies: tt.copies}}, testLogger())

			_, err := s.ListOperations(t.Context(), &dcctlv1.ListOperationsRequest{})
			st := assertCode(t, codes.Unavailable, err)
			assert.Equal(t, apierr.ReasonKVUnavailable, reasonOf(st))
			assert.Equal(t, "no copy of dcctl/op is readable", st.Message())
		})
	}
}

func TestListOperationsUnavailableBody(t *testing.T) {
	h := startAPI(t, withOperations(coord.OpView{Copies: []coord.OpCopy{
		{Name: "dc_a", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
		{Name: "dc_b", Status: coord.StatusInvalid, Reason: source.ReasonInvalid},
	}}))

	resp := h.get(t, "/api/v1/operations")
	assert.Equal(t, http.StatusServiceUnavailable, resp.code)
	assert.JSONEq(t, swaggerExample(t, "/v1/operations", "get", "503"), resp.body)
}

func TestListOperationsSomeUnreachable(t *testing.T) {
	s := NewOperationServer(fakeOperations{view: coord.OpView{Copies: []coord.OpCopy{
		{Name: "dc_a", Status: coord.StatusAbsent},
		{Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
	}}}, testLogger())

	got, err := s.ListOperations(t.Context(), &dcctlv1.ListOperationsRequest{})
	require.NoError(t, err)
	assert.Empty(t, got.GetOperations())
}

func TestOperationReadError(t *testing.T) {
	opts := testOptions()
	opts.Operations = fakeOperations{err: errors.New("get dcctl/op from 10.1.0.89:8500: refused")}
	h := startAPI(t, opts)
	client := dcctlv1.NewOperationServiceClient(h.conn)

	_, err := client.ListOperations(callCtx(t), &dcctlv1.ListOperationsRequest{})
	st := assertCode(t, codes.Internal, err)
	assert.NotContains(t, st.Message(), "10.1.0.89")

	_, err = client.GetOperation(callCtx(t), &dcctlv1.GetOperationRequest{Name: "operations/x"})
	st = assertCode(t, codes.Internal, err)
	assert.NotContains(t, st.Message(), "10.1.0.89")
	assert.True(t, h.logged("10.1.0.89:8500"))
}

func TestOperationActionsUnimplemented(t *testing.T) {
	h := startAPI(t, testOptions())

	_, err := dcctlv1.NewOperationServiceClient(h.conn).AbandonOperation(callCtx(t),
		&dcctlv1.AbandonOperationRequest{Name: "operations/x"})
	assertCode(t, codes.Unimplemented, err)
}
