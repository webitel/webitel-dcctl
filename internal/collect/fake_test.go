package collect_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

const (
	interval       = 5 * time.Second
	requestTimeout = 3 * time.Second
	nodeMetaKey    = "webitel_datacenter"
)

var (
	errRefused = &source.Error{Endpoint: "10.0.0.1:1", Reason: source.ReasonRefused, Err: errors.New("connection refused")}
	leader     = consul.QueryMeta{KnownLeader: true}
)

// call is how a fake method answers: with err, after delay, or not until ctx is done.
type call struct {
	mu    sync.Mutex
	err   error
	delay time.Duration
	hang  bool
	times []time.Time
}

func (c *call) set(err error, hang bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.err, c.hang = err, hang
}

func (c *call) setDelay(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.delay = d
}

func (c *call) calls() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]time.Time(nil), c.times...)
}

func (c *call) do(ctx context.Context) error {
	c.mu.Lock()
	err, delay, hang := c.err, c.delay, c.hang
	c.times = append(c.times, time.Now())
	c.mu.Unlock()

	if hang {
		<-ctx.Done()

		return ctx.Err()
	}

	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()

		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return err
}

type fakeKV struct {
	call

	entries map[string]*consul.KVEntry
	meta    consul.QueryMeta
}

func (f *fakeKV) KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error) {
	if err := f.do(ctx); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	e, ok := f.entries[key]
	if !ok {
		return nil, f.meta, consul.ErrKeyNotFound
	}

	return e, f.meta, nil
}

type fakeArbiter struct {
	call

	records map[string]arbiter.Record
	// uninitialized makes every Get fail with arbiter.ErrUninitialized.
	uninitialized bool
}

func (f *fakeArbiter) Get(ctx context.Context, name string) (arbiter.Record, error) {
	if err := f.do(ctx); err != nil {
		return arbiter.Record{}, err
	}

	if f.uninitialized {
		return arbiter.Record{}, arbiter.ErrUninitialized
	}

	r, ok := f.records[name]
	if !ok {
		return arbiter.Record{}, arbiter.ErrNotFound
	}

	return r, nil
}

type fakeServices struct {
	call

	byName map[string][]consul.ServiceNode
	// endpoint is the server that answers.
	endpoint string

	mu    sync.Mutex
	asked []string
}

func (f *fakeServices) Instances(ctx context.Context, service, _ string, _ bool) ([]consul.ServiceNode, consul.QueryMeta, error) {
	f.mu.Lock()
	f.asked = append(f.asked, service)
	f.mu.Unlock()

	if err := f.do(ctx); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	meta := leader
	meta.Endpoint = f.endpoint

	return f.byName[service], meta, nil
}

func (f *fakeServices) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.asked...)
}

type fakeCatalog struct {
	scopeCall call
	apiCall   call

	service string

	mu    sync.Mutex
	scope []consul.ServiceNode
	api   []consul.ServiceNode
}

func (f *fakeCatalog) setScope(nodes []consul.ServiceNode) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.scope = nodes
}

func (f *fakeCatalog) ClusterInstances(ctx context.Context, service, _ string, _ bool) ([]consul.ServiceNode, consul.QueryMeta, error) {
	if err := f.scopeCall.do(ctx); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if service != f.service {
		return nil, leader, nil
	}

	return f.scope, leader, nil
}

func (f *fakeCatalog) ExecuteQuery(ctx context.Context, name string) ([]consul.ServiceNode, consul.QueryMeta, error) {
	if err := f.apiCall.do(ctx); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if name != "webitel-api" {
		return nil, leader, nil
	}

	return f.api, leader, nil
}

type fakeNomad struct {
	jobsCall    call
	nodesCall   call
	versionCall call
	allocsCall  call
	leaderCall  call

	version   string
	managed   []nomad.Job
	unmanaged []nomad.Job
	allocs    map[string][]nomad.Alloc
	nodes     []nomad.Node
	// leader is empty for a cluster without one; endpoint is the server that answers.
	leader   string
	endpoint string
}

func (f *fakeNomad) Leader(ctx context.Context) (leader, endpoint string, err error) {
	if err := f.leaderCall.do(ctx); err != nil {
		return "", "", err
	}

	return f.leader, f.endpoint, nil
}

func (f *fakeNomad) Jobs(ctx context.Context) (managed, unmanaged []nomad.Job, err error) {
	if err := f.jobsCall.do(ctx); err != nil {
		return nil, nil, err
	}

	return f.managed, f.unmanaged, nil
}

func (f *fakeNomad) Allocs(ctx context.Context, jobID string) ([]nomad.Alloc, error) {
	if err := f.allocsCall.do(ctx); err != nil {
		return nil, err
	}

	return f.allocs[jobID], nil
}

func (f *fakeNomad) Nodes(ctx context.Context) ([]nomad.Node, error) {
	if err := f.nodesCall.do(ctx); err != nil {
		return nil, err
	}

	return f.nodes, nil
}

func (f *fakeNomad) Version(ctx context.Context) (string, error) {
	if err := f.versionCall.do(ctx); err != nil {
		return "", err
	}

	return f.version, nil
}

