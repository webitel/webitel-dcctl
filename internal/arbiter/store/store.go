// Package store keeps the arbiter's records in a local bbolt file.
//
// The store holds two records, [RecordState] and [RecordLease]. Their values
// are opaque bytes. Every write is a compare-and-swap on the record's index.
// A store without [RecordState] is uninitialized: the only write it accepts
// is creating [RecordState] with index 0, and everything else fails with
// [ErrUninitialized].
package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// Record names.
const (
	RecordState = "state"
	RecordLease = "lease"
)

const (
	fileName    = "arbiter.db"
	lockTimeout = time.Second
)

// A stored value is [formatVersion u8][index u64][update time, Unix ns i64][value].
const (
	formatVersion = 1
	headerSize    = 1 + 8 + 8
)

var bucket = []byte("records")

var (
	// ErrLocked means another process holds the store file.
	ErrLocked = errors.New("arbiter store is locked by another process")
	// ErrUninitialized means the store has no [RecordState] yet.
	ErrUninitialized = errors.New("arbiter store is not initialized")
	// ErrNotFound means an initialized store has no such record.
	ErrNotFound = errors.New("record not found")
	// ErrUnknownRecord means the name is neither [RecordState] nor [RecordLease].
	ErrUnknownRecord = errors.New("unknown record")
	// ErrIndexMismatch is matched by [*MismatchError].
	ErrIndexMismatch = errors.New("record index mismatch")
)

// Record is a stored value with its index and the time of its last write.
type Record struct {
	Value []byte
	// Index is the record's etag in the arbiter API. A new record starts at
	// its creation time in Unix nanoseconds and every write adds one, so a
	// record re-created in a new store gets a greater index than before.
	Index      uint64
	UpdateTime time.Time
}

// Store is the arbiter's record store. It is safe for concurrent use.
type Store struct {
	db  *bolt.DB
	now func() time.Time
}

// Open opens or creates the store file in dir. The directory must exist.
// It fails with [ErrLocked] when another process holds the file.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, fileName)

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: lockTimeout})
	if errors.Is(err, bolterrors.ErrTimeout) {
		err = ErrLocked
	}

	if err != nil {
		return nil, fmt.Errorf("open arbiter store %s: %w", path, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)

		return err
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open arbiter store %s: %w", path, err), db.Close())
	}

	return &Store{db: db, now: time.Now}, nil
}

// Close closes the store file. Further calls fail.
func (s *Store) Close() error {
	return s.db.Close()
}

// Get returns the named record.
func (s *Store) Get(name string) (Record, error) {
	if err := checkName(name); err != nil {
		return Record{}, fmt.Errorf("get record %q: %w", name, err)
	}

	var rec Record

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b.Get([]byte(RecordState)) == nil {
			return ErrUninitialized
		}

		var err error

		rec, err = get(b, name)

		return err
	})
	if err != nil {
		return Record{}, fmt.Errorf("get record %q: %w", name, err)
	}

	return rec, nil
}

// MismatchError is returned by [Store.CompareAndSwap] when the expected
// index does not match the stored record. It matches [ErrIndexMismatch].
type MismatchError struct {
	Current Record
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s: current index %d", ErrIndexMismatch, e.Current.Index)
}

func (e *MismatchError) Is(target error) bool {
	return target == ErrIndexMismatch
}

// CompareAndSwap writes value to the named record if the record's index
// equals index, and returns the written record. Index 0 creates the record
// only if it is absent. Creating [RecordState] initializes the store.
//
// It fails with:
//   - [*MismatchError] when the record exists and its index differs,
//     index 0 included;
//   - [ErrNotFound] when the record is absent and index is not 0;
//   - [ErrUninitialized] on an uninitialized store, unless it creates
//     [RecordState].
func (s *Store) CompareAndSwap(name string, index uint64, value []byte) (Record, error) {
	if err := checkName(name); err != nil {
		return Record{}, fmt.Errorf("update record %q: %w", name, err)
	}

	now := time.Unix(0, s.now().UnixNano()).UTC()

	var rec Record

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b.Get([]byte(RecordState)) == nil && (name != RecordState || index != 0) {
			return ErrUninitialized
		}

		cur, err := get(b, name)

		var next uint64

		switch {
		case errors.Is(err, ErrNotFound) && index == 0:
			next = uint64(now.UnixNano())
		case err != nil:
			return err
		case cur.Index != index:
			return &MismatchError{Current: cur}
		default:
			next = cur.Index + 1
		}

		rec = Record{Value: bytes.Clone(value), Index: next, UpdateTime: now}

		return b.Put([]byte(name), encode(rec))
	})
	if err != nil {
		return Record{}, fmt.Errorf("update record %q: %w", name, err)
	}

	return rec, nil
}

// Initialized reports whether the store holds [RecordState].
func (s *Store) Initialized() (bool, error) {
	var ok bool

	err := s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bucket).Get([]byte(RecordState)) != nil

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read arbiter store: %w", err)
	}

	return ok, nil
}

// Check reports whether the store is open and its records can be read.
// An uninitialized store passes.
func (s *Store) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(k, v []byte) error {
			_, err := decode(string(k), v)

			return err
		})
	})
	if err != nil {
		return fmt.Errorf("read arbiter store: %w", err)
	}

	return nil
}

func checkName(name string) error {
	if name != RecordState && name != RecordLease {
		return ErrUnknownRecord
	}

	return nil
}

func get(b *bolt.Bucket, name string) (Record, error) {
	v := b.Get([]byte(name))
	if v == nil {
		return Record{}, ErrNotFound
	}

	return decode(name, v)
}

func encode(rec Record) []byte {
	v := make([]byte, headerSize, headerSize+len(rec.Value))
	v[0] = formatVersion
	binary.BigEndian.PutUint64(v[1:], rec.Index)
	binary.BigEndian.PutUint64(v[9:], uint64(rec.UpdateTime.UnixNano()))

	return append(v, rec.Value...)
}

// decode copies v: bbolt values are valid only inside the transaction.
func decode(name string, v []byte) (Record, error) {
	if len(v) < headerSize {
		return Record{}, fmt.Errorf("record %q is corrupt: %d bytes", name, len(v))
	}

	if v[0] != formatVersion {
		return Record{}, fmt.Errorf("record %q has unsupported format version %d", name, v[0])
	}

	return Record{
		Value:      bytes.Clone(v[headerSize:]),
		Index:      binary.BigEndian.Uint64(v[1:]),
		UpdateTime: time.Unix(0, int64(binary.BigEndian.Uint64(v[9:]))).UTC(),
	}, nil
}
