package patroni_test

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func TestClient_State(t *testing.T) {
	p := newPKI(t)

	tests := []struct {
		name string
		file string
		want patroni.State
	}{
		{
			name: "primary",
			file: "patroni_primary.json",
			want: patroni.State{
				Role: patroni.RolePrimary, State: "running", Timeline: ptr[int64](4), Version: "4.1.3",
				Xlog: patroni.Xlog{Location: ptr[int64](117440512)},
				Replication: []patroni.Replica{
					{Username: "replication", ApplicationName: "pg-2", ClientAddr: "10.0.0.12", State: "streaming", SyncState: "async"},
					{Username: "replication", ApplicationName: "pg-3", ClientAddr: "10.0.0.13", State: "streaming", SyncState: "async"},
				},
			},
		},
		{
			name: "standby_leader",
			file: "patroni_standby_leader.json",
			want: patroni.State{
				Role: patroni.RoleStandbyLeader, State: "running", Timeline: ptr[int64](4), Version: "4.1.5",
				ReplicationState: "streaming", Pause: true,
				Xlog: patroni.Xlog{
					ReceivedLocation: ptr[int64](100663296), ReplayedLocation: ptr[int64](100663296),
					ReplayedTimestamp: "2026-11-24 09:59:58.000000+00:00",
				},
				Replication: []patroni.Replica{
					{Username: "replication", ApplicationName: "pg-5", ClientAddr: "10.1.0.15", State: "streaming", SyncState: "async"},
				},
			},
		},
		{
			name: "replica",
			file: "patroni_replica.json",
			want: patroni.State{
				Role: patroni.RoleReplica, State: "running", Timeline: ptr[int64](4), Version: "4.1.3",
				ReplicationState: "streaming",
				Xlog:             patroni.Xlog{ReceivedLocation: ptr[int64](117440000), ReplayedLocation: ptr[int64](117439488)},
			},
		},
		{
			// Postgres is down: nothing is known about the timeline and the positions, and
			// none of them may read as zero.
			name: "stopped",
			file: "patroni_stopped.json",
			want: patroni.State{Role: patroni.RoleReplica, State: "stopped", Version: "4.1.3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fixture(t, tt.file)
			addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/patroni", r.URL.Path)
				serveJSON(w, body)
			}))

			got, err := newClient(t, p, patroni.Options{}).State(t.Context(), addr)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClient_Cluster(t *testing.T) {
	p := newPKI(t)
	body := fixture(t, "cluster.json")
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/cluster", r.URL.Path)
		serveJSON(w, body)
	}))

	got, err := newClient(t, p, patroni.Options{}).Cluster(t.Context(), addr)
	require.NoError(t, err)

	assert.Equal(t, []patroni.Member{
		{
			Name: "pg-1", Host: "10.0.0.11", APIURL: "https://10.0.0.11:8008/patroni", Endpoint: "10.0.0.11:8008",
			Role: patroni.ClusterRoleLeader, State: "running", Timeline: ptr[int64](4), FailoverPriority: 3,
		},
		{
			Name: "pg-2", Host: "10.0.0.12", APIURL: "https://10.0.0.12:8008/patroni", Endpoint: "10.0.0.12:8008",
			Role: patroni.ClusterRoleReplica, State: "streaming", Timeline: ptr[int64](4), Lag: ptr[int64](0), FailoverPriority: 2,
		},
		{
			Name: "pg-3", Host: "10.0.0.13", APIURL: "https://10.0.0.13:8008/patroni", Endpoint: "10.0.0.13:8008",
			Role: patroni.ClusterRoleReplica, State: "streaming", Timeline: ptr[int64](4), FailoverPriority: 1, NoFailover: true,
		},
		{
			// No tags and no timeline: the Patroni defaults and unknown.
			Name: "pg-6", Host: "10.0.0.16", APIURL: "https://10.0.0.16:8008/patroni", Endpoint: "10.0.0.16:8008",
			Role: patroni.ClusterRoleReplica, State: "stopped", FailoverPriority: 1,
		},
	}, got)
}

func TestClient_Cluster_standby(t *testing.T) {
	p := newPKI(t)
	body := fixture(t, "cluster_standby.json")
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serveJSON(w, body) }))

	got, err := newClient(t, p, patroni.Options{}).Cluster(t.Context(), addr)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, patroni.ClusterRoleStandbyLeader, got[0].Role)
	assert.Equal(t, 2, got[0].FailoverPriority)
	assert.Equal(t, ptr[int64](1024), got[1].Lag)
}

