package fencing_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/fencing"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/status"
)

const (
	dcA = "dc_a"
	// dcB is the datacenter of the instance under test.
	dcB  = "dc_b"
	node = "dcctl-b"

	interval       = 5 * time.Second
	requestTimeout = 10 * time.Second
	stopTimeout    = 120 * time.Second

	stopsMetric = "webitel.dcctl.fencing.stops"
)

var errDown = errors.New("connection refused")

// world is a two-datacenter installation with an arbiter as the instance of dc_b sees
// it: the KVs, the arbiter and Nomad of dc_b are fakes that both the snapshots and the
// fencer read.
type world struct {
	t         *testing.T
	kvs       map[string]*fakeKV
	arb       *fakeArbiter
	nomad     *fakeNomad
	collector *fakeCollector
	reader    *sdkmetric.ManualReader
	fencer    *fencing.Fencer
}

// newWorld returns an installation where dc_a is active at generation 5 and dc_b runs
// opensips (stage 3), engine (stage 2) and freeswitch (stage 1). Call it inside a
// synctest bubble.
func newWorld(t *testing.T) *world {
	t.Helper()

	w := &world{
		t:         t,
		kvs:       map[string]*fakeKV{dcA: newFakeKV(), dcB: newFakeKV()},
		arb:       &fakeArbiter{},
		nomad:     newFakeNomad(),
		collector: newFakeCollector(),
		reader:    sdkmetric.NewManualReader(),
	}

	for _, voter := range []string{dcA, dcB, coord.ArbiterName} {
		w.setState(voter, kv.State{Generation: 5, ActiveDatacenter: dcA})
	}

	w.nomad.add(nomad.Job{ID: "opensips", Type: "service", Stage: 3})
	w.nomad.add(nomad.Job{ID: "engine", Type: "service", Stage: 2})
	w.nomad.add(nomad.Job{ID: "freeswitch", Type: "system", Stage: 1})

	log := logging.New(logging.WithProvider(logtest.NewRecorder()))

	a, err := apps.New(config.Apps{
		StopTimeout: stopTimeout, StageStartTimeout: 180 * time.Second,
		FailoverStopTimeout: 30 * time.Second, MinHealthyTime: 10 * time.Second,
	}, log)
	require.NoError(t, err)

	voters := []coord.Voter{
		&kvVoter{name: dcA, kv: w.kvs[dcA]},
		&kvVoter{name: dcB, kv: w.kvs[dcB]},
		w.arb,
	}

	state, err := coord.New(voters, time.Second, log)
	require.NoError(t, err)

	ops, err := coord.NewOpStore([]coord.KV{{Name: dcA, Client: w.kvs[dcA]}, {Name: dcB, Client: w.kvs[dcB]}}, time.Second, log)
	require.NoError(t, err)

	j, err := journal.New(
		[]journal.KV{{Name: dcA, Client: w.kvs[dcA]}, {Name: dcB, Client: w.kvs[dcB]}},
		journal.Options{MaxEntries: 100, Timeout: time.Second}, log,
	)
	require.NoError(t, err)

	w.fencer, err = fencing.New(fencing.Config{
		Datacenter:     apps.Datacenter{Name: dcB, Nomad: w.nomad},
		Status:         status.Config{Interval: interval, MinHealthyTime: 10 * time.Second},
		RequestTimeout: requestTimeout,
	}, fencing.Deps{
		Snapshots:     w.collector,
		Apps:          a,
		State:         state,
		Ops:           ops,
		Journal:       j,
		MeterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(w.reader)),
	}, log)
	require.NoError(t, err)

	return w
}

func stateValue(s kv.State) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return b
}

// setState writes dcctl/state of voter, a datacenter or the arbiter.
func (w *world) setState(voter string, s kv.State) {
	if voter == coord.ArbiterName {
		w.arb.set(stateValue(s))

		return
	}

	w.kvs[voter].put(kv.StateKey, stateValue(s))
}

// setOp writes an operation in state to dcctl/op of the KV of dc.
func (w *world) setOp(dc string, state dcctlv1.Operation_State) {
	b, err := kv.EncodeProto(&dcctlv1.Operation{
		Name: "operations/op-1", State: state,
		CreateTime: timestamppb.New(time.Now()), UpdateTime: timestamppb.New(time.Now()),
		Leader: &dcctlv1.Instance{Datacenter: dcA},
	})
	require.NoError(w.t, err)

	w.kvs[dc].put(kv.OpKey, b)
}

