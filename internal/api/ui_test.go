package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	indexHTML = `<!doctype html><script type="module" src="/dcctl/assets/index-abc.js"></script>`
	appJS     = "console.log('app')"
)

func uiFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(indexHTML)},
		"favicon.ico":            {Data: []byte("icon")},
		".env":                   {Data: []byte("SECRET=1")},
		"stats.html":             {Data: []byte("bundle stats")},
		"assets/index-abc.js":    {Data: []byte(appJS)},
		"assets/index-abc.js.br": {Data: []byte("brotli")},
		"assets/index-abc.js.gz": {Data: []byte("gzip")},
		"assets/style-def.css":   {Data: []byte("body{}")},
		"assets/font.woff2.gz":   {Data: []byte("only gzip")},
		"assets/font.woff2":      {Data: []byte("font")},
	}
}

func serveUI(t *testing.T, fsys fs.FS, method, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()

	h, err := NewUI(fsys, testLogger())
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// assertSecurityHeaders checks the headers every response of the UI has.
func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	assert.Equal(t, contentSecurityPolicy, rec.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
}

func assertIndex(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, indexHTML, rec.Body.String())
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assertSecurityHeaders(t, rec)
}

func assertNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "<!doctype html>")
	assertSecurityHeaders(t, rec)
}

func TestUIIndex(t *testing.T) {
	for _, target := range []string{"/", "/index.html"} {
		t.Run(target, func(t *testing.T) {
			assertIndex(t, serveUI(t, uiFS(), http.MethodGet, target))
		})
	}
}

func TestUIFallback(t *testing.T) {
	for _, target := range []string{
		"/datacenters/dc_a", "/operations/op-1", "/journal?type=ALARM", "/assets-like",
		"/dcctl/assets/index-abc.js", "/dcctl/api/v1/status",
	} {
		t.Run(target, func(t *testing.T) {
			assertIndex(t, serveUI(t, uiFS(), http.MethodGet, target))
		})
	}
}

func TestUIOnlyAllowlisted(t *testing.T) {
	for _, target := range []string{"/.env", "/stats.html", "/assets/../.env"} {
		t.Run(target, func(t *testing.T) {
			assertIndex(t, serveUI(t, uiFS(), http.MethodGet, target))
		})
	}
}

func TestUICSP(t *testing.T) {
	assert.Equal(t, "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; "+
		"base-uri 'self'; form-action 'self'; frame-ancestors 'none'", contentSecurityPolicy)
}

func TestUIAsset(t *testing.T) {
	rec := serveUI(t, uiFS(), http.MethodGet, "/assets/index-abc.js")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, appJS, rec.Body.String())
	assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	assertSecurityHeaders(t, rec)
}

func TestUIAssetMissing(t *testing.T) {
	for _, target := range []string{"/assets/index-old.js", "/assets/gone.css", "/assets", "/assets/"} {
		t.Run(target, func(t *testing.T) {
			assertNotFound(t, serveUI(t, uiFS(), http.MethodGet, target))
		})
	}
}

func TestUIPrecompressed(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		accept   string
		wantBody string
		wantEnc  string
	}{
		{name: "brotli", target: "/assets/index-abc.js", accept: "gzip, deflate, br", wantBody: "brotli", wantEnc: "br"},
		{name: "gzip", target: "/assets/index-abc.js", accept: "gzip", wantBody: "gzip", wantEnc: "gzip"},
		{name: "brotli_refused", target: "/assets/index-abc.js", accept: "br;q=0, gzip", wantBody: "gzip", wantEnc: "gzip"},
		{name: "case", target: "/assets/index-abc.js", accept: "GZIP", wantBody: "gzip", wantEnc: "gzip"},
		{name: "identity", target: "/assets/index-abc.js", accept: "", wantBody: appJS},
		{name: "gzip_refused", target: "/assets/index-abc.js", accept: "gzip;q=0.0", wantBody: appJS},
		{name: "only_gzip_file", target: "/assets/font.woff2", accept: "br", wantBody: "font"},
		{name: "no_variant", target: "/assets/style-def.css", accept: "br, gzip", wantBody: "body{}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveUI(t, uiFS(), http.MethodGet, tt.target, "Accept-Encoding", tt.accept)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tt.wantBody, rec.Body.String())
			assert.Equal(t, tt.wantEnc, rec.Header().Get("Content-Encoding"))
			assert.Equal(t, "Accept-Encoding", rec.Header().Get("Vary"))
		})
	}
}

