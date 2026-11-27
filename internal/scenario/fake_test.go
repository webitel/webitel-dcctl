package scenario_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

func quietLog() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

func unreachable() error {
	return source.Classify("fake:8500", fmt.Errorf("dial: %w", syscall.ECONNREFUSED))
}

type fakeKey struct {
	value   []byte
	index   uint64
	session string
}

// fakeConsul is the KV and the sessions of one Consul as dcctl/lock and dcctl/op use
// them.
type fakeConsul struct {
	mu       sync.Mutex
	index    uint64
	keys     map[string]*fakeKey
	sessions map[string]bool
	nextID   int
}

func newFakeConsul() *fakeConsul {
	return &fakeConsul{index: 100, keys: map[string]*fakeKey{}, sessions: map[string]bool{}}
}

func (f *fakeConsul) CreateSession(ctx context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
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

	if err := ctx.Err(); err != nil {
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

	if err := ctx.Err(); err != nil {
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

	if err := ctx.Err(); err != nil {
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

	if err := ctx.Err(); err != nil {
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
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
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

	if err := ctx.Err(); err != nil {
		return false, err
	}

	var (
		current uint64
		session string
	)

	if k := f.keys[key]; k != nil {
		current, session = k.index, k.session
	}

	if current != modifyIndex {
		return false, nil
	}

	f.put(key, value, session)

	return true, nil
}

func (f *fakeConsul) put(key string, value []byte, session string) {
	f.index++
	f.keys[key] = &fakeKey{value: slices.Clone(value), index: f.index, session: session}
}

func (f *fakeConsul) lockHolder() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if k := f.keys[kv.LockKey]; k != nil {
		return k.session
	}

	return ""
}

// rawOp returns dcctl/op as it is stored.
func (f *fakeConsul) rawOp() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	if k := f.keys[kv.OpKey]; k != nil {
		return slices.Clone(k.value)
	}

	return nil
}

// setRawOp writes dcctl/op as it was stored, as before a crash.
func (f *fakeConsul) setRawOp(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.put(kv.OpKey, b, "")
}

func (f *fakeConsul) op(t *testing.T) *dcctlv1.Operation {
	t.Helper()

	b := f.rawOp()
	if b == nil {
		return nil
	}

	op := new(dcctlv1.Operation)
	require.NoError(t, kv.DecodeProto(b, op))

	return op
}

type voterEntry struct {
	value   []byte
	version coord.Version
}

// fakeVoter holds dcctl/state and dcctl/lease as a voter does.
type fakeVoter struct {
	name string
	kind coord.Kind

	mu      sync.Mutex
	entries map[coord.Key]*voterEntry
	next    coord.Version
	getErr  error
	casErr  error
	// beforeCAS runs under the lock before a CAS of dcctl/state with the value sent, as
	// another writer that gets there first.
	beforeCAS func(v *fakeVoter, value []byte)
	// onGet runs under the lock before a read of dcctl/state, as another writer between
	// two reads.
	onGet func(v *fakeVoter)
}

func newVoter(t *testing.T, name string, kind coord.Kind, s kv.State) *fakeVoter {
	t.Helper()

	v := &fakeVoter{name: name, kind: kind, entries: map[coord.Key]*voterEntry{}, next: 10}
	v.setState(t, s)

	return v
}

func (v *fakeVoter) Name() string { return v.name }

func (v *fakeVoter) Kind() coord.Kind { return v.kind }

func (v *fakeVoter) Get(ctx context.Context, key coord.Key) (coord.Entry, error) {
	if err := ctx.Err(); err != nil {
		return coord.Entry{}, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.getErr != nil {
		return coord.Entry{}, v.getErr
	}

	if key == coord.KeyState && v.onGet != nil {
		v.onGet(v)
	}

	e := v.entries[key]
	if e == nil {
		return coord.Entry{}, coord.ErrNotFound
	}

	return coord.Entry{Value: slices.Clone(e.value), Version: e.version}, nil
}

func (v *fakeVoter) CAS(ctx context.Context, key coord.Key, value []byte, version coord.Version) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	switch {
	case v.getErr != nil:
		return v.getErr
	case v.casErr != nil && key == coord.KeyState:
		return v.casErr
	}

	if key == coord.KeyState && v.beforeCAS != nil {
		v.beforeCAS(v, value)
	}

	var current coord.Version
	if e := v.entries[key]; e != nil {
		current = e.version
	}

	if current != version {
		return coord.ErrVersionMismatch
	}

	v.store(key, value)

	return nil
}

func (v *fakeVoter) store(key coord.Key, value []byte) {
	v.next++
	v.entries[key] = &voterEntry{value: slices.Clone(value), version: v.next}
}

func (v *fakeVoter) set(fn func(v *fakeVoter)) {
	v.mu.Lock()
	defer v.mu.Unlock()

	fn(v)
}

func (v *fakeVoter) setState(t *testing.T, s kv.State) {
	t.Helper()

	b, err := kv.EncodeState(s, nil)
	require.NoError(t, err)

	v.set(func(v *fakeVoter) { v.store(coord.KeyState, b) })
}

func (v *fakeVoter) state(t *testing.T) kv.State {
	t.Helper()

	v.mu.Lock()
	defer v.mu.Unlock()

	var s kv.State
	require.NoError(t, json.Unmarshal(v.entries[coord.KeyState].value, &s))

	return s
}

func (v *fakeVoter) lease(t *testing.T) *kv.Lease {
	t.Helper()

	v.mu.Lock()
	defer v.mu.Unlock()

	e := v.entries[coord.KeyLease]
	if e == nil {
		return nil
	}

	var l kv.Lease
	require.NoError(t, json.Unmarshal(e.value, &l))

	return &l
}

type fakeJournal struct {
	mu      sync.Mutex
	entries []*dcctlv1.JournalEntry
}

func (f *fakeJournal) Append(_ context.Context, e *dcctlv1.JournalEntry) (journal.Appended, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries = append(f.entries, e)

	return journal.Appended{Entry: e, Writes: []journal.Write{{DC: dcA, Outcome: journal.Written}}}, nil
}

// fakePreflight decides the confirmations as the sets of warm_standby do, on the
// infrastructure of w: a failover always needs the isolation confirmed, and not_max_lsn
// when a standby other than dc_a, the datacenter the tests fail over from, received
// more WAL than the target or an amount that is not known. A resume needs the
// isolation when it writes forced; one of a failover not promoted yet, op as stored,
// needs both anew against the standbys the failover repoints. A switch takes none.
type fakePreflight struct {
	w  *infra
	op func() *dcctlv1.Operation
	// view reads the voters, for the actions on a standby.
	view func(ctx context.Context) (coord.View, error)
	// stretch decides as the sets of stretch do, see stretchChecks.
	stretch bool
}

func (f fakePreflight) Run(ctx context.Context, req preflight.Request) (check.Verdict, error) {
	if f.stretch {
		return f.stretchChecks(ctx, req)
	}

	var cs []check.Check

	switch req.Action {
	case preflight.Switch:
	case preflight.Failover:
		var others []string

		for _, name := range f.w.names() {
			if name != req.Params.Datacenter && name != dcA {
				others = append(others, name)
			}
		}

		cs = append(cs, isolationCheck(), f.maxLSN(req.Params.Datacenter, others), f.sourceDiscard(dcA, req.Params.Datacenter))
	case preflight.Resume:
		source, target, others, unpromoted := unpromotedFailover(f.op())
		if req.Params.ToForced || unpromoted {
			cs = append(cs, isolationCheck())
		}

		if unpromoted {
			cs = append(cs, f.maxLSN(target, others), f.sourceDiscard(source, target))
		}

		more, err := f.resumeStandby(ctx, f.op())
		if err != nil {
			return check.Verdict{}, err
		}

		cs = append(cs, more...)
	case preflight.Demote, preflight.Repoint:
		more, err := f.standbyChecks(ctx, req.Action, req.Params.Datacenter)
		if err != nil {
			return check.Verdict{}, err
		}

		cs = append(cs, more...)
	case preflight.MarkRecovered, preflight.Abandon, preflight.Resolve,
		preflight.Init, preflight.ArbiterInit, preflight.AppsSync, preflight.Verify:
		return check.Verdict{}, fmt.Errorf("unexpected action %s", req.Action)
	}

	return check.Decide(cs, req.Confirmations), nil
}

// unpromotedFailover returns the source and the target of op and the standbys it
// repoints when op is a failover whose promotion did not succeed.
func unpromotedFailover(op *dcctlv1.Operation) (source, target string, others []string, ok bool) {
	if op.GetType() != dcctlv1.Operation_FAILOVER {
		return "", "", nil, false
	}

	ok = true

	for _, st := range op.GetSteps() {
		if st.GetType() == dcctlv1.Operation_Step_PATRONI_REPOINT {
			others = append(others, strings.TrimPrefix(st.GetDatacenter(), "datacenters/"))
		}

		if st.GetType() == dcctlv1.Operation_Step_PATRONI_DEMOTE {
			source = strings.TrimPrefix(st.GetDatacenter(), "datacenters/")
		}

		if st.GetType() == dcctlv1.Operation_Step_PATRONI_PROMOTE {
			ok = st.GetState() != dcctlv1.Operation_Step_SUCCEEDED
		}
	}

	return source, strings.TrimPrefix(op.GetTarget(), "datacenters/"), others, ok
}

// sourceDiscard is patroni.source_discard of a failover from a to b: while a is read as
// primary, the WAL it wrote beyond what b received needs max_discard_bytes.
func (f fakePreflight) sourceDiscard(a, b string) check.Check {
	c := check.Check{
		ID: check.PatroniSourceDiscard, Result: check.Skip, Effect: check.Confirm, Confirmation: check.ConfirmMaxDiscardBytes,
		Datacenter: a,
	}

	if da := f.w.dc(a); da.unread || da.role != patroni.RolePrimary {
		return c
	}

	n, err := fakePatroni{f.w}.DemoteLoss(context.Background(), pgctl.Cluster{Datacenter: a}, pgctl.Cluster{Datacenter: b})

	switch {
	case err != nil:
		c.Result = check.Fail
	case n > 0:
		c.Result, c.DiscardBytes = check.Fail, &n
	default:
		c.Result = check.Pass
	}

	return c
}

func isolationCheck() check.Check {
	return check.Check{ID: check.CoordIsolation, Result: check.Fail, Effect: check.Confirm, Confirmation: check.ConfirmIsolation}
}

// maxLSN is patroni.target_max_lsn of target b against others; a standby whose Patroni
// is not read is not compared.
func (f fakePreflight) maxLSN(b string, others []string) check.Check {
	c := check.Check{ID: check.PatroniTargetMaxLSN, Result: check.Pass, Effect: check.Confirm, Confirmation: check.ConfirmNotMaxLSN}

	if len(others) == 0 {
		c.Result = check.Skip

		return c
	}

	tb := f.w.dc(b)
	if tb.receivedUnknown {
		c.Result = check.Fail
	}

	for _, name := range others {
		if d := f.w.dc(name); !d.unread && (d.receivedUnknown || d.received > tb.received) {
			c.Result = check.Fail
		}
	}

	return c
}

// storedOp returns dcctl/op of c as stored, nil when it is not or does not decode.
func storedOp(c *fakeConsul) *dcctlv1.Operation {
	b := c.rawOp()
	if b == nil {
		return nil
	}

	op := new(dcctlv1.Operation)
	if kv.DecodeProto(b, op) != nil {
		return nil
	}

	return op
}

type fakeRecorder struct{}

func (fakeRecorder) RecordOperation(context.Context, *dcctlv1.Operation) error { return nil }

func (fakeRecorder) RecordStep(context.Context, dcctlv1.Operation_Type, *dcctlv1.Operation_Step) error {
	return nil
}

// fakeVerifier answers verify with its checks.
type fakeVerifier struct {
	mu     sync.Mutex
	checks []check.Check
	calls  int
}

func (f *fakeVerifier) Run(_ context.Context, req preflight.Request) (check.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if req.Action != preflight.Verify {
		return check.Verdict{}, fmt.Errorf("unexpected action %s", req.Action)
	}

	f.calls++

	return check.Verdict{Checks: slices.Clone(f.checks)}, nil
}

// dcState is the infrastructure of one datacenter: the managed jobs by stage and the
// leader of its Patroni cluster.
type dcState struct {
	running  map[int]bool
	role     patroni.Role
	timeline int64
	upstream string
	// unread makes the Patroni cluster not read.
	unread bool
	// received is the WAL its leader received, unless receivedUnknown; streaming tells
	// that the leader streams from its upstream.
	received        int64
	receivedUnknown bool
	streaming       bool
	// location is the WAL its leader wrote, when primary; leftAt is where it left the
	// timeline of a standby on another timeline, 0 when its history does not tell.
	location int64
	leftAt   int64
	// history is GET /history of its leader, every timeline before its own; a promotion
	// adds the one it leaves.
	history []patroni.HistoryEntry
	// lagging makes a member of the cluster not stream once its leader follows.
	lagging bool
}

// infra is the infrastructure the fake primitives change. A hook runs once, when the
// primitive of its key is called, before the primitive acts; an error it returns is the
// error of the primitive, which then does nothing.
type infra struct {
	mu    sync.Mutex
	dcs   map[string]*dcState
	calls []string
	hooks map[string]func(ctx context.Context) error
	// cluster is the one Patroni cluster of stretch; nil in warm_standby.
	cluster *stretchCluster
}

func (w *infra) hook(key string, fn func(ctx context.Context) error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.hooks[key] = fn
}

func (w *infra) fail(key string, err error) {
	w.hook(key, func(context.Context) error { return err })
}

func (w *infra) call(ctx context.Context, key string) error {
	w.mu.Lock()
	w.calls = append(w.calls, key)
	fn := w.hooks[key]
	delete(w.hooks, key)
	w.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}

	return nil
}

// react runs the hook of key as call does, without recording a call: the hook of a read
// that a test changes the infrastructure at.
func (w *infra) react(ctx context.Context, key string) error {
	w.mu.Lock()
	fn := w.hooks[key]
	delete(w.hooks, key)
	w.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}

	return nil
}

