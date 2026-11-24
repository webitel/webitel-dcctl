package patroni_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func TestClient_mutations(t *testing.T) {
	p := newPKI(t)
	rec := &recorder{}
	addr := p.serve(t, rec.handler(http.StatusOK, `{"ttl":30}`))
	c := newClient(t, p, patroni.Options{Username: "patroni", Password: testPassword})
	ctx := t.Context()

	cfg, err := c.PatchConfig(ctx, addr, map[string]any{"standby_cluster": nil})
	require.NoError(t, err)
	assert.EqualValues(t, 30, cfg["ttl"])

	msg, err := c.Switchover(ctx, addr, "pg-1", "pg-2")
	require.NoError(t, err)
	assert.Equal(t, `{"ttl":30}`, msg, "the answer of Patroni is returned")

	_, err = c.Switchover(ctx, addr, "pg-1", "")
	require.NoError(t, err)
	_, err = c.Failover(ctx, addr, "pg-2")
	require.NoError(t, err)

	require.Len(t, rec.reqs, 4)

	want := []struct{ method, path, body string }{
		{http.MethodPatch, "/config", `{"standby_cluster":null}`},
		{http.MethodPost, "/switchover", `{"leader":"pg-1","candidate":"pg-2"}`},
		{http.MethodPost, "/switchover", `{"leader":"pg-1"}`},
		{http.MethodPost, "/failover", `{"candidate":"pg-2"}`},
	}

	for i, w := range want {
		got := rec.reqs[i]
		assert.Equal(t, w.method, got.method)
		assert.Equal(t, w.path, got.path)
		assert.JSONEq(t, w.body, got.body)
		assert.Equal(t, "application/json", got.contentType)
		assert.True(t, got.hasAuth, "%s %s must be authenticated", w.method, w.path)
		assert.Equal(t, "patroni", got.user)
		assert.Equal(t, testPassword, got.pass)
	}
}

func TestClient_mutations_requireArguments(t *testing.T) {
	p := newPKI(t)
	c := newClient(t, p, patroni.Options{})

	_, err := c.Switchover(t.Context(), "127.0.0.1:1", "", "pg-2")
	require.Error(t, err)

	_, err = c.Failover(t.Context(), "127.0.0.1:1", "")
	require.Error(t, err)
}

func TestClient_readsAreAnonymous(t *testing.T) {
	p := newPKI(t)
	rec := &recorder{}
	addr := p.serve(t, rec.handler(http.StatusOK, `{"members":[],"patroni":{}}`))
	c := newClient(t, p, patroni.Options{Username: "patroni", Password: testPassword})
	ctx := t.Context()

	_, _ = c.State(ctx, addr)
	_, _ = c.Cluster(ctx, addr)
	_, _ = c.Primary(ctx, addr)
	_, _ = c.Config(ctx, addr)
	_, _ = c.History(ctx, addr)

	require.Len(t, rec.reqs, 5)

	for _, got := range rec.reqs {
		assert.Equal(t, http.MethodGet, got.method)
		assert.False(t, got.hasAuth, "GET %s must not carry credentials", got.path)
	}
}

func TestClient_redirectIsNotFollowed(t *testing.T) {
	p := newPKI(t)
	rec := &recorder{}
	target := p.serve(t, rec.handler(http.StatusOK, `{}`))
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+target+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	c := newClient(t, p, patroni.Options{Username: "patroni", Password: testPassword})

	_, err := c.Failover(t.Context(), addr, "pg-2")
	require.Error(t, err)
	assertReason(t, err, source.ReasonHTTP)
	assert.Empty(t, rec.reqs, "credentials must not follow a redirect")
}

func TestClient_errorsNeverLeakPassword(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t)

	tests := []struct {
		name string
		addr func(t *testing.T) string
	}{
		{name: "401", addr: func(t *testing.T) string {
			t.Helper()

			return p.serve(t, (&recorder{}).handler(http.StatusUnauthorized, "bad credentials"))
		}},
		{name: "500", addr: func(t *testing.T) string {
			t.Helper()

			return p.serve(t, (&recorder{}).handler(http.StatusInternalServerError, "boom"))
		}},
		{name: "refused", addr: deadAddr},
		{name: "untrusted", addr: func(t *testing.T) string {
			t.Helper()

			return other.serve(t, (&recorder{}).handler(http.StatusOK, "{}"))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, p, patroni.Options{Username: "patroni", Password: testPassword})
			addr := tt.addr(t)
			all := calls(t, c, addr)

			for _, name := range []string{"Patch", "Switchover", "Failover"} {
				err := all[name]()
				require.Error(t, err, name)
				assert.NotContains(t, err.Error(), testPassword, name)
			}
		})
	}
}

