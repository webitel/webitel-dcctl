package store

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompareAndSwap_CreateState(t *testing.T) {
	s := newStore(t, t.TempDir())

	got, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)
	assert.Equal(t, Record{Value: stateJSON, Index: uint64(at(1).UnixNano()), UpdateTime: at(1)}, got)

	ok, err := s.Initialized()
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCompareAndSwap_CreateLease(t *testing.T) {
	s := newStore(t, t.TempDir())
	_, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)

	got, err := s.CompareAndSwap(RecordLease, 0, leaseJSON)
	require.NoError(t, err)
	assert.Equal(t, Record{Value: leaseJSON, Index: uint64(at(2).UnixNano()), UpdateTime: at(2)}, got)

	stored, err := s.Get(RecordLease)
	require.NoError(t, err)
	assert.Equal(t, got, stored)
}

func TestCompareAndSwap_Update(t *testing.T) {
	for _, name := range []string{RecordState, RecordLease} {
		t.Run(name, func(t *testing.T) {
			s, state, lease := newInitialized(t)
			cur := map[string]Record{RecordState: state, RecordLease: lease}[name]

			got, err := s.CompareAndSwap(name, cur.Index, newJSON)
			require.NoError(t, err)
			assert.Equal(t, Record{Value: newJSON, Index: cur.Index + 1, UpdateTime: at(3)}, got)

			stored, err := s.Get(name)
			require.NoError(t, err)
			assert.Equal(t, got, stored)
		})
	}
}

func TestCompareAndSwap_Mismatch(t *testing.T) {
	tests := []struct {
		name   string
		record string
		index  func(state, lease Record) uint64
	}{
		{name: "stale_state", record: RecordState, index: func(s, _ Record) uint64 { return s.Index - 1 }},
		{name: "future_lease", record: RecordLease, index: func(_, l Record) uint64 { return l.Index + 1 }},
		{name: "create_existing_state", record: RecordState, index: func(_, _ Record) uint64 { return 0 }},
		{name: "create_existing_lease", record: RecordLease, index: func(_, _ Record) uint64 { return 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, state, lease := newInitialized(t)
			want, err := s.Get(tt.record)
			require.NoError(t, err)

			_, err = s.CompareAndSwap(tt.record, tt.index(state, lease), newJSON)
			require.ErrorIs(t, err, ErrIndexMismatch)

			var mismatch *MismatchError
			require.ErrorAs(t, err, &mismatch)
			assert.Equal(t, want, mismatch.Current)

			got, err := s.Get(tt.record)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestCompareAndSwap_Uninitialized(t *testing.T) {
	tests := []struct {
		name   string
		record string
		index  uint64
	}{
		{name: "create_lease", record: RecordLease, index: 0},
		{name: "update_lease", record: RecordLease, index: 1},
		{name: "update_state", record: RecordState, index: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t, t.TempDir())

			_, err := s.CompareAndSwap(tt.record, tt.index, newJSON)
			require.ErrorIs(t, err, ErrUninitialized)

			ok, err := s.Initialized()
			require.NoError(t, err)
			assert.False(t, ok)

			_, err = s.Get(RecordLease)
			assert.ErrorIs(t, err, ErrUninitialized)
		})
	}
}

func TestCompareAndSwap_NotFound(t *testing.T) {
	s := newStore(t, t.TempDir())
	_, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)

	_, err = s.CompareAndSwap(RecordLease, 1, leaseJSON)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.Get(RecordLease)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestCompareAndSwap_UnknownRecord(t *testing.T) {
	s, _, _ := newInitialized(t)

	_, err := s.CompareAndSwap("op", 0, newJSON)
	assert.ErrorIs(t, err, ErrUnknownRecord)
}

func TestCompareAndSwap_Concurrent(t *testing.T) {
	const writers = 16

	s, _, lease := newInitialized(t)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  []Record
		errs []error
	)

	for i := range writers {
		wg.Go(func() {
			rec, err := s.CompareAndSwap(RecordLease, lease.Index, []byte{byte(i)})

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)

				return
			}

			won = append(won, rec)
		})
	}

	wg.Wait()

	require.Len(t, won, 1)
	assert.Equal(t, lease.Index+1, won[0].Index)
	require.Len(t, errs, writers-1)

	for _, err := range errs {
		var mismatch *MismatchError
		require.ErrorAs(t, err, &mismatch)
		assert.Equal(t, won[0], mismatch.Current)
	}
}

func TestCompareAndSwap_Reopen(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	require.NoError(t, err)
	_, err = s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)
	created, err := s.CompareAndSwap(RecordLease, 0, leaseJSON)
	require.NoError(t, err)
	updated, err := s.CompareAndSwap(RecordLease, created.Index, newJSON)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s = newStore(t, dir)

	stored, err := s.Get(RecordLease)
	require.NoError(t, err)
	assert.Equal(t, updated, stored)
	assert.False(t, stored.UpdateTime.Before(created.UpdateTime))

	got, err := s.CompareAndSwap(RecordLease, updated.Index, leaseJSON)
	require.NoError(t, err)
	assert.Equal(t, Record{Value: leaseJSON, Index: updated.Index + 1, UpdateTime: at(1)}, got)
}

func TestCompareAndSwap_RecreatedIndexGrows(t *testing.T) {
	old, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Close() })

	rec, err := old.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)

	for range 3 {
		rec, err = old.CompareAndSwap(RecordState, rec.Index, newJSON)
		require.NoError(t, err)
	}

	replaced, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = replaced.Close() })

	got, err := replaced.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)
	assert.Greater(t, got.Index, rec.Index)
}