type fakePatroni struct {
	membersCall call
	configCall  call
	historyCall call

	members []patroni.Member
	// endpoint is the seed that answers.
	endpoint string
	config   map[string]any
	history  []patroni.HistoryEntry

	mu      sync.Mutex
	histFor []string
}

func (f *fakePatroni) Discover(ctx context.Context) ([]patroni.Member, string, error) {
	if err := f.membersCall.do(ctx); err != nil {
		return nil, "", err
	}

	return f.members, f.endpoint, nil
}

func (f *fakePatroni) Config(ctx context.Context, _ string) (map[string]any, error) {
	if err := f.configCall.do(ctx); err != nil {
		return nil, err
	}

	return f.config, nil
}

func (f *fakePatroni) History(ctx context.Context, endpoint string) ([]patroni.HistoryEntry, error) {
	f.mu.Lock()
	f.histFor = append(f.histFor, endpoint)
	f.mu.Unlock()

	if err := f.historyCall.do(ctx); err != nil {
		return nil, err
	}

	return f.history, nil
}

func (f *fakePatroni) historyEndpoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.histFor...)
}

// world is the fakes of an installation of two datacenters, dc_a and dc_b.
type world struct {
	profile  config.Profile
	kv       map[string]*fakeKV
	arbiter  *fakeArbiter
	services map[string]*fakeServices
	nomad    map[string]*fakeNomad
	catalogs []*fakeCatalog
	patroni  []*fakePatroni
}

var datacenters = []string{"dc_a", "dc_b"}

func newWorld(profile config.Profile) *world {
	w := &world{
		profile:  profile,
		kv:       map[string]*fakeKV{},
		services: map[string]*fakeServices{},
		nomad:    map[string]*fakeNomad{},
	}

	for _, dc := range datacenters {
		w.services[dc] = &fakeServices{byName: map[string][]consul.ServiceNode{}}
		w.nomad[dc] = &fakeNomad{version: "1.10.0"}
	}

	voters := datacenters
	if profile == config.ProfileStretch {
		voters = datacenters[:1]
	}

	for _, dc := range voters {
		w.kv[dc] = &fakeKV{entries: map[string]*consul.KVEntry{}, meta: leader}
	}

	clusters := len(datacenters)
	if profile == config.ProfileStretch {
		clusters = 1
	}

	for range clusters {
		w.catalogs = append(w.catalogs, &fakeCatalog{service: "pg"})
		w.patroni = append(w.patroni, &fakePatroni{})
	}

	return w
}

func (w *world) sources() collect.Sources {
	var src collect.Sources

	for _, dc := range datacenters {
		if kv, ok := w.kv[dc]; ok {
			src.Voters = append(src.Voters, collect.VoterSource{Datacenter: dc, KV: kv})
		}

		src.Datacenters = append(src.Datacenters, collect.DatacenterSource{
			Name: dc, Services: w.services[dc], Nomad: w.nomad[dc],
		})
	}

	if w.arbiter != nil {
		src.Arbiter = w.arbiter
	}

	for i := range w.catalogs {
		dcs := datacenters
		if w.profile != config.ProfileStretch {
			dcs = datacenters[i : i+1]
		}

		src.Clusters = append(src.Clusters, collect.ClusterSource{
			Datacenters: dcs, Catalog: w.catalogs[i], Patroni: w.patroni[i],
		})
	}

	return src
}

func newCollector(t *testing.T, w *world) *collect.Collector {
	t.Helper()

	return collect.New(collect.Config{
		Profile:  w.profile,
		Instance: collect.Instance{Datacenter: "dc_a", Node: "dcctl-a1"},
		Status: config.Status{
			Interval:        interval,
			RequestTimeout:  requestTimeout,
			RabbitMQService: "rabbitmq-{datacenter}",
			InfoServices:    []string{"engine", "storage"},
		},
		Scope:       "pg",
		NodeMetaKey: nodeMetaKey,
	}, w.sources(), logging.New(logging.WithProvider(logtest.NewRecorder())))
}

// start runs c until the test ends.
func start(t *testing.T, c *collect.Collector) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		c.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitSeq returns the first snapshot numbered seq or later.
func waitSeq(c *collect.Collector, seq uint64) *collect.Snapshot {
	for {
		s, next := c.Watch()
		if s != nil && s.Seq >= seq {
			return s
		}

		<-next
	}
}

// deepCopy copies v and everything it points to, so that a later change of v by anyone
// shows as a difference.
func deepCopy[T any](v T) T {
	out, ok := copyValue(reflect.ValueOf(&v).Elem()).Interface().(T)
	if !ok {
		panic("deepCopy: type changed")
	}

	return out
}

func copyValue(v reflect.Value) reflect.Value {
	switch v.Kind() { //nolint:exhaustive // a value of any other kind is copied by assignment
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}

		out := reflect.New(v.Type().Elem())
		out.Elem().Set(copyValue(v.Elem()))

		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}

		out := reflect.New(v.Type()).Elem()
		out.Set(copyValue(v.Elem()))

		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}

		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(copyValue(v.Index(i)))
		}

		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}

		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), copyValue(it.Value()))
		}

		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)

		for i := range v.NumField() {
			if out.Field(i).CanSet() {
				out.Field(i).Set(copyValue(v.Field(i)))
			}
		}

		return out
	default:
		return v
	}
}