func TestClient_statusErrorKeepsPatroniMessage(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, (&recorder{}).handler(http.StatusPreconditionFailed, "candidate name does not match with sync_standby"))
	c := newClient(t, p, patroni.Options{Username: "patroni", Password: testPassword})

	_, err := c.Switchover(t.Context(), addr, "pg-1", "pg-2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "candidate name does not match")

	var se *source.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusPreconditionFailed, se.Code)
}

func TestClient_statusErrorBodyIsHardened(t *testing.T) {
	p := newPKI(t)

	body := "bad\r\nINJECTED \x1b[31mred\x00 " + string([]byte{0xff, 0xfe}) + strings.Repeat("я", 300)
	addr := p.serve(t, (&recorder{}).handler(http.StatusBadRequest, body))

	_, err := newClient(t, p, patroni.Options{}).Config(t.Context(), addr)
	require.Error(t, err)

	msg := err.Error()
	assert.True(t, utf8.ValidString(msg), "valid UTF-8")
	assert.NotContains(t, msg, "\n")
	assert.NotContains(t, msg, "\r")
	assert.NotContains(t, msg, "\x1b")
	assert.NotContains(t, msg, "\x00")
	assert.Contains(t, msg, "badINJECTED")
	assert.Less(t, len(msg), 600, "the body is cut")
}

// calls returns every method of the client against addr.
func calls(t *testing.T, c *patroni.Client, addr string) map[string]func() error {
	t.Helper()

	return map[string]func() error{
		"State":      func() error { return drop(c.State(t.Context(), addr)) },
		"Cluster":    func() error { return drop(c.Cluster(t.Context(), addr)) },
		"Config":     func() error { return drop(c.Config(t.Context(), addr)) },
		"History":    func() error { return drop(c.History(t.Context(), addr)) },
		"Primary":    func() error { return drop(c.Primary(t.Context(), addr)) },
		"Patch":      func() error { return drop(c.PatchConfig(t.Context(), addr, map[string]any{"ttl": 1})) },
		"Switchover": func() error { return drop(c.Switchover(t.Context(), addr, "pg-1", "pg-2")) },
		"Failover":   func() error { return drop(c.Failover(t.Context(), addr, "pg-2")) },
	}
}

func drop[T any](_ T, err error) error { return err }

func requireClassified(t *testing.T, c *patroni.Client, addr string, only []string, want source.Reason) {
	t.Helper()

	all := calls(t, c, addr)

	for _, name := range only {
		err := all[name]()
		require.Error(t, err, name)
		assertReason(t, err, want, name)

		var se *source.Error
		require.ErrorAs(t, err, &se, name)
		assert.Equal(t, addr, se.Endpoint, name)
	}
}

var (
	everyCall = []string{"State", "Cluster", "Config", "History", "Primary", "Patch", "Switchover", "Failover"}
	// decodingCalls are the calls that decode a JSON body.
	decodingCalls = []string{"State", "Cluster", "Config", "History", "Patch"}
)

func TestClient_transportErrors(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t)

	tests := []struct {
		name    string
		client  func(t *testing.T) *patroni.Client
		handler http.HandlerFunc
		addr    string
		want    source.Reason
	}{
		{name: "refused", addr: deadAddr(t), want: source.ReasonRefused},
		{
			name: "untrusted_certificate",
			client: func(t *testing.T) *patroni.Client {
				t.Helper()

				return newClient(t, other, patroni.Options{})
			},
			handler: func(w http.ResponseWriter, _ *http.Request) { serveJSON(w, []byte("{}")) },
			want:    source.ReasonTLS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, p, patroni.Options{})
			if tt.client != nil {
				c = tt.client(t)
			}

			addr := tt.addr
			if tt.handler != nil {
				addr = p.serve(t, tt.handler)
			}

			requireClassified(t, c, addr, everyCall, tt.want)
		})
	}

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })

		addr := p.serve(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(releaseOnce) // runs before the server is closed

		c := newClient(t, p, patroni.Options{RequestTimeout: 100 * time.Millisecond})
		bounded := []string{"State", "Cluster", "Config", "History", "Primary", "Patch"}
		requireClassified(t, c, addr, bounded, source.ReasonTimeout)
	})
}

