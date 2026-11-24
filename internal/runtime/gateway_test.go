package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func withDetails(t *testing.T, code codes.Code, msg string, details ...protoadapt.MessageV1) *status.Status {
	t.Helper()

	st, err := status.New(code, msg).WithDetails(details...)
	require.NoError(t, err)

	return st
}

func errorInfo(r apierr.Reason, md map[string]string) *errdetails.ErrorInfo {
	return &errdetails.ErrorInfo{Domain: apierr.Domain, Reason: string(r), Metadata: md}
}

// testErrorHandler is a Runtime with only what handleError uses.
func testErrorHandler() *Runtime {
	return &Runtime{log: testLogger()}
}

func TestErrorBodyReasons(t *testing.T) {
	for _, r := range apierr.Reasons() {
		t.Run(string(r), func(t *testing.T) {
			st := withDetails(t, r.Code(), "message", errorInfo(r, map[string]string{"operation": "operations/1"}))

			got := errorBody(st)

			code := runtime.HTTPStatusFromCode(r.Code())
			assert.Equal(t, r.ID(), got.GetId())
			assert.EqualValues(t, code, got.GetCode())
			assert.Equal(t, http.StatusText(code), got.GetStatus())
			assert.Equal(t, "message", got.GetDetail())
			assert.Equal(t, map[string]string{"operation": "operations/1"}, got.GetMetadata())
		})
	}
}

func TestErrorBodyDetails(t *testing.T) {
	preflight := &dcctlv1.Preflight{
		Checks: []*dcctlv1.Check{{Id: "coord.lock_free", Result: dcctlv1.Check_FAIL}},
		Mode:   dcctlv1.Operation_QUORUM,
	}

	tests := []struct {
		name string
		st   *status.Status
		want *dcctlv1.Error
	}{
		{
			name: "bad_request",
			st: withDetails(t, codes.InvalidArgument, "invalid request",
				errorInfo(apierr.ReasonRequestInvalid, nil),
				&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{
					{Field: "page_size", Description: "must be at most 500"},
				}},
			),
			want: &dcctlv1.Error{
				Id: "dcctl.request.invalid", Code: 400, Status: "Bad Request", Detail: "invalid request",
				Violations: []*dcctlv1.Violation{{Type: "field", Subject: "page_size", Description: "must be at most 500"}},
			},
		},
		{
			name: "preflight_failed",
			st: withDetails(t, codes.FailedPrecondition, "preflight failed",
				errorInfo(apierr.ReasonPreflightFailed, nil),
				&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
					{Type: "check", Subject: "coord.lock_free", Description: "dcctl/lock is held"},
					{Type: "confirmation", Subject: "isolation", Description: "confirm isolation"},
				}},
				protoadapt.MessageV1Of(preflight),
			),
			want: &dcctlv1.Error{
				Id: "dcctl.preflight.failed", Code: 400, Status: "Bad Request", Detail: "preflight failed",
				Violations: []*dcctlv1.Violation{
					{Type: "check", Subject: "coord.lock_free", Description: "dcctl/lock is held"},
					{Type: "confirmation", Subject: "isolation", Description: "confirm isolation"},
				},
				Preflight: preflight,
			},
		},
		{
			name: "foreign_domain",
			st: withDetails(t, codes.NotFound, "not found",
				&errdetails.ErrorInfo{Domain: "example.com", Reason: string(apierr.ReasonDatacenterNotFound), Metadata: map[string]string{"k": "v"}},
			),
			want: &dcctlv1.Error{Code: 404, Status: "Not Found", Detail: "not found"},
		},
		{
			name: "no_details",
			st:   status.New(codes.Unavailable, "down"),
			want: &dcctlv1.Error{Code: 503, Status: "Service Unavailable", Detail: "down"},
		},
		{
			name: "plain_error",
			st:   status.Convert(errors.New("boom")),
			want: &dcctlv1.Error{Code: 500, Status: "Internal Server Error", Detail: "boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errorBody(tt.st)
			assert.True(t, proto.Equal(tt.want, got), "errorBody() = %v; want %v", got, tt.want)
		})
	}
}

func TestHandleError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/status", nil)

	err := withDetails(t, codes.Unauthenticated, "authentication required",
		errorInfo(apierr.ReasonAuthenticationRequired, nil)).Err()

	testErrorHandler().handleError(t.Context(), nil, gatewayMarshaler(), w, r, err)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, w.Header().Values("Www-Authenticate"))
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.JSONEq(t,
		`{"id":"dcctl.auth.required","code":401,"status":"Unauthorized","detail":"authentication required"}`,
		w.Body.String())
}

func TestHandleErrorHTTPStatusError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/nope", nil)

	err := &runtime.HTTPStatusError{HTTPStatus: http.StatusMethodNotAllowed, Err: status.Error(codes.Unimplemented, "method not allowed")}

	testErrorHandler().handleError(t.Context(), nil, gatewayMarshaler(), w, r, err)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.JSONEq(t, `{"code":405,"status":"Method Not Allowed","detail":"method not allowed"}`, w.Body.String())
}

func TestHandleErrorInvalidHTTPStatus(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/status", nil)

	err := &runtime.HTTPStatusError{HTTPStatus: 0, Err: status.Error(codes.Internal, "boom")}

	testErrorHandler().handleError(t.Context(), nil, gatewayMarshaler(), w, r, err)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t, `{"code":500,"status":"Internal Server Error","detail":"boom"}`, w.Body.String())
}

func TestStreamErrorChunk(t *testing.T) {
	m := gatewayMarshaler()

	st := withDetails(t, codes.NotFound, "operation not found",
		errorInfo(apierr.ReasonOperationNotFound, map[string]string{"operation": "operations/1"}))

	got, err := m.Marshal(map[string]proto.Message{"error": st.Proto()})
	require.NoError(t, err)

	var line struct {
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(got, &line))
	assert.JSONEq(t,
		`{"id":"dcctl.operation.not_found","code":404,"status":"Not Found","detail":"operation not found","metadata":{"operation":"operations/1"}}`,
		string(line.Error))
}

func TestMarshalerUsesProtoNames(t *testing.T) {
	got, err := gatewayMarshaler().Marshal(map[string]any{"result": &dcctlv1.Status{DetectedActiveDatacenter: "datacenters/dc_a"}})
	require.NoError(t, err)

	assert.JSONEq(t, `{"result":{"detected_active_datacenter":"datacenters/dc_a"}}`, string(got))
}