func (w *infra) do(fn func(dcs map[string]*dcState)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	fn(w.dcs)
}

func (w *infra) dc(name string) dcState {
	w.mu.Lock()
	defer w.mu.Unlock()

	d := *w.dcs[name]
	d.running = maps.Clone(d.running)

	return d
}

// names returns the datacenters of the infrastructure, sorted.
func (w *infra) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Sorted(maps.Keys(w.dcs))
}

func (w *infra) allCalls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.calls)
}

// guarded runs guard as the primitives do: a refusal wraps errGuard, an ended context
// gives its cause.
func guarded(ctx context.Context, guard func(context.Context) error, errGuard error) error {
	if guard == nil {
		return nil
	}

	err := guard(ctx)

	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return context.Cause(ctx)
	}

	return fmt.Errorf("%w: %w", errGuard, err)
}

type fakeApps struct{ w *infra }

func stageKey(n int) string {
	if n == math.MaxInt {
		return "all"
	}

	return fmt.Sprint(n)
}

func (f fakeApps) StopFrom(ctx context.Context, dc apps.Datacenter, from int, opts apps.Options) (apps.Result, error) {
	res := apps.Result{Datacenter: dc.Name, Action: apps.ActionStop}

	if err := guarded(ctx, opts.Guard, apps.ErrGuard); err != nil {
		return res, err
	}

	if err := f.w.call(ctx, fmt.Sprintf("stop %s from %d", dc.Name, from)); err != nil {
		return res, err
	}

	f.w.do(func(dcs map[string]*dcState) {
		for stage := range dcs[dc.Name].running {
			if stage >= from {
				dcs[dc.Name].running[stage] = false
			}
		}
	})

	opts.Progress(apps.Progress{Datacenter: dc.Name, Action: apps.ActionStop, Stage: apps.Stage{Number: from, State: apps.StageSucceeded}})

	return res, nil
}