// publish collects a snapshot from the fakes, lets edit change it and publishes it.
func (w *world) publish(edit func(*collect.Snapshot)) {
	now := time.Now()
	s := &collect.Snapshot{
		ObserveTime: now,
		Observer:    collect.Instance{Datacenter: dcB, Node: node},
		Profile:     config.ProfileWarmStandby,
		Voters: []collect.Voter{
			{Datacenter: dcA, KV: w.kvs[dcA].read(now)},
			{Datacenter: dcB, KV: w.kvs[dcB].read(now)},
		},
		Arbiter: new(w.arb.read(now)),
		Datacenters: []collect.Datacenter{
			{Name: dcA, Nomad: collect.Source[collect.Nomad]{Reachable: true, ObserveTime: now}},
			{Name: dcB, Nomad: collect.Source[collect.Nomad]{Reachable: true, Data: w.nomad.collect(), ObserveTime: now}},
		},
	}

	if edit != nil {
		edit(s)
	}

	w.collector.publish(s)
}

// start runs the fencer until the returned stop is called, which waits for it.
func (w *world) start() (stop func()) {
	ctx, cancel := context.WithCancel(w.t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		w.fencer.Run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}

// settle lets a stop run to its end, a timeout included.
func settle() {
	time.Sleep(stopTimeout + time.Minute)
	synctest.Wait()
}

// entries returns the journal entries in every KV, by name.
func (w *world) entries() []*dcctlv1.JournalEntry {
	byName := make(map[string]*dcctlv1.JournalEntry)

	for _, k := range w.kvs {
		for _, e := range k.list(kv.JournalPrefix) {
			entry := new(dcctlv1.JournalEntry)
			require.NoError(w.t, kv.DecodeProto(e.Value, entry))

			byName[entry.GetName()] = entry
		}
	}

	return slices.SortedFunc(maps.Values(byName), func(a, b *dcctlv1.JournalEntry) int {
		return strings.Compare(a.GetName(), b.GetName())
	})
}

// stops returns the data points of the fencing stops counter.
func (w *world) stops() []metricdata.DataPoint[int64] {
	var rm metricdata.ResourceMetrics
	require.NoError(w.t, w.reader.Collect(w.t.Context(), &rm))

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == stopsMetric {
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(w.t, ok, "%s is %T", stopsMetric, m.Data)

				return sum.DataPoints
			}
		}
	}

	return nil
}

// fakeCollector publishes the snapshots a test builds.
type fakeCollector struct {
	mu   sync.Mutex
	seq  uint64
	snap *collect.Snapshot
	next chan struct{}
}

func newFakeCollector() *fakeCollector {
	return &fakeCollector{next: make(chan struct{})}
}

func (c *fakeCollector) Watch() (*collect.Snapshot, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snap, c.next
}

func (c *fakeCollector) publish(s *collect.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	s.Seq = c.seq
	c.snap = s
	close(c.next)
	c.next = make(chan struct{})
}

// fakeKV is the Consul KV of one datacenter: the voter, the operation record and the
// journal share it.
type fakeKV struct {
	// KVClient is nil: the lock is not used.
	coord.KVClient

	mu      sync.Mutex
	down    bool
	index   uint64
	entries map[string]consul.KVEntry
}

func newFakeKV() *fakeKV {
	return &fakeKV{entries: make(map[string]consul.KVEntry)}
}

func (k *fakeKV) put(key string, value []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.index++
	k.entries[key] = consul.KVEntry{Key: key, Value: value, ModifyIndex: k.index}
}

func (k *fakeKV) setDown(down bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.down = down
}

func (k *fakeKV) get(key string) *consul.KVEntry {
	e, ok := k.entries[key]
	if !ok {
		return nil
	}

	return &e
}

// read is the KV as the collector reads it at now.
func (k *fakeKV) read(now time.Time) collect.Source[collect.KV] {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.down {
		return collect.Source[collect.KV]{
			Reason: source.ReasonRefused, Err: errDown, UnreachableSince: now,
		}
	}

	return collect.Source[collect.KV]{
		Reachable: true,
		Data: collect.KV{
			State: k.get(kv.StateKey), Op: k.get(kv.OpKey), Meta: consul.QueryMeta{KnownLeader: true},
		},
		ObserveTime: now,
	}
}

