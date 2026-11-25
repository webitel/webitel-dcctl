package runtime

import "net/http"

// WriteError writes err as the gateway error handler does.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	testErrorHandler().handleError(r.Context(), nil, gatewayMarshaler(), w, r, err)
}