func (f fakeApps) StopFromFailover(ctx context.Context, dc apps.Datacenter, from int, opts apps.Options) (apps.Result, error) {
	res := apps.Result{Datacenter: dc.Name, Action: apps.ActionStop}

	if err := guarded(ctx, opts.Guard, apps.ErrGuard); err != nil {
		return res, err
	}

	if err := f.w.call(ctx, fmt.Sprintf("failover stop %s from %d", dc.Name, from)); err != nil {
		return res, err
	}

	f.w.do(func(dcs map[string]*dcState) {
		for stage := range dcs[dc.Name].running {
			if stage >= from {
				dcs[dc.Name].running[stage] = false
			}
		}
	})

	return res, nil
}

func (f fakeApps) StartUpTo(ctx context.Context, dc apps.Datacenter, to int, opts apps.Options) (apps.Result, error) {
	res := apps.Result{Datacenter: dc.Name, Action: apps.ActionStart}

	if err := guarded(ctx, opts.Guard, apps.ErrGuard); err != nil {
		return res, err
	}

	if err := f.w.call(ctx, fmt.Sprintf("start %s to %s", dc.Name, stageKey(to))); err != nil {
		return res, err
	}

	f.w.do(func(dcs map[string]*dcState) {
		for stage := range dcs[dc.Name].running {
			if stage <= to {
				dcs[dc.Name].running[stage] = true
			}
		}
	})

	return res, nil
}

