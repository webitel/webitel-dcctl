package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/auth"
)

func TestCheckAPIRequestAccepts(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
	}{
		{"post_json", http.MethodPost, "application/json"},
		{"post_json_charset", http.MethodPost, "application/json; charset=utf-8"},
		{"post_json_upper", http.MethodPost, "Application/JSON"},
		{"get_no_type", http.MethodGet, ""},
		{"get_text", http.MethodGet, "text/plain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tt.method, "/v1/operations/op1:resume", strings.NewReader("{}"))
			r.Header.Set("Content-Type", tt.contentType)

			assert.NoError(t, auth.CheckAPIRequest(r))
		})
	}
}

func TestCheckAPIRequestRejects(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
	}{
		{"post_text", http.MethodPost, "text/plain"},
		{"post_form", http.MethodPost, "application/x-www-form-urlencoded"},
		{"post_multipart", http.MethodPost, "multipart/form-data; boundary=x"},
		{"post_no_type", http.MethodPost, ""},
		{"post_malformed", http.MethodPost, "application/json; ="},
		{"put_text", http.MethodPut, "text/plain"},
		{"patch_text", http.MethodPatch, "text/plain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tt.method, "/v1/operations/op1:resume", strings.NewReader("{}"))
			r.Header.Set("Content-Type", tt.contentType)

			var se *runtime.HTTPStatusError

			require.ErrorAs(t, auth.CheckAPIRequest(r), &se)
			assert.Equal(t, http.StatusUnsupportedMediaType, se.HTTPStatus)
		})
	}
}
