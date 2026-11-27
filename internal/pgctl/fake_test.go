package pgctl_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

var (
	_ pgctl.Patroni = (*patroni.Client)(nil)
	_ pgctl.Catalog = (*consul.Clients)(nil)
)

const (
	scope = "webitel-postgres"
	dcA   = "dc_a"
	dcB   = "dc_b"
	dcC   = "dc_c"
)

var (
	timeouts = config.PatroniTimeouts{
		Demote:  120 * time.Second,
		Catchup: 60 * time.Second,
		Promote: 120 * time.Second,
		Repoint: 300 * time.Second,
		Rewind:  600 * time.Second,
	}

	errDown = errors.New("connection refused")
	errLost = errors.New("lease lost")
)

func allow(context.Context) error { return nil }

func i64(v int64) *int64 { return &v }

// world is the Patroni clusters and the Consul catalog of every datacenter.
type world struct {
	catalog *fakeCatalog
	ctl     *pgctl.Controller
}

func newWorld(t *testing.T) *world {
	t.Helper()

	cat := &fakeCatalog{hosts: map[string]string{}, primaryOverride: map[string][]string{}}

	ctl, err := pgctl.New(config.Patroni{Scope: scope, Timeouts: timeouts}, cat, logging.New(logging.WithProvider(logtest.NewRecorder())))
	require.NoError(t, err)

	return &world{catalog: cat, ctl: ctl}
}

// cluster adds a Patroni cluster whose members run in dc.
func (w *world) cluster(dc string, members ...patroni.Member) *fakeCluster {
	f := &fakeCluster{config: map[string]any{}, readErr: map[string]error{}}
	for _, m := range members {
		f.add(dc, m)
		w.catalog.hosts[m.Host] = dc
	}

	w.catalog.clusters = append(w.catalog.clusters, f)

	return f
}

// member is a usable member at host whose own role is role.
func member(name, host string, role patroni.Role, timeline int64, priority int) patroni.Member {
	endpoint := net.JoinHostPort(host, patroni.DefaultAPIPort)

	return patroni.Member{
		Name: name, Host: host, APIURL: "https://" + endpoint + "/patroni", Endpoint: endpoint,
		Role: clusterRole(role), State: "running", Timeline: i64(timeline), FailoverPriority: priority, Usable: true,
		Probe: &patroni.State{Role: role, State: "running", Timeline: i64(timeline), Version: "4.1.5"},
	}
}

// event is a change of the fake due at a time.
type event struct {
	at time.Time
	fn func(f *fakeCluster)
}

type patchCall struct {
	endpoint string
	patch    map[string]any
}

// fakeCluster is a Patroni cluster in memory. Changes scheduled with after happen on the
// first read at or past their time.
type fakeCluster struct {
	mu      sync.Mutex
	members []patroni.Member
	config  map[string]any
	events  []event

	// onPatch and onPost react to a request that was applied, usually by scheduling events.
	onPatch func(f *fakeCluster, patch map[string]any)
	onPost  func(f *fakeCluster, path, candidate string)
	// patchErr and postErr answer the requests; patchApplies applies a failing PATCH anyway,
	// as a PATCH answered after the connection broke.
	patchErr     error
	patchApplies bool
	// patchDelay is how long a PATCH takes; one whose context ends first is not applied.
	patchDelay time.Duration
	postErr    error
	postAnswer string
	// postHang holds the switchover or failover until its context ends.
	postHang bool
	// membersErr fails the discovery, readErr the GET /patroni of a member.
	membersErr error
	readErr    map[string]error
	primaryErr error
	// history answers GET /history, historyErr fails it.
	history    []patroni.HistoryEntry
	historyErr error

	patches []patchCall
	posts   []string
}

func (f *fakeCluster) add(_ string, m patroni.Member) { f.members = append(f.members, m) }

// after schedules fn d from now.
func (f *fakeCluster) after(d time.Duration, fn func(f *fakeCluster)) {
	f.events = append(f.events, event{at: time.Now().Add(d), fn: fn})
}

