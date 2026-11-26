package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

type fakeKey struct {
	value   []byte
	index   uint64
	session string
}

// fakeConsul is the KV and the sessions of one Consul, shared by every
// instance that talks to it.
type fakeConsul struct {
	mu       sync.Mutex
	index    uint64
	keys     map[string]*fakeKey
	sessions map[string]bool
	nextID   int
	// down makes every call fail as an unreachable Consul.
	down bool
	// onGet runs at the start of KVGet, outside the lock, as another instance
	// would write meanwhile; an error fails the read.
	onGet func(key string) error
	// casErr fails a CAS of key when it returns an error.
	casErr func(key string) error
}

func newFakeConsul() *fakeConsul {
	return &fakeConsul{index: 100, keys: map[string]*fakeKey{}, sessions: map[string]bool{}}
}

func unreachable() error {
	return source.Classify("fake:8500", fmt.Errorf("dial: %w", syscall.ECONNREFUSED))
}

func (f *fakeConsul) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if f.down {
		return unreachable()
	}

	return nil
}

func (f *fakeConsul) set(fn func(f *fakeConsul)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

func (f *fakeConsul) CreateSession(ctx context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return "", err
	}

	f.nextID++
	id := fmt.Sprintf("session-%d", f.nextID)
	f.sessions[id] = true

	return id, nil
}

func (f *fakeConsul) RenewSession(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return err
	}

	if !f.sessions[id] {
		return consul.ErrSessionNotFound
	}

	return nil
}

func (f *fakeConsul) DestroySession(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return err
	}

	f.invalidate(id)

	return nil
}

// expireAll ends every session as their TTL would.
func (f *fakeConsul) expireAll() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for id := range f.sessions {
		f.invalidate(id)
	}
}

func (f *fakeConsul) invalidate(id string) {
	delete(f.sessions, id)

	for _, k := range f.keys {
		if k.session == id {
			k.session = ""
			f.index++
			k.index = f.index
		}
	}
}

func (f *fakeConsul) Acquire(ctx context.Context, key string, value []byte, session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return false, err
	}

	if !f.sessions[session] {
		return false, fmt.Errorf("%w: invalid session", consul.ErrSessionNotFound)
	}

	if k := f.keys[key]; k != nil && k.session != "" && k.session != session {
		return false, nil
	}

	f.put(key, value, session)

	return true, nil
}

func (f *fakeConsul) Release(ctx context.Context, key string, value []byte, session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return false, err
	}

	if !f.sessions[session] {
		return false, fmt.Errorf("%w: invalid session", consul.ErrSessionNotFound)
	}

	k := f.keys[key]
	if k == nil || k.session != session {
		return false, nil
	}

	f.put(key, value, "")

	return true, nil
}

func (f *fakeConsul) KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error) {
	f.mu.Lock()
	hook := f.onGet
	f.mu.Unlock()

	if hook != nil {
		if err := hook(key); err != nil {
			return nil, consul.QueryMeta{}, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	k := f.keys[key]
	if k == nil {
		return nil, consul.QueryMeta{KnownLeader: true}, consul.ErrKeyNotFound
	}

	return &consul.KVEntry{Key: key, Value: slices.Clone(k.value), ModifyIndex: k.index, Session: k.session},
		consul.QueryMeta{KnownLeader: true}, nil
}

func (f *fakeConsul) KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check(ctx); err != nil {
		return false, err
	}

	if f.casErr != nil {
		if err := f.casErr(key); err != nil {
			return false, err
		}
	}

	var current uint64
	if k := f.keys[key]; k != nil {
		current = k.index
	}

	if current != modifyIndex {
		return false, nil
	}

	session := ""
	if k := f.keys[key]; k != nil {
		session = k.session
	}

	f.put(key, value, session)

	return true, nil
}

func (f *fakeConsul) put(key string, value []byte, session string) {
	f.index++
	f.keys[key] = &fakeKey{value: slices.Clone(value), index: f.index, session: session}
}

// holdLock makes a live session of another instance hold dcctl/lock.
func (f *fakeConsul) holdLock(t *testing.T, holder kv.Lock) {
	t.Helper()

	id, err := f.CreateSession(t.Context(), "other")
	require.NoError(t, err)

	ok, err := f.Acquire(t.Context(), kv.LockKey, []byte(fmt.Sprintf(`{"instance":%q,"operation":%q}`, holder.Instance, holder.Operation)), id)
	require.NoError(t, err)
	require.True(t, ok)
}

// lockHolder returns the session that holds dcctl/lock, empty when free.
func (f *fakeConsul) lockHolder() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if k := f.keys[kv.LockKey]; k != nil {
		return k.session
	}

	return ""
}