type fakePatroni struct{ w *infra }

func (f fakePatroni) result(dc string) pgctl.Result {
	d := f.w.dc(dc)
	tl := d.timeline

	return pgctl.Result{Datacenter: dc, Observation: pgctl.Observation{Role: d.role, Timeline: &tl}}
}

func (f fakePatroni) Demote(ctx context.Context, cl, target pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error) {
	if err := guarded(ctx, opts.Guard, pgctl.ErrGuard); err != nil {
		return f.result(cl.Datacenter), err
	}

	if err := f.w.call(ctx, "demote "+cl.Datacenter); err != nil {
		return f.result(cl.Datacenter), err
	}

	f.w.do(func(dcs map[string]*dcState) {
		dcs[cl.Datacenter].role, dcs[cl.Datacenter].upstream = patroni.RoleStandbyLeader, target.Datacenter
	})

	return f.result(cl.Datacenter), nil
}

func (f fakePatroni) Catchup(ctx context.Context, demoted, target pgctl.Cluster, _ pgctl.Options) (pgctl.Result, error) {
	if err := f.w.call(ctx, "catchup "+target.Datacenter); err != nil {
		return f.result(target.Datacenter), err
	}

	for _, dc := range []string{demoted.Datacenter, target.Datacenter} {
		switch role := f.w.dc(dc).role; {
		case role == patroni.RolePrimary:
			return f.result(target.Datacenter), &pgctl.PrimaryError{Datacenter: dc, Where: "leader " + dc + "1"}
		case role != patroni.RoleStandbyLeader:
			return f.result(target.Datacenter), fmt.Errorf("%w: %s is %q", pgctl.ErrTimeout, dc, role)
		}
	}

	return f.result(target.Datacenter), nil
}