// tick applies the events that are due; f.mu is held.
func (f *fakeCluster) tick() {
	now := time.Now()

	for {
		i := slices.IndexFunc(f.events, func(e event) bool { return !e.at.After(now) })
		if i < 0 {
			return
		}

		e := f.events[i]
		f.events = slices.Delete(f.events, i, i+1)
		e.fn(f)
	}
}

// m returns the member called name; f.mu is held.
func (f *fakeCluster) m(name string) *patroni.Member {
	i := slices.IndexFunc(f.members, func(m patroni.Member) bool { return m.Name == name })
	if i < 0 {
		panic("no member " + name)
	}

	return &f.members[i]
}

func (f *fakeCluster) byEndpoint(endpoint string) *patroni.Member {
	i := slices.IndexFunc(f.members, func(m patroni.Member) bool { return m.Endpoint == endpoint })
	if i < 0 {
		return nil
	}

	return &f.members[i]
}

// setRole sets the role of a member in GET /cluster and in its GET /patroni.
func (f *fakeCluster) setRole(name string, role patroni.Role) {
	m := f.m(name)
	m.Probe.Role, m.Role = role, clusterRole(role)
}

// clusterRole is what GET /cluster shows for a member whose own role is role.
func clusterRole(role patroni.Role) patroni.ClusterRole {
	switch role {
	case patroni.RolePrimary:
		return patroni.ClusterRoleLeader
	case patroni.RoleStandbyLeader:
		return patroni.ClusterRoleStandbyLeader
	case patroni.RoleReplica, patroni.RoleDemoted, patroni.RolePromoted, patroni.RoleUninitialized:
	}

	return patroni.ClusterRoleReplica
}

func (f *fakeCluster) setTimeline(name string, tl int64) {
	m := f.m(name)
	m.Timeline, m.Probe.Timeline = i64(tl), i64(tl)
}

func (f *fakeCluster) do(fn func(f *fakeCluster)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

func (f *fakeCluster) Members(context.Context) ([]patroni.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tick()

	if f.membersErr != nil {
		return nil, f.membersErr
	}

	out := slices.Clone(f.members)

	for i := range out {
		if err := f.readErr[out[i].Name]; err != nil {
			out[i].Probe, out[i].Err, out[i].Usable = nil, err, false

			continue
		}

		p := *out[i].Probe
		p.Replication = slices.Clone(p.Replication)
		out[i].Probe = &p
	}

	return out, nil
}

func (f *fakeCluster) Primary(_ context.Context, endpoint string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tick()

	if f.primaryErr != nil {
		return false, f.primaryErr
	}

	m := f.byEndpoint(endpoint)

	return m != nil && m.Probe.Role == patroni.RolePrimary, nil
}

func (f *fakeCluster) Config(context.Context, string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tick()

	return maps.Clone(f.config), nil
}

func (f *fakeCluster) History(context.Context, string) ([]patroni.HistoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tick()

	return slices.Clone(f.history), f.historyErr
}

func (f *fakeCluster) PatchConfig(ctx context.Context, endpoint string, patch map[string]any) (map[string]any, error) {
	f.mu.Lock()
	f.patches = append(f.patches, patchCall{endpoint: endpoint, patch: patch})
	delay := f.patchDelay
	f.mu.Unlock()

	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.tick()

	if f.patchErr != nil && !f.patchApplies {
		return nil, f.patchErr
	}

	for k, v := range patch {
		if v == nil {
			delete(f.config, k)
		} else {
			f.config[k] = v
		}
	}

	if f.onPatch != nil {
		f.onPatch(f, patch)
	}

	if f.patchErr != nil {
		return nil, f.patchErr
	}

	return maps.Clone(f.config), nil
}

func (f *fakeCluster) Switchover(ctx context.Context, endpoint, leader, candidate string) (string, error) {
	return f.post(ctx, "/switchover "+endpoint+" "+leader, candidate)
}

func (f *fakeCluster) Failover(ctx context.Context, endpoint, candidate string) (string, error) {
	return f.post(ctx, "/failover "+endpoint, candidate)
}

func (f *fakeCluster) post(ctx context.Context, path, candidate string) (string, error) {
	f.mu.Lock()
	f.tick()
	f.posts = append(f.posts, path+" "+candidate)

	refused := isRefusal(f.postErr)
	if !refused && f.onPost != nil {
		f.onPost(f, path, candidate)
	}

	hang, err, answer := f.postHang, f.postErr, f.postAnswer
	f.mu.Unlock()

	if hang {
		<-ctx.Done()

		return "", ctx.Err()
	}

	return answer, err
}

func (f *fakeCluster) patchCalls() []patchCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.patches)
}