func TestClient_statusErrors(t *testing.T) {
	p := newPKI(t)

	tests := []struct {
		name   string
		status int
		skip   []string
		want   source.Reason
	}{
		{name: "401", status: http.StatusUnauthorized, want: source.ReasonAuth},
		{name: "403", status: http.StatusForbidden, want: source.ReasonAuth},
		// 503 is the answer of GET /primary on any member that is not the primary.
		{name: "503", status: http.StatusServiceUnavailable, skip: []string{"Primary"}, want: source.ReasonHTTP},
		{name: "500", status: http.StatusInternalServerError, want: source.ReasonHTTP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.status) }))
			only := slices.DeleteFunc(slices.Clone(everyCall), func(n string) bool { return slices.Contains(tt.skip, n) })

			requireClassified(t, newClient(t, p, patroni.Options{}), addr, only, tt.want)
		})
	}
}

func TestClient_decodeErrors(t *testing.T) {
	p := newPKI(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "malformed_json", body: `{"role":`},
		{name: "empty_body", body: ``},
		{name: "wrong_type", body: `"oops"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveJSON(w, []byte(tt.body)) }))

			requireClassified(t, newClient(t, p, patroni.Options{}), addr, decodingCalls, source.ReasonInvalid)
		})
	}
}

func TestClient_canceledStaysUnclassified(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveJSON(w, []byte("{}")) }))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := newClient(t, p, patroni.Options{}).State(ctx, addr)
	require.ErrorIs(t, err, context.Canceled)

	_, ok := source.ReasonOf(err)
	assert.False(t, ok)
}

// Patroni holds a switchover longer than a read may take, so the request timeout does not
// apply to it; only the context of the caller does.
func TestClient_switchoverIsNotBoundedByRequestTimeout(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)

		_, _ = w.Write([]byte("Successfully switched over to pg-2"))
	}))
	c := newClient(t, p, patroni.Options{RequestTimeout: 100 * time.Millisecond})

	msg, err := c.Switchover(t.Context(), addr, "pg-1", "pg-2")
	require.NoError(t, err)
	assert.Contains(t, msg, "Successfully")

	_, err = c.Failover(t.Context(), addr, "pg-2")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err = c.Switchover(ctx, addr, "pg-1", "pg-2")
	require.Error(t, err)
	assertReason(t, err, source.ReasonTimeout)
}

func TestClient_responseSizeIsBounded(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 1<<20)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"x":"`))

		for range 8 {
			_, _ = w.Write(buf)
		}
	}))

	_, err := newClient(t, p, patroni.Options{}).Config(t.Context(), addr)
	require.Error(t, err)
	assertReason(t, err, source.ReasonInvalid)
}

func TestNew_validates(t *testing.T) {
	_, err := patroni.New(patroni.Options{})
	require.Error(t, err, "CA file is required")

	_, err = patroni.New(patroni.Options{CAFile: "testdata/missing.pem"})
	require.Error(t, err)

	bad := t.TempDir() + "/bad.pem"
	require.NoError(t, os.WriteFile(bad, []byte("not a pem"), 0o600))

	_, err = patroni.New(patroni.Options{CAFile: bad})
	require.Error(t, err)
}

// Plain HTTP on a TLS-only member is a TLS failure, not an unreachable endpoint.
func TestClient_plainHTTPMemberIsTLS(t *testing.T) {
	p := newPKI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveJSON(w, []byte("{}")) }))
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	requireClassified(t, newClient(t, p, patroni.Options{}), addr, everyCall, source.ReasonTLS)
}

// A body cut short by the server is an invalid response.
func TestClient_truncatedBodyIsInvalid(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"members":[`))
	}))

	requireClassified(t, newClient(t, p, patroni.Options{}), addr, decodingCalls, source.ReasonInvalid)
}

func assertReason(t *testing.T, err error, want source.Reason, msg ...any) {
	t.Helper()

	got, ok := source.ReasonOf(err)
	require.True(t, ok, "err is not classified: %v %v", err, msg)
	assert.Equal(t, want, got, "%v: %v", msg, err)
}
