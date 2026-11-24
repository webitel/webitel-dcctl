package runtime

import (
	"net/http"
	"strings"
)

// httpHandler routes gRPC to the gRPC server, /api/ to the gateway and the
// rest to the UI.
func (r *Runtime) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", r.guardAPI(http.StripPrefix("/api", r.gateway)))

	if r.ui != nil {
		mux.Handle("/", r.ui)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.ProtoMajor == 2 && strings.HasPrefix(req.Header.Get("Content-Type"), "application/grpc") {
			r.httpGRPC.ServeHTTP(w, req)

			return
		}

		mux.ServeHTTP(w, req)
	})
}

// guardAPI runs Config.APIGuard before next.
func (r *Runtime) guardAPI(next http.Handler) http.Handler {
	guard := r.cfg.APIGuard
	if guard == nil {
		return next
	}

	m := gatewayMarshaler()

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := guard(req); err != nil {
			r.handleError(req.Context(), r.gateway, m, w, req, err)

			return
		}

		next.ServeHTTP(w, req)
	})
}
