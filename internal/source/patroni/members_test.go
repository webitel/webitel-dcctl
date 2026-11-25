package patroni_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// memberServer serves GET /patroni with the fixture; GET /cluster is served by cluster.
func memberServer(t *testing.T, p *pki, state string, cluster func() []byte) string {
	t.Helper()

	body := fixture(t, state)

	return p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/patroni":
			serveJSON(w, body)
		case "/cluster":
			serveJSON(w, cluster())
		default:
			http.NotFound(w, r)
		}
	}))
}

func clusterJSON(members ...patroni.Member) []byte {
	out := `{"scope":"webitel-postgres","members":[`

	for i, m := range members {
		if i > 0 {
			out += ","
		}

		out += fmt.Sprintf(`{"name":%q,"role":%q,"state":%q,"api_url":"https://%s/patroni","host":"127.0.0.1","port":5432,"timeline":4,"lag":0}`,
			m.Name, m.Role, m.State, m.Endpoint)
	}

	return []byte(out + "]}")
}

type fleet struct {
	p                *pki
	leader, rep, old string
	cluster          []byte
}

// newFleet starts a 4.1 primary, a 4.1 replica and a 4.0 replica that all report the same cluster.
func newFleet(t *testing.T) *fleet {
	t.Helper()

	f := &fleet{p: newPKI(t)}
	cluster := func() []byte { return f.cluster }
	f.leader = memberServer(t, f.p, "patroni_primary.json", cluster)
	f.rep = memberServer(t, f.p, "patroni_replica.json", cluster)
	f.old = memberServer(t, f.p, "patroni_replica_4_0.json", cluster)
	f.cluster = clusterJSON(
		patroni.Member{Name: "pg-1", Role: "leader", State: "running", Endpoint: f.leader},
		patroni.Member{Name: "pg-2", Role: "replica", State: "streaming", Endpoint: f.rep},
		patroni.Member{Name: "pg-3", Role: "replica", State: "streaming", Endpoint: f.old},
	)

	return f
}

func TestClient_Members_staticEndpoints(t *testing.T) {
	f := newFleet(t)
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{f.rep}})

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 3)

	byName := map[string]patroni.Member{}
	for _, m := range got {
		byName[m.Name] = m
	}

	pg1 := byName["pg-1"]
	assert.True(t, pg1.Usable)
	assert.Empty(t, pg1.UnusableReason)
	assert.Equal(t, "4.1.3", pg1.Probe.Version)
	assert.Equal(t, f.leader, pg1.Endpoint)
	assert.NoError(t, pg1.Err)

	pg2 := byName["pg-2"]
	assert.True(t, pg2.Usable)

	pg3 := byName["pg-3"]
	assert.False(t, pg3.Usable, "4.0.x must not be used for operations")
	assert.Equal(t, patroni.UnusableVersion, pg3.UnusableReason)
	assert.Equal(t, "4.0.6", pg3.Probe.Version)
}

func TestClient_Members_consulDiscovery(t *testing.T) {
	f := newFleet(t)
	host, port, err := net.SplitHostPort(f.leader)
	require.NoError(t, err)

	// Consul holds the PostgreSQL port; the REST port comes from APIPort.
	r := &fakeResolver{addrs: []string{net.JoinHostPort(host, "5432")}}
	c := newClient(t, f.p, patroni.Options{Scope: "webitel-postgres", Datacenter: "dc_a", APIPort: port, Resolver: r})

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.Equal(t, "dc_a", r.gotDC)
	assert.Equal(t, "webitel-postgres", r.gotName)
	assert.Empty(t, r.gotTag)
}

func TestClient_Members_staticEndpointsWinOverConsul(t *testing.T) {
	f := newFleet(t)
	r := &fakeResolver{err: errors.New("consul must not be asked")}
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{f.leader}, Resolver: r})

	_, err := c.Members(t.Context())
	require.NoError(t, err)
	assert.Empty(t, r.gotName)
}

func TestClient_Members_skipsUnreachableSeed(t *testing.T) {
	f := newFleet(t)
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{deadAddr(t), f.leader}})

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestClient_Members_allSeedsFail(t *testing.T) {
	f := newFleet(t)
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{deadAddr(t), "127.0.0.1:1"}})

	_, err := c.Members(t.Context())
	require.Error(t, err)

	_, ok := source.ReasonOf(err)
	assert.True(t, ok, "the failure of the seeds is classified: %v", err)
}