func (k *fakeKV) list(prefix string) []consul.KVEntry {
	k.mu.Lock()
	defer k.mu.Unlock()

	var out []consul.KVEntry

	for key, e := range k.entries {
		if strings.HasPrefix(key, prefix) {
			out = append(out, e)
		}
	}

	slices.SortFunc(out, func(a, b consul.KVEntry) int { return cmp.Compare(a.Key, b.Key) })

	return out
}

func (k *fakeKV) KVGet(_ context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.down {
		return nil, consul.QueryMeta{}, errDown
	}

	e := k.get(key)
	if e == nil {
		return nil, consul.QueryMeta{KnownLeader: true}, consul.ErrKeyNotFound
	}

	return e, consul.QueryMeta{KnownLeader: true}, nil
}

func (k *fakeKV) KVList(_ context.Context, prefix string) ([]consul.KVEntry, error) {
	if k.isDown() {
		return nil, errDown
	}

	return k.list(prefix), nil
}

func (k *fakeKV) KVCAS(_ context.Context, key string, value []byte, modifyIndex uint64) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.down {
		return false, errDown
	}

	cur, exists := k.entries[key]
	if (modifyIndex == 0 && exists) || (modifyIndex != 0 && cur.ModifyIndex != modifyIndex) {
		return false, nil
	}

	k.index++
	k.entries[key] = consul.KVEntry{Key: key, Value: value, ModifyIndex: k.index}

	return true, nil
}

func (k *fakeKV) KVDeleteCAS(_ context.Context, key string, modifyIndex uint64) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.down {
		return false, errDown
	}

	if cur, ok := k.entries[key]; !ok || cur.ModifyIndex != modifyIndex {
		return false, nil
	}

	delete(k.entries, key)

	return true, nil
}

func (k *fakeKV) isDown() bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.down
}

// kvVoter is the voter over a fake KV.
type kvVoter struct {
	name string
	kv   *fakeKV
}

func (v *kvVoter) Name() string { return v.name }

func (v *kvVoter) Kind() coord.Kind { return coord.KindKV }

func (v *kvVoter) Get(ctx context.Context, key coord.Key) (coord.Entry, error) {
	if key != coord.KeyState {
		return coord.Entry{}, errors.New("only the state is read")
	}

	e, _, err := v.kv.KVGet(ctx, kv.StateKey)

	switch {
	case errors.Is(err, consul.ErrKeyNotFound):
		return coord.Entry{}, coord.ErrNotFound
	case err != nil:
		return coord.Entry{}, err
	}

	return coord.Entry{Value: e.Value, Version: coord.Version(e.ModifyIndex)}, nil
}

func (v *kvVoter) CAS(context.Context, coord.Key, []byte, coord.Version) error {
	return errors.New("fencing never writes a voter")
}

// fakeArbiter is the arbiter voter; without a state it is uninitialized.
type fakeArbiter struct {
	mu    sync.Mutex
	down  bool
	state []byte
}

func (a *fakeArbiter) set(state []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = state
}

func (a *fakeArbiter) setDown(down bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.down = down
}

func (a *fakeArbiter) read(now time.Time) collect.Source[collect.Arbiter] {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case a.down:
		return collect.Source[collect.Arbiter]{Reason: source.ReasonRefused, Err: errDown, UnreachableSince: now}
	case a.state == nil:
		return collect.Source[collect.Arbiter]{Reachable: true, ObserveTime: now}
	}

	return collect.Source[collect.Arbiter]{
		Reachable:   true,
		Data:        collect.Arbiter{Initialized: true, State: &arbiter.Record{Value: a.state, Etag: 1}},
		ObserveTime: now,
	}
}

func (a *fakeArbiter) Name() string { return coord.ArbiterName }

func (a *fakeArbiter) Kind() coord.Kind { return coord.KindArbiter }

func (a *fakeArbiter) Get(_ context.Context, key coord.Key) (coord.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case key != coord.KeyState:
		return coord.Entry{}, errors.New("only the state is read")
	case a.down:
		return coord.Entry{}, errDown
	case a.state == nil:
		return coord.Entry{}, coord.ErrUninitialized
	}

	return coord.Entry{Value: a.state, Version: 1}, nil
}