// setOp writes op to dcctl/op as another instance would.
func (f *fakeConsul) setOp(t *testing.T, op *dcctlv1.Operation) {
	t.Helper()

	b, err := kv.EncodeProto(op)
	require.NoError(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.put(kv.OpKey, b, "")
}

// op returns the operation in dcctl/op, nil when there is none.
func (f *fakeConsul) op(t *testing.T) *dcctlv1.Operation {
	t.Helper()

	f.mu.Lock()
	k := f.keys[kv.OpKey]
	f.mu.Unlock()

	if k == nil {
		return nil
	}

	op := new(dcctlv1.Operation)
	require.NoError(t, kv.DecodeProto(k.value, op))

	return op
}

// fakeVoter holds dcctl/state as a voter does.
type fakeVoter struct {
	name string
	kind coord.Kind

	mu      sync.Mutex
	value   []byte
	version coord.Version
	getErr  error
	casErr  error
}

func newVoter(t *testing.T, name string, kind coord.Kind, s kv.State) *fakeVoter {
	t.Helper()

	b, err := kv.EncodeState(s, nil)
	require.NoError(t, err)

	return &fakeVoter{name: name, kind: kind, value: b, version: 10}
}

func (f *fakeVoter) Name() string { return f.name }

func (f *fakeVoter) Kind() coord.Kind { return f.kind }

func (f *fakeVoter) Get(ctx context.Context, _ coord.Key) (coord.Entry, error) {
	if err := ctx.Err(); err != nil {
		return coord.Entry{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.getErr != nil:
		return coord.Entry{}, f.getErr
	case f.value == nil:
		return coord.Entry{}, coord.ErrNotFound
	}

	return coord.Entry{Value: f.value, Version: f.version}, nil
}

func (f *fakeVoter) CAS(ctx context.Context, _ coord.Key, value []byte, version coord.Version) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.getErr != nil:
		return f.getErr
	case f.casErr != nil:
		return f.casErr
	case version != f.version:
		return coord.ErrVersionMismatch
	}

	f.value = value
	f.version++

	return nil
}

func (f *fakeVoter) state(t *testing.T) kv.State {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	var s kv.State
	require.NoError(t, json.Unmarshal(f.value, &s))

	return s
}

func (f *fakeVoter) setUnreachable() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getErr = unreachable()
}

// fakeJournal keeps what is journaled.
type fakeJournal struct {
	mu      sync.Mutex
	entries []*dcctlv1.JournalEntry
}

func (f *fakeJournal) Append(_ context.Context, e *dcctlv1.JournalEntry) (journal.Appended, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries = append(f.entries, e)

	return journal.Appended{Entry: e, Writes: []journal.Write{{DC: "dc_a", Outcome: journal.Written}}}, nil
}

func (f *fakeJournal) system(action dcctlv1.JournalEntry_SystemAction) []*dcctlv1.JournalEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*dcctlv1.JournalEntry

	for _, e := range f.entries {
		if e.GetSystemAction() == action {
			out = append(out, e)
		}
	}

	return out
}

// events returns params.event of the entries of type t.
func (f *fakeJournal) events(t dcctlv1.JournalEntry_Type) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string

	for _, e := range f.entries {
		if e.GetType() == t {
			out = append(out, e.GetParams().GetFields()["event"].GetStringValue())
		}
	}

	return out
}

// event returns the entries of type OPERATION with params.event.
func (f *fakeJournal) event(event string) []*dcctlv1.JournalEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*dcctlv1.JournalEntry

	for _, e := range f.entries {
		if e.GetType() == dcctlv1.JournalEntry_OPERATION && e.GetParams().GetFields()["event"].GetStringValue() == event {
			out = append(out, e)
		}
	}

	return out
}

// results returns the results of the entries of type t.
func (f *fakeJournal) results(t dcctlv1.JournalEntry_Type) []dcctlv1.JournalEntry_Result {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []dcctlv1.JournalEntry_Result

	for _, e := range f.entries {
		if e.GetType() == t {
			out = append(out, e.GetResult())
		}
	}

	return out
}

// fakePreflight answers every action with its verdict.
type fakePreflight struct {
	mu       sync.Mutex
	verdict  check.Verdict
	requests []preflight.Request
}

func (f *fakePreflight) Run(_ context.Context, req preflight.Request) (check.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, req)

	return f.verdict, nil
}

// fakeRecorder keeps what is recorded.
type fakeRecorder struct {
	mu    sync.Mutex
	ops   []dcctlv1.Operation_State
	steps []dcctlv1.Operation_Step_State
}

func (f *fakeRecorder) RecordOperation(_ context.Context, op *dcctlv1.Operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ops = append(f.ops, op.GetState())

	return nil
}

func (f *fakeRecorder) RecordStep(_ context.Context, _ dcctlv1.Operation_Type, step *dcctlv1.Operation_Step) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.steps = append(f.steps, step.GetState())

	return nil
}

func (f *fakeRecorder) operations() []dcctlv1.Operation_State {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.ops)
}

var self = operation.Instance{Node: "node-b", Datacenter: "dc_b", Address: "10.0.0.2:10023"}

// world is two datacenters with their Consul KVs, three voters and the fakes an
// engine runs on.
type world struct {
	t         *testing.T
	consuls   map[string]*fakeConsul
	voters    []*fakeVoter
	journal   *fakeJournal
	preflight *fakePreflight
	recorder  *fakeRecorder
	resumers  map[dcctlv1.Operation_Type]operation.Resumer
}

