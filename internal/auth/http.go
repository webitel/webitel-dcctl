package auth

import (
	"mime"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CheckAPIRequest applies the HTTP rules of the gateway that the gRPC
// interceptors cannot see: a request with a body must be application/json,
// so that a cross-site form or text/plain request cannot reach an action.
// It returns a *runtime.HTTPStatusError with 415 otherwise.
//
// The gateway reads credentials only from the Authorization header and sets
// no CORS headers, so neither needs a check here.
func CheckAPIRequest(r *http.Request) error {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return nil
	}

	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mt == "application/json" {
		return nil
	}

	return &runtime.HTTPStatusError{
		HTTPStatus: http.StatusUnsupportedMediaType,
		Err:        status.Error(codes.InvalidArgument, "content type must be application/json"),
	}
}