func clusterNames(clusters []pgctl.Cluster) string {
	names := make([]string, 0, len(clusters))
	for _, cl := range clusters {
		names = append(names, cl.Datacenter)
	}

	return strings.Join(names, ",")
}

func (f fakePatroni) NoPrimary(ctx context.Context, clusters, others []pgctl.Cluster, _ pgctl.Options) (pgctl.Result, error) {
	res := pgctl.Result{Datacenter: clusterNames(clusters), Action: pgctl.ActionNoPrimary}

	// A check that needs no cluster is named by the others.
	key := "no_primary " + res.Datacenter
	if len(clusters) == 0 {
		key = "no_primary " + clusterNames(others)
	}

	if err := f.w.call(ctx, key); err != nil {
		return res, err
	}

	for _, cl := range clusters {
		switch d := f.w.dc(cl.Datacenter); {
		case d.unread:
			return res, fmt.Errorf("no_primary: patroni cluster of %s not read: %w", cl.Datacenter, pgctl.ErrTimeout)
		case d.role == patroni.RolePrimary:
			return res, &pgctl.PrimaryError{Datacenter: cl.Datacenter, Where: "leader " + cl.Datacenter + "1"}
		}
	}

	for _, cl := range others {
		switch d := f.w.dc(cl.Datacenter); {
		case d.unread:
			res.Unread = append(res.Unread, cl.Datacenter)
		case d.role == patroni.RolePrimary:
			return res, &pgctl.PrimaryError{Datacenter: cl.Datacenter, Where: "leader " + cl.Datacenter + "1"}
		}
	}

	return res, nil
}

func (f fakePatroni) Promote(ctx context.Context, cl pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error) {
	if err := guarded(ctx, opts.Guard, pgctl.ErrGuard); err != nil {
		return f.result(cl.Datacenter), err
	}

	if err := f.w.call(ctx, "promote "+cl.Datacenter); err != nil {
		return f.result(cl.Datacenter), err
	}

	fromPrimary := false

	f.w.do(func(dcs map[string]*dcState) {
		d := dcs[cl.Datacenter]
		if fromPrimary = d.role == patroni.RolePrimary; !fromPrimary {
			d.history = append(slices.Clone(d.history), patroni.HistoryEntry{Timeline: d.timeline, LSN: d.received})
			d.role = patroni.RolePrimary
			d.timeline++
		}

		d.upstream = ""
	})

	res := f.result(cl.Datacenter)
	res.FromPrimary = fromPrimary

	return res, nil
}

