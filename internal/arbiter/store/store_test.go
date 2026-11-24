package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

var (
	stateJSON = []byte(`{"generation":1,"active_datacenter":"dc1"}`)
	leaseJSON = []byte(`{"holder":"dc1","generation":1,"counter":7}`)
	newJSON   = []byte(`{"generation":2,"active_datacenter":"dc2"}`)

	t0 = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
)

// at is the time the n-th call of a [tick] clock returns.
func at(n int) time.Time {
	return t0.Add(time.Duration(n) * time.Second)
}

// tick returns a clock that moves one second forward on every call.
func tick() func() time.Time {
	var (
		mu sync.Mutex
		n  int
	)

	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		n++

		return at(n)
	}
}

func newStore(t *testing.T, dir string) *Store {
	t.Helper()

	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	s.now = tick()

	return s
}

// newInitialized returns a store whose state was created at at(1) and lease at at(2).
func newInitialized(t *testing.T) (s *Store, state, lease Record) {
	t.Helper()

	s = newStore(t, t.TempDir())

	state, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)

	lease, err = s.CompareAndSwap(RecordLease, 0, leaseJSON)
	require.NoError(t, err)

	return s, state, lease
}

func TestOpen_Uninitialized(t *testing.T) {
	s := newStore(t, t.TempDir())

	ok, err := s.Initialized()
	require.NoError(t, err)
	assert.False(t, ok)

	for _, name := range []string{RecordState, RecordLease} {
		_, err = s.Get(name)
		assert.ErrorIs(t, err, ErrUninitialized, name)
	}
}

func TestOpen_MissingDir(t *testing.T) {
	_, err := Open(t.TempDir() + "/missing")
	assert.Error(t, err)
}

func TestOpen_Locked(t *testing.T) {
	dir := t.TempDir()
	newStore(t, dir)

	start := time.Now()
	_, err := Open(dir)

	require.ErrorIs(t, err, ErrLocked)
	assert.Contains(t, err.Error(), dir)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestGet(t *testing.T) {
	s, state, lease := newInitialized(t)

	ok, err := s.Initialized()
	require.NoError(t, err)
	assert.True(t, ok)

	got, err := s.Get(RecordState)
	require.NoError(t, err)
	assert.Equal(t, Record{Value: stateJSON, Index: uint64(at(1).UnixNano()), UpdateTime: at(1)}, got)
	assert.Equal(t, state, got)

	got, err = s.Get(RecordLease)
	require.NoError(t, err)
	assert.Equal(t, Record{Value: leaseJSON, Index: uint64(at(2).UnixNano()), UpdateTime: at(2)}, got)
	assert.Equal(t, lease, got)
}

func TestGet_NotFound(t *testing.T) {
	s := newStore(t, t.TempDir())
	_, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)

	_, err = s.Get(RecordLease)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGet_UnknownRecord(t *testing.T) {
	s, _, _ := newInitialized(t)

	_, err := s.Get("op")
	assert.ErrorIs(t, err, ErrUnknownRecord)
}

func TestGet_CorruptValue(t *testing.T) {
	valid := encode(Record{Value: leaseJSON, Index: 1, UpdateTime: t0})
	unknownVersion := append([]byte{formatVersion + 1}, valid[1:]...)

	tests := []struct {
		name  string
		value []byte
	}{
		{name: "short", value: valid[:headerSize-1]},
		{name: "unknown_version", value: unknownVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := newInitialized(t)
			require.NoError(t, s.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucket).Put([]byte(RecordLease), tt.value)
			}))

			_, err := s.Get(RecordLease)
			require.Error(t, err)
			assert.Contains(t, err.Error(), RecordLease)

			_, err = s.CompareAndSwap(RecordLease, 1, newJSON)
			require.Error(t, err)

			assert.Error(t, s.Check(t.Context()))
		})
	}
}

func TestReopen(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	require.NoError(t, err)
	want, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s = newStore(t, dir)

	ok, err := s.Initialized()
	require.NoError(t, err)
	assert.True(t, ok)

	got, err := s.Get(RecordState)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestClosed(t *testing.T) {
	s, state, _ := newInitialized(t)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())

	_, err := s.Get(RecordState)
	require.ErrorIs(t, err, ErrClosed)

	_, err = s.CompareAndSwap(RecordState, state.Index, newJSON)
	require.ErrorIs(t, err, ErrClosed)

	_, err = s.Initialized()
	require.ErrorIs(t, err, ErrClosed)

	assert.ErrorIs(t, s.Check(t.Context()), ErrClosed)
}

func TestCheck(t *testing.T) {
	s, _, _ := newInitialized(t)
	require.NoError(t, s.Check(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, s.Check(ctx), context.Canceled)
}

func TestCheck_Uninitialized(t *testing.T) {
	s := newStore(t, t.TempDir())

	assert.NoError(t, s.Check(t.Context()))
}