func (a *fakeArbiter) CAS(context.Context, coord.Key, []byte, coord.Version) error {
	return errors.New("fencing never writes a voter")
}

// fakeNomad is the Nomad of dc_b. A stopped job loses its allocation at once unless it
// is stuck or lost.
type fakeNomad struct {
	mu     sync.Mutex
	jobs   map[string]*nomad.Job
	allocs map[string][]nomad.Alloc
	// stuck jobs keep their allocation running after a stop.
	stuck map[string]bool
	// lost jobs are on a node that stops answering: their allocation turns unknown
	// after a stop.
	lost map[string]bool
	// block, when set, holds every stop until it is closed.
	block chan struct{}
	// onStop runs after a job is stopped, outside the lock.
	onStop func(job string)

	calls       []string
	inFlight    int
	maxInFlight int
}

func newFakeNomad() *fakeNomad {
	return &fakeNomad{
		jobs:   make(map[string]*nomad.Job),
		allocs: make(map[string][]nomad.Alloc),
		stuck:  make(map[string]bool),
		lost:   make(map[string]bool),
	}
}

func runningAlloc(job string) nomad.Alloc {
	return nomad.Alloc{
		ID: job + "-1", JobID: job, NodeID: "n1", ClientStatus: "running", DesiredStatus: "run",
		TaskStates: map[string]nomad.TaskState{"main": {State: "running", StartedAt: time.Now().Add(-time.Hour)}},
	}
}

// add registers job, running unless it is stopped.
func (n *fakeNomad) add(job nomad.Job) {
	n.mu.Lock()
	defer n.mu.Unlock()

	job.Name = job.ID
	n.jobs[job.ID] = &job

	n.allocs[job.ID] = nil
	if !job.Stop {
		n.allocs[job.ID] = []nomad.Alloc{runningAlloc(job.ID)}
	}
}

// restart starts job again, as Ansible or an operator would.
func (n *fakeNomad) restart(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.jobs[id].Stop = false
	n.allocs[id] = []nomad.Alloc{runningAlloc(id)}
}

func (n *fakeNomad) stopped(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.jobs[id].Stop
}

func (n *fakeNomad) stopCalls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return slices.Clone(n.calls)
}

// collect is what the collector reads from this Nomad.
func (n *fakeNomad) collect() collect.Nomad {
	jobs, _, err := n.Jobs(context.Background())
	if err != nil {
		panic(err)
	}

	out := collect.Nomad{Version: "1.9.0", Managed: jobs, Allocs: make(map[string][]nomad.Alloc)}

	for _, j := range jobs {
		allocs, err := n.Allocs(context.Background(), j.ID)
		if err != nil {
			panic(err)
		}

		out.Allocs[j.ID] = allocs
	}

	return out
}

func (n *fakeNomad) Jobs(context.Context) (managed, unmanaged []nomad.Job, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, j := range n.jobs {
		managed = append(managed, *j)
	}

	slices.SortFunc(managed, func(a, b nomad.Job) int { return strings.Compare(a.ID, b.ID) })

	return managed, nil, nil
}

func (n *fakeNomad) Allocs(_ context.Context, jobID string) ([]nomad.Alloc, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return slices.Clone(n.allocs[jobID]), nil
}

func (n *fakeNomad) Nodes(context.Context) ([]nomad.Node, error) {
	return nil, nil
}

func (n *fakeNomad) Stop(ctx context.Context, jobID string) error {
	n.mu.Lock()
	n.calls = append(n.calls, jobID)
	n.inFlight++
	n.maxInFlight = max(n.maxInFlight, n.inFlight)
	block := n.block
	n.mu.Unlock()

	defer func() {
		n.mu.Lock()
		n.inFlight--
		n.mu.Unlock()
	}()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	n.mu.Lock()
	n.jobs[jobID].Stop = true

	switch {
	case n.lost[jobID]:
		for i := range n.allocs[jobID] {
			n.allocs[jobID][i].ClientStatus = "unknown"
		}
	case !n.stuck[jobID]:
		n.allocs[jobID] = nil
	}

	hook := n.onStop
	n.mu.Unlock()

	if hook != nil {
		hook(jobID)
	}

	return nil
}

func (n *fakeNomad) Start(context.Context, string, func(context.Context) error) error {
	return errors.New("fencing never starts a job")
}