func TestUIPrecompressedType(t *testing.T) {
	rec := serveUI(t, uiFS(), http.MethodGet, "/assets/index-abc.js", "Accept-Encoding", "br")

	assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestUIFavicon(t *testing.T) {
	rec := serveUI(t, uiFS(), http.MethodGet, "/favicon.ico")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "icon", rec.Body.String())
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assertSecurityHeaders(t, rec)
}

func TestUINotAPI(t *testing.T) {
	for _, target := range []string{"/api", "/api/v1/status"} {
		t.Run(target, func(t *testing.T) {
			assertNotFound(t, serveUI(t, uiFS(), http.MethodGet, target))
		})
	}
}

func TestUIMethods(t *testing.T) {
	rec := serveUI(t, uiFS(), http.MethodPost, "/")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
	assertSecurityHeaders(t, rec)

	rec = serveUI(t, uiFS(), http.MethodHead, "/datacenters/dc_a")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
	assertSecurityHeaders(t, rec)
}

// rootFS lays out a UI directory next to a secret file and returns the UI
// directory opened as an os.Root, as serve opens it.
func rootFS(t *testing.T) fs.FS {
	t.Helper()

	dir := t.TempDir()
	ui := filepath.Join(dir, "ui")

	require.NoError(t, os.MkdirAll(filepath.Join(ui, "assets"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(ui, "index.html"), []byte(indexHTML), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(ui, "assets", "link.txt")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(ui, "favicon.ico")))

	root, err := os.OpenRoot(ui)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	return root.FS()
}

func TestUITraversal(t *testing.T) {
	fsys := rootFS(t)

	// Paths that clean to a file outside the allowlist get index.html.
	for _, target := range []string{
		"/../secret.txt",
		"/assets/../../secret.txt",
		"/assets/..%2f..%2fsecret.txt",
		"/%2e%2e/secret.txt",
	} {
		t.Run(target, func(t *testing.T) {
			assertIndex(t, serveUI(t, fsys, http.MethodGet, target))
		})
	}

	// Allowlisted names that escape the root or cannot be opened are not
	// found.
	for _, target := range []string{"/assets/link.txt", "/favicon.ico", "/assets/a%00.js"} {
		t.Run(target, func(t *testing.T) {
			rec := serveUI(t, fsys, http.MethodGet, target)

			assertNotFound(t, rec)
			assert.NotContains(t, rec.Body.String(), "secret")
		})
	}
}

func TestNewUIWithoutIndex(t *testing.T) {
	_, err := NewUI(fstest.MapFS{"assets/a.js": {Data: []byte("x")}}, testLogger())
	assert.Error(t, err)
}

func TestUIThroughRuntime(t *testing.T) {
	opts := testOptions()
	opts.UI = uiFS()
	h := startAPI(t, opts)

	resp := h.get(t, "/datacenters/dc_a")
	assert.Equal(t, http.StatusOK, resp.code)
	assert.Equal(t, indexHTML, resp.body)
	assert.Equal(t, contentSecurityPolicy, resp.header.Get("Content-Security-Policy"))

	resp = h.get(t, "/assets/index-abc.js", "Accept-Encoding", "br")
	assert.Equal(t, "brotli", resp.body)
	assert.Equal(t, "br", resp.header.Get("Content-Encoding"))

	resp = h.get(t, "/api/v1/nope")
	assert.Equal(t, http.StatusNotFound, resp.code)
	assert.JSONEq(t, `{"code":404,"status":"Not Found","detail":"Not Found"}`, resp.body)

	resp = h.get(t, "/api/v1/status")
	assert.Equal(t, http.StatusOK, resp.code)
	assert.Contains(t, resp.body, `"name":"status"`)
}
