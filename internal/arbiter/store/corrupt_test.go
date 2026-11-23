package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writtenFile returns the contents of an initialized store file and its page size.
func writtenFile(t *testing.T) (data []byte, pageSize int) {
	t.Helper()

	dir := t.TempDir()

	s, err := Open(dir)
	require.NoError(t, err)
	state, err := s.CompareAndSwap(RecordState, 0, stateJSON)
	require.NoError(t, err)
	_, err = s.CompareAndSwap(RecordLease, 0, leaseJSON)
	require.NoError(t, err)
	_, err = s.CompareAndSwap(RecordState, state.Index, newJSON)
	require.NoError(t, err)

	pageSize = s.db.Info().PageSize
	require.NoError(t, s.Close())

	data, err = os.ReadFile(filepath.Join(dir, fileName))
	require.NoError(t, err)

	return data, pageSize
}

func TestOpen_Corrupt(t *testing.T) {
	data, pageSize := writtenFile(t)

	flipped := make([]byte, len(data))
	for i, b := range data {
		flipped[i] = b
		if i >= 2*pageSize {
			flipped[i] = ^b
		}
	}

	// The leaf holding the records gets a broken page header.
	leaf := bytes.LastIndex(data, newJSON) / pageSize * pageSize
	leafFlipped := bytes.Clone(data)

	for i := leaf; i < leaf+16; i++ {
		leafFlipped[i] = ^data[i]
	}

	tests := []struct {
		name string
		data []byte
	}{
		{name: "truncated", data: data[:len(data)/2]},
		{name: "pages_flipped", data: flipped},
		{name: "leaf_flipped", data: leafFlipped},
		{name: "garbage", data: []byte("not a bbolt file at all, just text")},
		{name: "zeros", data: make([]byte, 4*pageSize)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, fileName)
			require.NoError(t, os.WriteFile(path, tt.data, 0o600))

			s, err := Open(dir)
			if s != nil {
				_ = s.Close()
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), path)
		})
	}
}
