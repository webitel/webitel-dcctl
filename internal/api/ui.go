package api

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/webitel/webitel-go-kit/pkg/logging"
)

const (
	indexFile   = "index.html"
	faviconFile = "favicon.ico"
	// assetsDir holds the build output with content hashes in the names, so
	// a file there never changes.
	assetsDir = "assets"

	immutableCache = "public, max-age=31536000, immutable"

	// contentSecurityPolicy guards every response of the UI: the UI keeps the
	// credentials in sessionStorage, where any injected script could read
	// them. Styles stay inline-capable because the component library injects
	// <style> elements.
	contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; " +
		"base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
)

// encodings are the pre-compressed variants of a file, in order of
// preference: <file>.br, then <file>.gz.
var encodings = []struct{ name, ext string }{{"br", ".br"}, {"gzip", ".gz"}}

type ui struct {
	fsys fs.FS
	log  *logging.Logger
}

// NewUI returns the handler of the UI build in fsys. It serves only
// index.html, favicon.ico and the files of assets/, those with immutable
// caching; any other path outside /api/ gets index.html, so that routes of
// the UI load the UI and other files of the build stay unseen. A file with
// a .br or .gz sibling is served pre-compressed when the client accepts it.
func NewUI(fsys fs.FS, log *logging.Logger) (http.Handler, error) {
	if _, err := fs.Stat(fsys, indexFile); err != nil {
		return nil, fmt.Errorf("ui: %w", err)
	}

	return ui{fsys: fsys, log: log}, nil
}

func (u ui) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)

		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	switch {
	case name == "api", strings.HasPrefix(name, "api/"):
		// The gateway serves /api/; a request that reaches the UI there is
		// not a route of the UI.
		http.NotFound(w, r)
	case name == assetsDir || strings.HasPrefix(name, assetsDir+"/"):
		u.file(w, r, name, immutableCache)
	case name == faviconFile:
		u.file(w, r, name, "no-cache")
	default:
		u.file(w, r, indexFile, "no-cache")
	}
}

// file serves the regular file name with the Cache-Control cache. A file
// that cannot be opened, also one outside the root of fsys, is not found.
func (u ui) file(w http.ResponseWriter, r *http.Request, name, cache string) {
	if !fs.ValidPath(name) {
		http.NotFound(w, r)

		return
	}

	f, encoding, err := u.open(name, r.Header.Get("Accept-Encoding"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)

		return
	}

	content, err := seeker(f)
	if err != nil {
		u.log.WarnCtx(r.Context(), "read ui file", logging.Str("file", name), logging.Err(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	h := w.Header()
	h.Set("Cache-Control", cache)
	h.Set("Vary", "Accept-Encoding")

	// The type is of the file asked for: content sniffing would look at the
	// compressed bytes.
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	h.Set("Content-Type", ctype)

	if encoding != "" {
		h.Set("Content-Encoding", encoding)
	}

	http.ServeContent(w, r, name, info.ModTime(), content)
}

// open opens the best pre-compressed variant of name the client accepts, or
// name itself, with the encoding of what it opened. Only regular files open.
func (u ui) open(name, acceptEncoding string) (fs.File, string, error) {
	for _, e := range encodings {
		if !accepts(acceptEncoding, e.name) {
			continue
		}

		if f, err := u.openFile(name + e.ext); err == nil {
			return f, e.name, nil
		}
	}

	f, err := u.openFile(name)

	return f, "", err
}

func (u ui) openFile(name string) (fs.File, error) {
	f, err := u.fsys.Open(name)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fs.ErrNotExist
	}

	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return f, nil
}

// accepts reports whether the Accept-Encoding header lists encoding with a
// non-zero weight.
func accepts(header, encoding string) bool {
	for part := range strings.SplitSeq(header, ",") {
		token, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(token), encoding) {
			continue
		}

		q, ok := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !ok {
			return true
		}

		weight, err := strconv.ParseFloat(q, 64)

		return err == nil && weight > 0
	}

	return false
}

// seeker returns f as an io.ReadSeeker for http.ServeContent, reading it into
// memory when the file system gives files that cannot seek.
func seeker(f fs.File) (io.ReadSeeker, error) {
	if rs, ok := f.(io.ReadSeeker); ok {
		return rs, nil
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(b), nil
}