func newWorld(t *testing.T) *world {
	t.Helper()

	base := kv.State{Generation: 5, ActiveDatacenter: "dc_a", OpID: "old"}

	return &world{
		t:       t,
		consuls: map[string]*fakeConsul{"dc_a": newFakeConsul(), "dc_b": newFakeConsul()},
		voters: []*fakeVoter{
			newVoter(t, "dc_a", coord.KindKV, base),
			newVoter(t, "dc_b", coord.KindKV, base),
			newVoter(t, coord.ArbiterName, coord.KindArbiter, base),
		},
		journal:   &fakeJournal{},
		preflight: &fakePreflight{verdict: check.Verdict{Outcome: check.Allowed}},
		recorder:  &fakeRecorder{},
		resumers:  map[dcctlv1.Operation_Type]operation.Resumer{},
	}
}

func quietLog() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

// engine returns an engine of instance s on w.
func (w *world) engine(s operation.Instance) *operation.Engine {
	w.t.Helper()

	kvs := []coord.KV{{Name: "dc_a", Client: w.consuls["dc_a"]}, {Name: "dc_b", Client: w.consuls["dc_b"]}}

	locker, err := coord.NewLocker(kvs, coord.LockOptions{TTL: 15 * time.Second, LockDelay: 15 * time.Second, Timeout: 3 * time.Second}, quietLog())
	require.NoError(w.t, err)

	ops, err := coord.NewOpStore(kvs, 3*time.Second, quietLog())
	require.NoError(w.t, err)

	voters := make([]coord.Voter, 0, len(w.voters))
	for _, v := range w.voters {
		voters = append(voters, v)
	}

	c, err := coord.New(voters, 3*time.Second, quietLog())
	require.NoError(w.t, err)

	e, err := operation.New(operation.Config{
		Self:        s,
		Locker:      locker,
		Ops:         ops,
		Coordinator: c,
		Preflight:   w.preflight,
		Journal:     w.journal,
		Recorder:    w.recorder,
		Resumers:    w.resumers,
		Interval:    15 * time.Second,
		Log:         quietLog(),
	})
	require.NoError(w.t, err)

	return e
}

// start runs e until the returned stop, which waits for Run to return.
func start(t *testing.T, e *operation.Engine) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- e.Run(ctx) }()

	return func() {
		cancel()
		require.NoError(t, <-done)
	}
}

// lockFree reports whether no session holds dcctl/lock in any KV.
func (w *world) lockFree() bool {
	for _, c := range w.consuls {
		if c.lockHolder() != "" {
			return false
		}
	}

	return true
}

// op returns the operation in the KV of dc.
func (w *world) op(dc string) *dcctlv1.Operation {
	w.t.Helper()

	return w.consuls[dc].op(w.t)
}

// setOp writes op to the KV of every datacenter.
func (w *world) setOp(op *dcctlv1.Operation) {
	w.t.Helper()

	for _, c := range w.consuls {
		c.setOp(w.t, op)
	}
}

// selfLeader is [self] as the leader of an operation.
func selfLeader() *dcctlv1.Instance {
	return &dcctlv1.Instance{Node: self.Node, Datacenter: "datacenters/" + self.Datacenter, Address: self.Address}
}

// interruptedOp is a switch to dc_b led by node-x of dc_a and interrupted in
// its second step; the third step is the point of no return.
func interruptedOp(id string, revision int64) *dcctlv1.Operation {
	created := timestamppb.New(time.Now().Add(-time.Minute))

	return &dcctlv1.Operation{
		Name:               "operations/" + id,
		Type:               dcctlv1.Operation_SWITCH,
		State:              dcctlv1.Operation_INTERRUPTED,
		Target:             "datacenters/dc_b",
		Initiator:          &dcctlv1.Initiator{User: "admin"},
		Leader:             &dcctlv1.Instance{Node: "node-x", Datacenter: "datacenters/dc_a"},
		Mode:               dcctlv1.Operation_QUORUM,
		PreviousGeneration: 5,
		NewGeneration:      6,
		Steps: []*dcctlv1.Operation_Step{
			{Index: 0, Type: dcctlv1.Operation_Step_APPS_STOP, Stage: 1, State: succeeded},
			{Index: 1, Type: dcctlv1.Operation_Step_PATRONI_DEMOTE, State: failed},
			{Index: 2, Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, State: pending, PointOfNoReturn: true},
		},
		CreateTime: created,
		UpdateTime: created,
		Revision:   revision,
	}
}

func runningOp(id string, revision int64) *dcctlv1.Operation {
	op := interruptedOp(id, revision)
	op.State = dcctlv1.Operation_RUNNING
	op.Steps[1].State = dcctlv1.Operation_Step_RUNNING

	return op
}

// statusOf returns the gRPC status the API answers err with.
func statusOf(err error) *status.Status {
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		return gs.GRPCStatus()
	}

	return status.New(codes.Unknown, err.Error())
}

// reasonOf returns the reason of the ErrorInfo of err, empty without one.
func reasonOf(err error) string {
	for _, d := range statusOf(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}

	return ""
}
