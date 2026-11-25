package journal_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

type fakeKey struct {
	value []byte
	index uint64
}

// fakeConsul is the KV of one Consul, shared by every journal that uses it.
type fakeConsul struct {
	mu    sync.Mutex
	index uint64
	keys  map[string]*fakeKey

	// down makes every call fail as a server that does not answer.
	down bool
	// casErr fails a CAS after applying it when casApplied is set.
	casErr     error
	casApplied bool
	getErr     error
	delErr     error
	// beforeDelete runs at the start of KVDeleteCAS, as another writer would.
	beforeDelete func(f *fakeConsul, key string)

	casCalls int
	delCalls int
}

func newFakeConsul() *fakeConsul {
	return &fakeConsul{index: 100, keys: map[string]*fakeKey{}}
}

func unreachable() error {
	return source.Classify("fake:8500", fmt.Errorf("dial: %w", syscall.ECONNREFUSED))
}

func outcomeUnknown() error {
	return fmt.Errorf("%w: %w", consul.ErrOutcomeUnknown, unreachable())
}

func (f *fakeConsul) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.down = down
}

func (f *fakeConsul) KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, consul.QueryMeta{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.down:
		return nil, consul.QueryMeta{}, unreachable()
	case f.getErr != nil:
		return nil, consul.QueryMeta{}, f.getErr
	}

	k := f.keys[key]
	if k == nil {
		return nil, consul.QueryMeta{KnownLeader: true}, consul.ErrKeyNotFound
	}

	return &consul.KVEntry{Key: key, Value: slices.Clone(k.value), ModifyIndex: k.index}, consul.QueryMeta{KnownLeader: true}, nil
}

func (f *fakeConsul) KVList(ctx context.Context, prefix string) ([]consul.KVEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.down {
		return nil, unreachable()
	}

	var out []consul.KVEntry

	for key, k := range f.keys {
		if strings.HasPrefix(key, prefix) {
			out = append(out, consul.KVEntry{Key: key, Value: slices.Clone(k.value), ModifyIndex: k.index})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	return out, nil
}

func (f *fakeConsul) KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.casCalls++

	if f.down {
		return false, unreachable()
	}

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

	f.put(key, value)

	return true, f.casErr
}

func (f *fakeConsul) KVDeleteCAS(ctx context.Context, key string, modifyIndex uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	f.mu.Lock()
	before := f.beforeDelete
	f.mu.Unlock()

	if before != nil {
		before(f, key)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.delCalls++

	switch {
	case f.down:
		return false, unreachable()
	case f.delErr != nil:
		return false, f.delErr
	}

	k := f.keys[key]
	if k == nil || k.index != modifyIndex {
		return false, nil
	}

	delete(f.keys, key)

	return true, nil
}

func (f *fakeConsul) put(key string, value []byte) {
	f.index++
	f.keys[key] = &fakeKey{value: slices.Clone(value), index: f.index}
}

// set writes key as another dcctl would.
func (f *fakeConsul) set(key string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.put(key, value)
}

func (f *fakeConsul) value(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	if k := f.keys[key]; k != nil {
		return slices.Clone(k.value)
	}

	return nil
}

// names returns the keys in order.
func (f *fakeConsul) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.keys))
	for k := range f.keys {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