func TestClient_Cluster_lag(t *testing.T) {
	p := newPKI(t)

	tests := []struct {
		name    string
		lag     string
		want    *int64
		invalid bool
	}{
		{name: "number", lag: `7`, want: ptr[int64](7)},
		{name: "unknown", lag: `"unknown"`},
		{name: "null", lag: `null`},
		{name: "other_string", lag: `"lots"`, invalid: true},
		{name: "object", lag: `{}`, invalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serveJSON(w, []byte(`{"members":[{"name":"pg-1","lag":`+tt.lag+`}]}`))
			}))

			got, err := newClient(t, p, patroni.Options{}).Cluster(t.Context(), addr)
			if tt.invalid {
				assertReason(t, err, source.ReasonInvalid)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got[0].Lag)
		})
	}
}

func TestClient_Cluster_rejectsPlainHTTPMember(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, []byte(`{"members":[{"name":"pg-1","api_url":"http://10.0.0.11:8008/patroni","host":"10.0.0.11"}]}`))
	}))

	got, err := newClient(t, p, patroni.Options{}).Cluster(t.Context(), addr)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Endpoint, "a member without an https API URL must not get an endpoint")
}

func TestClient_Config(t *testing.T) {
	p := newPKI(t)
	body := fixture(t, "config.json")
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/config", r.URL.Path)
		serveJSON(w, body)
	}))

	got, err := newClient(t, p, patroni.Options{}).Config(t.Context(), addr)
	require.NoError(t, err)
	assert.EqualValues(t, 1048576, got["maximum_lag_on_failover"])
	assert.Equal(t, false, got["synchronous_mode"])
}

func TestClient_History(t *testing.T) {
	p := newPKI(t)
	body := fixture(t, "history.json")
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/history", r.URL.Path)
		serveJSON(w, body)
	}))

	got, err := newClient(t, p, patroni.Options{}).History(t.Context(), addr)
	require.NoError(t, err)
	assert.Equal(t, []patroni.HistoryEntry{
		{Timeline: 1, LSN: 25165824, Reason: "no recovery target specified", Timestamp: "2026-10-01T08:00:00+00:00", NewLeader: "pg-1"},
		{Timeline: 2, LSN: 50331648, Reason: "no recovery target specified", Timestamp: "2026-10-15T08:00:00+00:00", NewLeader: "pg-2"},
		{Timeline: 3, LSN: 83886080, Reason: "no recovery target specified", Timestamp: "2026-11-01T09:00:00+00:00"},
	}, got)
}

func TestClient_History_malformedEntry(t *testing.T) {
	p := newPKI(t)
	addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, []byte(`[[1, 25165824]]`))
	}))

	_, err := newClient(t, p, patroni.Options{}).History(t.Context(), addr)
	require.Error(t, err)
	assertReason(t, err, source.ReasonInvalid)
}

func TestClient_Primary(t *testing.T) {
	p := newPKI(t)

	tests := []struct {
		name   string
		status int
		want   bool
		reason source.Reason
	}{
		{name: "primary", status: http.StatusOK, want: true},
		{name: "not_primary", status: http.StatusServiceUnavailable},
		{name: "forbidden", status: http.StatusForbidden, reason: source.ReasonAuth},
		{name: "server_error", status: http.StatusInternalServerError, reason: source.ReasonHTTP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/primary", r.URL.Path)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"role":"replica"}`))
			}))

			got, err := newClient(t, p, patroni.Options{}).Primary(t.Context(), addr)
			if tt.reason != "" {
				assertReason(t, err, tt.reason)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

type recorded struct {
	method, path, contentType, user, pass, body string
	hasAuth                                     bool
}

type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (r *recorder) handler(status int, resp string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		user, pass, ok := req.BasicAuth()

		r.mu.Lock()
		r.reqs = append(r.reqs, recorded{
			method: req.Method, path: req.URL.Path, contentType: req.Header.Get("Content-Type"),
			user: user, pass: pass, hasAuth: ok || req.Header.Get("Authorization") != "", body: string(body),
		})
		r.mu.Unlock()

		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	})
}