func TestClient_Members_noEndpoints(t *testing.T) {
	p := newPKI(t)

	_, err := newClient(t, p, patroni.Options{}).Members(t.Context())
	require.ErrorIs(t, err, patroni.ErrNoEndpoints)
	require.ErrorIs(t, err, source.ErrNoEndpoints)

	r := &fakeResolver{}
	_, err = newClient(t, p, patroni.Options{Scope: "s", Datacenter: "dc_a", Resolver: r}).Members(t.Context())
	require.ErrorIs(t, err, patroni.ErrNoEndpoints)

	r = &fakeResolver{err: errors.New("consul is down")}
	_, err = newClient(t, p, patroni.Options{Scope: "s", Datacenter: "dc_a", Resolver: r}).Members(t.Context())
	require.ErrorContains(t, err, "consul is down")
}

func TestClient_Members_unreachableAndTLS(t *testing.T) {
	f := newFleet(t)
	untrusted := memberServer(t, newPKI(t), "patroni_replica.json", func() []byte { return nil })
	dead := deadAddr(t)

	f.cluster = clusterJSON(
		patroni.Member{Name: "pg-1", Role: "leader", State: "running", Endpoint: f.leader},
		patroni.Member{Name: "pg-2", Role: "replica", State: "streaming", Endpoint: dead},
		patroni.Member{Name: "pg-3", Role: "replica", State: "streaming", Endpoint: untrusted},
	)
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{f.leader}})

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.True(t, got[0].Usable)

	assert.False(t, got[1].Usable)
	assert.Equal(t, patroni.UnusableUnreachable, got[1].UnusableReason)
	assertReason(t, got[1].Err, source.ReasonRefused)

	assert.False(t, got[2].Usable)
	assert.Equal(t, patroni.UnusableTLS, got[2].UnusableReason)
	assertReason(t, got[2].Err, source.ReasonTLS)
}

func TestClient_Members_stoppedMemberHasUnknownPositions(t *testing.T) {
	f := newFleet(t)
	stopped := memberServer(t, f.p, "patroni_stopped.json", func() []byte { return f.cluster })
	f.cluster = clusterJSON(patroni.Member{Name: "pg-6", Role: "replica", State: "stopped", Endpoint: stopped})
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{stopped}})

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Probe)
	assert.Nil(t, got[0].Probe.Xlog.ReceivedLocation)
	assert.Nil(t, got[0].Probe.Timeline)
}

func TestClient_Members_memberWithoutEndpoint(t *testing.T) {
	f := newFleet(t)
	f.cluster = []byte(`{"members":[{"name":"pg-1","role":"leader","api_url":"http://10.0.0.11:8008/patroni","host":"10.0.0.11"}]}`)
	c := newClient(t, f.p, patroni.Options{Endpoints: []string{f.leader}})

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Usable)
	assert.Equal(t, patroni.UnusableTLS, got[0].UnusableReason)
	assert.Empty(t, got[0].Endpoint)
	assertReason(t, got[0].Err, source.ReasonInvalid)
}

func TestClient_Members_brokenPatroniAnswer(t *testing.T) {
	p := newPKI(t)

	var addr string

	addr = p.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster" {
			serveJSON(w, clusterJSON(patroni.Member{Name: "pg-1", Role: "leader", State: "running", Endpoint: addr}))

			return
		}

		serveJSON(w, []byte(`{"role":"primary","patroni":{"scope":"s"}}`))
	}))

	got, err := newClient(t, p, patroni.Options{Endpoints: []string{addr}}).Members(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Usable, "a member that does not report its version is not used")
	assert.Equal(t, patroni.UnusableVersion, got[0].UnusableReason)
}

func TestFromConfig(t *testing.T) {
	f := newFleet(t)
	r := &fakeResolver{}

	cfg := config.Serve{
		TLS:    config.TLS{CA: f.p.caFile},
		Status: config.Status{RequestTimeout: time.Second},
		Patroni: config.Patroni{
			Scope:     "webitel-postgres",
			Username:  "patroni",
			Password:  testPassword,
			Endpoints: []string{f.leader},
		},
	}

	c, err := patroni.FromConfig(cfg, "dc_b", r)
	require.NoError(t, err)

	got, err := c.Members(t.Context())
	require.NoError(t, err)
	assert.Len(t, got, 3, "patroni.endpoints are the seeds")
	assert.Empty(t, r.gotName, "static endpoints win over Consul")

	cfg.TLS.CA = ""
	_, err = patroni.FromConfig(cfg, "dc_b", r)
	require.Error(t, err, "tls.ca is required")
}

func TestVersionSupported(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "4.1.0", want: true},
		{in: "4.1.3", want: true},
		{in: "4.1.5", want: true},
		{in: "4.2.0", want: true},
		{in: "5.0.0", want: true},
		{in: "10.0.1", want: true},
		{in: "4.1.0.dev1", want: true},
		{in: "4.1", want: true},
		{in: "4.0.6", want: false},
		{in: "3.9.9", want: false},
		{in: "", want: false},
		{in: "dev", want: false},
		{in: "4.x.0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, patroni.VersionSupported(tt.in))
		})
	}
}