func (f *fakeCluster) postCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.posts)
}

// fakeCatalog is the Consul catalog: every member is an instance in the datacenter of
// its host, tagged primary and passing while its own role is primary.
type fakeCatalog struct {
	mu       sync.Mutex
	clusters []*fakeCluster
	// hosts maps a host to its datacenter.
	hosts map[string]string
	// primaryOverride replaces the healthy primary hosts of a datacenter.
	primaryOverride map[string][]string
	// alsoIn registers a host in a second datacenter.
	alsoIn map[string]string
	err    error
}

func (c *fakeCatalog) HealthyAddrs(_ context.Context, dc, service, tag string) ([]string, error) {
	if service != scope || tag != "primary" {
		return nil, errors.New("unexpected query")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return nil, c.err
	}

	return addrs(c.primaries(dc)), nil
}

// ClusterHealthyAddrs returns the healthy primary instances of every datacenter.
func (c *fakeCatalog) ClusterHealthyAddrs(_ context.Context, _, service, tag string) ([]string, error) {
	if service != scope || tag != "primary" {
		return nil, errors.New("unexpected query")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return nil, c.err
	}

	dcs := slices.Collect(maps.Keys(c.primaryOverride))
	for _, dc := range c.hosts {
		dcs = append(dcs, dc)
	}

	slices.Sort(dcs)

	var hosts []string
	for _, dc := range slices.Compact(dcs) {
		hosts = append(hosts, c.primaries(dc)...)
	}

	return addrs(hosts), nil
}

// primaries returns the hosts of the healthy primary instances of dc; c.mu is held.
func (c *fakeCatalog) primaries(dc string) []string {
	if hosts, ok := c.primaryOverride[dc]; ok {
		return hosts
	}

	var hosts []string

	for _, f := range c.clusters {
		f.do(func(f *fakeCluster) {
			f.tick()

			for _, m := range f.members {
				if c.hosts[m.Host] == dc && m.Probe.Role == patroni.RolePrimary {
					hosts = append(hosts, m.Host)
				}
			}
		})
	}

	return hosts
}

func (c *fakeCatalog) RegisteredAddrs(_ context.Context, dc, service string) ([]string, error) {
	if service != scope {
		return nil, errors.New("unexpected service")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return nil, c.err
	}

	var hosts []string

	for h, d := range c.hosts {
		if d == dc || c.alsoIn[h] == dc {
			hosts = append(hosts, h)
		}
	}

	slices.Sort(hosts)

	return addrs(hosts), nil
}

func (c *fakeCatalog) setPrimary(dc string, hosts ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.primaryOverride[dc] = hosts
}

func (c *fakeCatalog) clearPrimary(dc string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.primaryOverride, dc)
}

func addrs(hosts []string) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = net.JoinHostPort(h, strconv.Itoa(5432))
	}

	return out
}

func isRefusal(err error) bool {
	if err == nil {
		return false
	}

	se, ok := errors.AsType[*source.StatusError](err)

	return ok && se.Code >= 400 && se.Code < 500
}

// refusal is a 4xx answer of Patroni as the client returns it.
func refusal(code int) error {
	return source.Classify("10.0.0.1:8008", fmt.Errorf("POST /switchover: %w: no", &source.StatusError{Code: code}))
}

// recorder collects the progress of a primitive.
type recorder struct {
	mu   sync.Mutex
	seen []pgctl.Progress
}

func (r *recorder) add(p pgctl.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seen = append(r.seen, p)
}

func (r *recorder) phases() []pgctl.Phase {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []pgctl.Phase

	for _, p := range r.seen {
		if len(out) == 0 || out[len(out)-1] != p.Phase {
			out = append(out, p.Phase)
		}
	}

	return out
}
