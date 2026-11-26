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
}

func (f fakePreflight) Run(_ context.Context, req preflight.Request) (check.Verdict, error) {
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

		cs = append(cs, isolationCheck(), f.maxLSN(req.Params.Datacenter, others))
	case preflight.Resume:
		target, others, unpromoted := unpromotedFailover(f.op())
		if req.Params.ToForced || unpromoted {
			cs = append(cs, isolationCheck())
		}

		if unpromoted {
			cs = append(cs, f.maxLSN(target, others))
		}
	case preflight.Demote, preflight.Repoint, preflight.MarkRecovered, preflight.Abandon, preflight.Resolve,
		preflight.Init, preflight.ArbiterInit, preflight.AppsSync, preflight.Verify:
		return check.Verdict{}, fmt.Errorf("unexpected action %s", req.Action)
	}

	return check.Decide(cs, req.Confirmations), nil
}

// unpromotedFailover returns the target of op and the standbys it repoints when op is a
// failover whose promotion did not succeed.
func unpromotedFailover(op *dcctlv1.Operation) (target string, others []string, ok bool) {
	if op.GetType() != dcctlv1.Operation_FAILOVER {
		return "", nil, false
	}

	ok = true

	for _, st := range op.GetSteps() {
		if st.GetType() == dcctlv1.Operation_Step_PATRONI_REPOINT {
			others = append(others, strings.TrimPrefix(st.GetDatacenter(), "datacenters/"))
		}

		if st.GetType() == dcctlv1.Operation_Step_PATRONI_PROMOTE {
			ok = st.GetState() != dcctlv1.Operation_Step_SUCCEEDED
		}
	}

	return strings.TrimPrefix(op.GetTarget(), "datacenters/"), others, ok
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
	// diverged is how many bytes the cluster holds past its divergence from the primary.
	diverged int64
}

// infra is the infrastructure the fake primitives change. A hook runs once, when the
// primitive of its key is called, before the primitive acts; an error it returns is the
// error of the primitive, which then does nothing.
type infra struct {
	mu    sync.Mutex
	dcs   map[string]*dcState
	calls []string
	hooks map[string]func(ctx context.Context) error
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

	return patroni.Member{Name: cl.Datacenter + "1", Probe: st}, nil
}

// Divergence reads the divergence of standby; it records no call.
func (f fakePatroni) Divergence(_ context.Context, standby, _ pgctl.Cluster) (patroni.Divergence, error) {
	switch d := f.w.dc(standby.Datacenter); {
	case d.unread:
		return patroni.Divergence{}, fmt.Errorf("patroni cluster of %s not read", standby.Datacenter)
	case d.role == patroni.RolePrimary:
		return patroni.Divergence{}, &pgctl.PrimaryError{Datacenter: standby.Datacenter, Where: "leader " + standby.Datacenter + "1"}
	case d.diverged > 0:
		return patroni.Divergence{Diverged: true, LSN: 1000, Bytes: d.diverged}, nil
	}

	return patroni.Divergence{}, nil
}

// nomadStub and patroniStub fill the datacenters of the deps; the fake primitives do not
// call them.
type nomadStub struct{ apps.Nomad }

type patroniStub struct{ pgctl.Patroni }
