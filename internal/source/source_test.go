package source_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/source"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  func(t *testing.T) error
		want source.Reason
	}{
		{name: "deadline", err: func(*testing.T) error { return context.DeadlineExceeded }, want: source.ReasonTimeout},
		{name: "http_client_timeout", err: httpClientTimeout, want: source.ReasonTimeout},
		{name: "refused", err: dialRefused, want: source.ReasonRefused},
		{name: "dns", err: func(*testing.T) error {
			return &net.DNSError{Err: "no such host", Name: "consul.invalid", IsNotFound: true}
		}, want: source.ReasonDNS},
		{name: "dns_timeout", err: func(*testing.T) error {
			return &net.DNSError{Err: "i/o timeout", Name: "consul.invalid", IsTimeout: true}
		}, want: source.ReasonDNS},
		{name: "unknown_authority", err: untrustedTLS, want: source.ReasonTLS},
		{name: "status_401", err: func(*testing.T) error { return &source.StatusError{Code: http.StatusUnauthorized} }, want: source.ReasonAuth},
		{name: "status_403", err: func(*testing.T) error { return &source.StatusError{Code: http.StatusForbidden} }, want: source.ReasonAuth},
		{name: "status_500", err: func(*testing.T) error { return &source.StatusError{Code: http.StatusInternalServerError} }, want: source.ReasonHTTP},
		{name: "invalid_response", err: func(*testing.T) error {
			return fmt.Errorf("decode members: %w", source.ErrInvalidResponse)
		}, want: source.ReasonInvalid},
		{name: "json_syntax", err: func(*testing.T) error { return json.Unmarshal([]byte("{"), new(any)) }, want: source.ReasonInvalid},
		{name: "json_type", err: func(*testing.T) error { return json.Unmarshal([]byte(`"x"`), new(int)) }, want: source.ReasonInvalid},
		{name: "other", err: func(*testing.T) error { return errors.New("connection reset by peer") }, want: source.ReasonHTTP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := source.Classify("10.0.0.1:8500", fmt.Errorf("get kv: %w", tt.err(t)))

			got, ok := source.ReasonOf(err)
			require.True(t, ok, "classified error: %v", err)
			assert.Equal(t, tt.want, got)

			var se *source.Error
			require.ErrorAs(t, err, &se)
			assert.Equal(t, "10.0.0.1:8500", se.Endpoint)
		})
	}
}

func TestClassify_passThrough(t *testing.T) {
	assert.NoError(t, source.Classify("10.0.0.1:8500", nil))

	canceled := fmt.Errorf("get kv: %w", context.Canceled)
	got := source.Classify("10.0.0.1:8500", canceled)
	assert.Same(t, canceled, got)
	_, ok := source.ReasonOf(got)
	assert.False(t, ok)

	classified := source.Classify("10.0.0.1:8500", &source.StatusError{Code: http.StatusUnauthorized})
	wrapped := fmt.Errorf("renew session: %w", classified)
	assert.Same(t, wrapped, source.Classify("10.0.0.2:8500", wrapped))
}

func TestError_message(t *testing.T) {
	err := source.Classify("10.0.0.1:8500", &source.StatusError{Code: http.StatusBadGateway})
	assert.EqualError(t, err, "10.0.0.1:8500: http: unexpected HTTP status 502")
}

func dialRefused(t *testing.T) error {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	_, err = net.DialTimeout("tcp", addr, time.Second)
	require.Error(t, err)

	return err
}

func httpClientTimeout(t *testing.T) error {
	t.Helper()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	client := &http.Client{Timeout: 50 * time.Millisecond}

	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)

	return err
}

func untrustedTLS(t *testing.T) error {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = nil
	t.Cleanup(srv.Close)

	// The default client does not trust the test server's self-signed certificate.
	resp, err := http.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)

	return err
}
