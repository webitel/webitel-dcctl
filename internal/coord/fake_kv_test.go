package coord_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"syscall"

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

	createErr error
	// onCreate runs at the start of CreateSession.
	onCreate func()
	// lockDelay refuses to acquire a key that no session holds, as Consul
	// does in the lock delay after the session of its holder ended.
	lockDelay bool
	// forgetSessions creates sessions that are gone at once.
	forgetSessions bool
	// onAcquire runs after a successful acquire.
	onAcquire  func()
	acquireErr error
	// acquireApplied applies an acquire that fails with acquireErr.
	acquireApplied bool
	releaseErr     error
	renewErr       error
	getErr         error
	// casErr fails a CAS after applying it when casApplied is set.
	casErr     error
	casApplied bool
	// beforeCAS runs at the start of KVCAS, as another writer would.
	beforeCAS func(f *fakeConsul)

	destroyed []string
	casCalls  int
}

func newFakeConsul() *fakeConsul {
	return &fakeConsul{index: 100, keys: map[string]*fakeKey{}, sessions: map[string]bool{}}
}

// unreachable is an error as the consul client returns it for a server that
// does not answer.
func unreachable() error {
	return source.Classify("fake:8500", fmt.Errorf("dial: %w", syscall.ECONNREFUSED))
}

func outcomeUnknown() error {
	return fmt.Errorf("%w: %w", consul.ErrOutcomeUnknown, unreachable())
}

func (f *fakeConsul) CreateSession(ctx context.Context, _ string) (string, error) {
	f.mu.Lock()
	onCreate := f.onCreate
	f.mu.Unlock()

	if onCreate != nil {
		onCreate()
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return "", f.createErr
	}

	f.nextID++
	id := fmt.Sprintf("session-%d", f.nextID)
	f.sessions[id] = !f.forgetSessions

	return id, nil
}

func (f *fakeConsul) RenewSession(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.renewErr != nil:
		return f.renewErr
	case !f.sessions[id]:
		return consul.ErrSessionNotFound
	}

	return nil
}

func (f *fakeConsul) DestroySession(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.destroyed = append(f.destroyed, id)
	f.invalidate(id)

	return nil
}

// expire ends a session as its TTL would.
func (f *fakeConsul) expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.invalidate(id)
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
	if err := ctx.Err(); err != nil {
		return false, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.acquireErr != nil && !f.acquireApplied {
		return false, f.acquireErr
	}

	if !f.sessions[session] {
		return false, fmt.Errorf("%w: invalid session", consul.ErrSessionNotFound)
	}

	k := f.keys[key]
	if k != nil && k.session != "" && k.session != session || k != nil && k.session == "" && f.lockDelay {
		return false, f.acquireErr
	}

	f.put(key, value, session)

	if f.onAcquire != nil {
		f.onAcquire()
	}

	return true, f.acquireErr
}

func (f *fakeConsul) Release(ctx context.Context, key string, value []byte, session string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.releaseErr != nil {
		return false, f.releaseErr
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
	if err := ctx.Err(); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return nil, consul.QueryMeta{}, f.getErr
	}

	k := f.keys[key]
	if k == nil {
		return nil, consul.QueryMeta{KnownLeader: true}, consul.ErrKeyNotFound
	}

	return &consul.KVEntry{Key: key, Value: slices.Clone(k.value), ModifyIndex: k.index, Session: k.session},
		consul.QueryMeta{KnownLeader: true}, nil
}

func (f *fakeConsul) KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	f.mu.Lock()
	before := f.beforeCAS
	f.beforeCAS = nil
	f.mu.Unlock()

	if before != nil {
		before(f)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.casCalls++

	if f.casErr != nil && !f.casApplied {
		return false, f.casErr
	}

	var current uint64
	if k := f.keys[key]; k != nil {
		current = k.index
	}

	if current != modifyIndex {
		return false, f.casErr
	}

	f.put(key, value, f.sessionOf(key))

	return true, f.casErr
}

func (f *fakeConsul) sessionOf(key string) string {
	if k := f.keys[key]; k != nil {
		return k.session
	}

	return ""
}

func (f *fakeConsul) put(key string, value []byte, session string) {
	f.index++
	f.keys[key] = &fakeKey{value: slices.Clone(value), index: f.index, session: session}
}

func (f *fakeConsul) holder(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sessionOf(key)
}

func (f *fakeConsul) live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string

	for id, alive := range f.sessions {
		if alive {
			out = append(out, id)
		}
	}

	slices.Sort(out)

	return out
}

func (f *fakeConsul) with(fn func(f *fakeConsul)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}