func (f fakePatroni) Repoint(ctx context.Context, standby, primary pgctl.Cluster, opts pgctl.Options) (pgctl.Result, error) {
	if err := guarded(ctx, opts.Guard, pgctl.ErrGuard); err != nil {
		return f.result(standby.Datacenter), err
	}

	if err := f.w.call(ctx, "repoint "+standby.Datacenter); err != nil {
		return f.result(standby.Datacenter), err
	}

	f.w.do(func(dcs map[string]*dcState) {
		dcs[standby.Datacenter].upstream = primary.Datacenter
		dcs[standby.Datacenter].timeline = dcs[primary.Datacenter].timeline
	})

	return f.result(standby.Datacenter), nil
}

func (f fakePatroni) Follow(ctx context.Context, standby, primary pgctl.Cluster, _ pgctl.Options) (pgctl.Result, error) {
	if err := f.w.call(ctx, "follow "+standby.Datacenter); err != nil {
		return f.result(standby.Datacenter), err
	}

	if d := f.w.dc(standby.Datacenter); d.upstream != primary.Datacenter || d.role != patroni.RoleStandbyLeader {
		return f.result(standby.Datacenter), fmt.Errorf("%w: %s does not point at %s", pgctl.ErrTimeout, standby.Datacenter, primary.Datacenter)
	}

	f.w.do(func(dcs map[string]*dcState) {
		dcs[standby.Datacenter].timeline = dcs[primary.Datacenter].timeline
	})

	return f.result(standby.Datacenter), nil
}

// Leader reads the leader of the cluster; it records no call.
func (f fakePatroni) Leader(_ context.Context, cl pgctl.Cluster) (patroni.Member, error) {
	d := f.w.dc(cl.Datacenter)
	if d.unread {
		return patroni.Member{}, fmt.Errorf("patroni cluster of %s not read", cl.Datacenter)
	}

	tl := d.timeline
	st := &patroni.State{Role: d.role, Timeline: &tl}

	if !d.receivedUnknown {
		received := d.received
		st.Xlog.ReceivedLocation = &received
	}

	if d.streaming {
		st.ReplicationState = patroni.ReplicationStreaming
	}

	if d.role == patroni.RolePrimary {
		location := d.location
		st.Xlog.Location = &location
	}

	return patroni.Member{Name: cl.Datacenter + "1", Probe: st}, nil
}

// Divergence reads the divergence of standby; it records no call, but runs the hook of
// "divergence <standby>" first.
func (f fakePatroni) Divergence(ctx context.Context, standby, primary pgctl.Cluster) (patroni.Divergence, error) {
	if err := f.w.react(ctx, "divergence "+standby.Datacenter); err != nil {
		return patroni.Divergence{}, err
	}

	if d := f.w.dc(standby.Datacenter); !d.unread && d.role == patroni.RolePrimary {
		return patroni.Divergence{}, &pgctl.PrimaryError{Datacenter: standby.Datacenter, Where: "leader " + standby.Datacenter + "1"}
	}

	return f.divergence(standby.Datacenter, primary.Datacenter)
}

// Demoted reads whether cl is a standby leader pointed at target; it records no call.
func (f fakePatroni) Demoted(_ context.Context, cl, target pgctl.Cluster) (bool, error) {
	d := f.w.dc(cl.Datacenter)
	if d.unread {
		return false, fmt.Errorf("patroni cluster of %s not read", cl.Datacenter)
	}

	return d.role == patroni.RoleStandbyLeader && d.upstream == target.Datacenter, nil
}

// DivergenceOf reads the divergence of cl whatever the role of its leader, as Divergence
// does.
func (f fakePatroni) DivergenceOf(ctx context.Context, cl, primary pgctl.Cluster) (patroni.Divergence, error) {
	if err := f.w.react(ctx, "divergence "+cl.Datacenter); err != nil {
		return patroni.Divergence{}, err
	}

	return f.divergence(cl.Datacenter, primary.Datacenter)
}

