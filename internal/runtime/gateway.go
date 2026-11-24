package runtime

import (
	"context"
	"errors"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// violationField is the Violation.type of a BadRequest field violation.
const violationField = "field"

func newGatewayMux(handleError runtime.ErrorHandlerFunc) *runtime.ServeMux {
	return runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, gatewayMarshaler()),
		runtime.WithErrorHandler(handleError),
	)
}

func gatewayMarshaler() runtime.Marshaler {
	return streamErrorMarshaler{&runtime.HTTPBodyMarshaler{
		Marshaler: &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false},
		},
	}}
}

// streamErrorMarshaler writes the error line of a server stream in the
// gateway error format. grpc-gateway builds that line itself as
// {"error": google.rpc.Status}, and the marshaler is the only hook that sees it.
type streamErrorMarshaler struct {
	runtime.Marshaler
}

func (m streamErrorMarshaler) Marshal(v any) ([]byte, error) {
	if chunk, ok := v.(map[string]proto.Message); ok && len(chunk) == 1 {
		if st, ok := chunk["error"].(*spb.Status); ok {
			v = map[string]proto.Message{"error": errorBody(status.FromProto(st))}
		}
	}

	return m.Marshaler.Marshal(v)
}

// handleError writes err in the gateway error format. Unlike the grpc-gateway
// default it never sets WWW-Authenticate: browsers would show their own login
// dialog even for fetch.
func (r *Runtime) handleError(ctx context.Context, _ *runtime.ServeMux, m runtime.Marshaler, w http.ResponseWriter, _ *http.Request, err error) {
	var custom *runtime.HTTPStatusError
	if errors.As(err, &custom) {
		err = custom.Err
	}

	body := errorBody(status.Convert(err))
	if custom != nil {
		setHTTPStatus(body, custom.HTTPStatus)
	}

	buf, merr := m.Marshal(body)
	if merr != nil {
		r.log.ErrorCtx(ctx, "marshal gateway error", logging.Err(merr))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	w.Header().Del("Trailer")
	w.Header().Del("Transfer-Encoding")
	w.Header().Set("Content-Type", m.ContentType(body))
	w.WriteHeader(int(body.GetCode()))

	if _, err := w.Write(buf); err != nil {
		r.log.WarnCtx(ctx, "write gateway error", logging.Err(err))
	}
}

// errorBody converts st and its details to the gateway error format.
func errorBody(st *status.Status) *dcctlv1.Error {
	body := &dcctlv1.Error{Detail: st.Message()}
	setHTTPStatus(body, runtime.HTTPStatusFromCode(st.Code()))

	for _, d := range st.Details() {
		switch d := d.(type) {
		case *errdetails.ErrorInfo:
			if d.GetDomain() == apierr.Domain {
				body.Id = apierr.Reason(d.GetReason()).ID()
				body.Metadata = d.GetMetadata()
			}
		case *errdetails.BadRequest:
			for _, v := range d.GetFieldViolations() {
				body.Violations = append(body.Violations, &dcctlv1.Violation{
					Type:        violationField,
					Subject:     v.GetField(),
					Description: v.GetDescription(),
				})
			}
		case *errdetails.PreconditionFailure:
			for _, v := range d.GetViolations() {
				body.Violations = append(body.Violations, &dcctlv1.Violation{
					Type:        v.GetType(),
					Subject:     v.GetSubject(),
					Description: v.GetDescription(),
				})
			}
		case *dcctlv1.Preflight:
			body.Preflight = d
		}
	}

	return body
}

// setHTTPStatus sets code, or 500 for a value that is not an HTTP status.
func setHTTPStatus(body *dcctlv1.Error, code int) {
	body.Code = http.StatusInternalServerError
	if code >= 100 && code <= 599 {
		body.Code = int32(code)
	}

	body.Status = http.StatusText(int(body.GetCode()))
}
