package nomad_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// registerRequest is what the fake saw on a job register or plan.
type registerRequest struct {
	Job            map[string]any
	EnforceIndex   bool
	JobModifyIndex uint64
}

// fakeNomad is a Nomad HTTP API that keeps jobs in memory and enforces the job
// modify index like the real one.
type fakeNomad struct {
	mu        sync.Mutex
	jobs      map[string]map[string]any
	allocs    map[string]string
	nodes     string
	nodeMeta  map[string]map[string]string
	plan      string
	registers []registerRequest
	plans     []registerRequest
	deletes   []string
	// beforeRegister runs once before the first register is checked, to change a job
	// between the client's read and its write.
	beforeRegister func(f *fakeNomad)
	conflictStatus int
	conflictAlways bool
	gets           int
	version        string
	stale          map[string]bool
	planStatus     int
}

func newFakeNomad() *fakeNomad {
	return &fakeNomad{
		jobs:           map[string]map[string]any{},
		allocs:         map[string]string{},
		conflictStatus: http.StatusInternalServerError,
		version:        "1.10.0",
		stale:          map[string]bool{},
	}
}

func (f *fakeNomad) addJob(id string, index uint64, meta map[string]string, stop bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.jobs[id] = map[string]any{
		"ID":             id,
		"Name":           id,
		"Type":           "service",
		"Status":         "running",
		"Stop":           stop,
		"Meta":           meta,
		"ModifyIndex":    index,
		"JobModifyIndex": index,
		"TaskGroups":     []any{map[string]any{"Name": "g", "Unknown": "kept"}},
	}
}

func (f *fakeNomad) bumpIndex(id string) {
	f.jobs[id]["JobModifyIndex"] = f.jobs[id]["JobModifyIndex"].(uint64) + 1
}

func (f *fakeNomad) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/self", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{"member": map[string]any{"Tags": map[string]string{"build": f.version}}})
	})
	mux.HandleFunc("GET /v1/jobs", f.listJobs)
	mux.HandleFunc("GET /v1/job/{id}", f.getJob)
	mux.HandleFunc("GET /v1/job/{id}/allocations", f.listAllocs)
	mux.HandleFunc("PUT /v1/job/{id}/plan", f.planJob)
	mux.HandleFunc("PUT /v1/jobs", f.register)
	mux.HandleFunc("DELETE /v1/job/{id}", f.deregister)
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		_, _ = io.WriteString(w, f.nodes)
	})
	mux.HandleFunc("GET /v1/node/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{"ID": r.PathValue("id"), "Meta": f.nodeMeta[r.PathValue("id")]})
	})
	mux.HandleFunc("GET /v1/status/leader", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `"127.0.0.1:4647"`)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.stale[r.Method+" "+r.URL.Path] = r.URL.Query().Has("stale")
		f.mu.Unlock()

		mux.ServeHTTP(w, r)
	})
}

func (f *fakeNomad) listJobs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Query().Get("meta") != "true" {
		http.Error(w, "meta=true is required for job meta", http.StatusBadRequest)

		return
	}

	stubs := make([]map[string]any, 0, len(f.jobs))
	for _, j := range f.jobs {
		stubs = append(stubs, j)
	}

	_ = json.NewEncoder(w).Encode(stubs)
}

func (f *fakeNomad) getJob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gets++

	j, ok := f.jobs[r.PathValue("id")]
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)

		return
	}

	_ = json.NewEncoder(w).Encode(j)
}

func (f *fakeNomad) listAllocs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, _ = io.WriteString(w, f.allocs[r.PathValue("id")])
}

func (f *fakeNomad) planJob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var req registerRequest

	_ = json.NewDecoder(r.Body).Decode(&req)
	f.plans = append(f.plans, req)

	if f.planStatus != 0 {
		http.Error(w, "job validation failed: group has no tasks", f.planStatus)

		return
	}

	_, _ = io.WriteString(w, f.plan)
}

func (f *fakeNomad) register(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	id, _ := req.Job["ID"].(string)

	if f.beforeRegister != nil {
		hook := f.beforeRegister
		f.beforeRegister = nil
		hook(f)
	}

	cur := f.jobs[id]
	if f.conflictAlways {
		f.bumpIndex(id)
	}

	if req.EnforceIndex && req.JobModifyIndex != cur["JobModifyIndex"] {
		http.Error(w, "Enforcing job modify index "+strconv.FormatUint(req.JobModifyIndex, 10)+
			": job exists with conflicting job modify index: "+jsonNum(cur["JobModifyIndex"]), f.conflictStatus)

		return
	}

	f.registers = append(f.registers, req)
	req.Job["JobModifyIndex"] = cur["JobModifyIndex"].(uint64) + 1
	req.Job["ModifyIndex"] = req.Job["JobModifyIndex"]
	f.jobs[id] = req.Job

	_, _ = io.WriteString(w, `{"EvalID":"e1"}`)
}

func (f *fakeNomad) deregister(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := r.PathValue("id")

	f.deletes = append(f.deletes, id+"?"+r.URL.RawQuery)

	// Nomad answers 200 with an evaluation for a job that does not exist.
	if job, ok := f.jobs[id]; ok {
		job["Stop"] = true
	}

	_, _ = io.WriteString(w, `{"EvalID":"e2"}`)
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)

	return string(b)
}

// startTLS serves h over TLS with a certificate issued by ca and returns host:port.
func startTLS(t *testing.T, ca *testCA, h http.Handler) string {
	t.Helper()

	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t)}, MinVersion: tls.VersionTLS12}
	// A rejected handshake is the expected outcome in the CA mismatch test.
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "https://")
}

// startMutualTLS is startTLS for a server that requires a client certificate from ca.
func startMutualTLS(t *testing.T, ca *testCA, h http.Handler) string {
	t.Helper()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.serverCert(t)},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "https://")
}

// startPlain serves h over plain HTTP and returns host:port.
func startPlain(t *testing.T, h http.Handler) string {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

// fakeResolver is the Consul client of the tests.
type fakeResolver struct {
	// addrs are the HTTP API instances by "dc/service".
	addrs map[string][]string
	// extra are the rpc and serf instances Nomad registers under the same service name;
	// they come back whenever the tag asked for is not http.
	extra []string
	err   error
}

func (r *fakeResolver) HealthyAddrs(_ context.Context, dc, service, tag string) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}

	addrs := r.addrs[dc+"/"+service]
	if tag == "http" {
		return addrs, nil
	}

	return append(slices.Clone(addrs), r.extra...), nil
}