// divergence is the divergence of dc from primary, whose leader must be primary.
func (f fakePatroni) divergence(dc, primary string) (patroni.Divergence, error) {
	d, p := f.w.dc(dc), f.w.dc(primary)

	switch {
	case d.unread:
		return patroni.Divergence{}, fmt.Errorf("patroni cluster of %s not read", dc)
	case p.unread || p.role != patroni.RolePrimary:
		return patroni.Divergence{}, fmt.Errorf("%w: leader of %s is not a primary", pgctl.ErrPrecondition, primary)
	case d.role == patroni.RolePrimary && d.timeline == p.timeline:
		return patroni.Divergence{}, fmt.Errorf("%s and %s are primary on the same timeline %d", dc, primary, d.timeline)
	}

	var pos *int64

	switch {
	case d.role == patroni.RolePrimary:
		pos = &d.location
	case !d.receivedUnknown:
		pos = &d.received
	}

	div, ok := patroni.Diverge(p.timeline, p.history, d.timeline, d.history, pos)
	if !ok {
		return patroni.Divergence{}, fmt.Errorf("the histories of %s and %s do not tell whether %s diverged", primary, dc, dc)
	}

	return div, nil
}

// ancestry is the history of a cluster on timeline: it left every earlier timeline t at
// 100 × t.
func ancestry(timeline int64) []patroni.HistoryEntry {
	out := make([]patroni.HistoryEntry, 0, timeline)
	for t := int64(1); t < timeline; t++ {
		out = append(out, patroni.HistoryEntry{Timeline: t, LSN: 100 * t})
	}

	return out
}

// follows reports why the cluster of standby does not follow primary in full, "" when it
// does.
func (f fakePatroni) follows(standby, primary string) string {
	d, p := f.w.dc(standby), f.w.dc(primary)

	switch {
	case d.unread:
		return "patroni cluster of " + standby + " not read"
	case d.role != patroni.RoleStandbyLeader || d.upstream != primary:
		return fmt.Sprintf("%s is %s of %q", standby, d.role, d.upstream)
	case d.timeline != p.timeline:
		return fmt.Sprintf("%s is on timeline %d", standby, d.timeline)
	case d.lagging:
		return "not streaming: " + standby + "2"
	}

	return ""
}

// Recover makes standby, pointed at primary, follow it on its timeline.
func (f fakePatroni) Recover(ctx context.Context, standby, primary pgctl.Cluster, _ pgctl.Options) (pgctl.Result, error) {
	if err := f.w.call(ctx, "recover "+standby.Datacenter); err != nil {
		return f.result(standby.Datacenter), err
	}

	if d := f.w.dc(standby.Datacenter); d.role == patroni.RoleStandbyLeader && d.upstream == primary.Datacenter {
		f.w.do(func(dcs map[string]*dcState) { dcs[standby.Datacenter].timeline = dcs[primary.Datacenter].timeline })
	}

	if why := f.follows(standby.Datacenter, primary.Datacenter); why != "" {
		return f.result(standby.Datacenter), fmt.Errorf("%w: %s", pgctl.ErrTimeout, why)
	}

	return f.result(standby.Datacenter), nil
}

// Recovered reads whether standby follows primary in full.
func (f fakePatroni) Recovered(ctx context.Context, standby, primary pgctl.Cluster) error {
	if err := f.w.call(ctx, "recovered "+standby.Datacenter); err != nil {
		return err
	}

	if why := f.follows(standby.Datacenter, primary.Datacenter); why != "" {
		return fmt.Errorf("%s: %w", why, pgctl.ErrNotRecovered)
	}

	return nil
}

// DemoteLoss is what demoting primary to follow standby discards; it records no call.
func (f fakePatroni) DemoteLoss(_ context.Context, primary, standby pgctl.Cluster) (int64, error) {
	dp, ds := f.w.dc(primary.Datacenter), f.w.dc(standby.Datacenter)

	switch {
	case dp.unread || ds.unread:
		return 0, fmt.Errorf("patroni cluster of %s or %s not read", primary.Datacenter, standby.Datacenter)
	case ds.receivedUnknown:
		return 0, fmt.Errorf("the WAL %s received is not known", standby.Datacenter)
	case dp.timeline != ds.timeline && dp.leftAt == 0:
		return 0, fmt.Errorf("where %s left timeline %d is not known", primary.Datacenter, ds.timeline)
	case dp.timeline != ds.timeline:
		return max(0, dp.location-min(ds.received, dp.leftAt)), nil
	}

	return max(0, dp.location-ds.received), nil
}

// nomadStub and patroniStub fill the datacenters of the deps; the fake primitives do not
// call them.
type nomadStub struct{ apps.Nomad }

type patroniStub struct{ pgctl.Patroni }
