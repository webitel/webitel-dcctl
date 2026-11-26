package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// Kind is the storage behind a voter.
type Kind string

const (
	KindKV      Kind = "kv"
	KindArbiter Kind = "arbiter"
)

// Names of the voters that are not a datacenter's KV.
const (
	ArbiterName = "arbiter"
	// StretchName is the one global Consul of the stretch profile.
	StretchName = "consul"
)

// Key is a record every voter holds.
type Key string

const (
	KeyState Key = "state"
	KeyLease Key = "lease"
)

// Version is the token a [Voter.CAS] compares against: the ModifyIndex of a
// KV key or the etag of an arbiter record. Zero means no record.
type Version uint64

// Entry is a record as a voter holds it.
type Entry struct {
	Value   []byte
	Version Version
}

var (
	// ErrNotFound means the voter answered and has no such record.
	ErrNotFound = errors.New("record not found")
	// ErrUninitialized means the voter is an arbiter that has no state yet
	// and does not vote.
	ErrUninitialized = errors.New("voter is not initialized")
	// ErrVersionMismatch means the record changed since it was read, or
	// the record to create exists. Read it again.
	ErrVersionMismatch = errors.New("record version mismatch")
	// ErrOutcomeUnknown marks a write that may have been applied. Read the
	// record again.
	ErrOutcomeUnknown = errors.New("outcome of the write is unknown")
	// ErrCreateRefused is returned for a create on the arbiter: only its
	// initialization creates its records, and an empty arbiter must not
	// start voting with whatever a dcctl instance sends it.
	ErrCreateRefused = errors.New("arbiter records are created only by its initialization")
)

// Voter is a store that votes on the shared state.
//
// Errors other than [ErrNotFound], [ErrUninitialized], [ErrVersionMismatch]
// and [ErrOutcomeUnknown] mean the voter could not be reached or used and
// are classified by [source.Classify].
type Voter interface {
	Name() string
	Kind() Kind
	Get(ctx context.Context, key Key) (Entry, error)
	// CAS writes value if the record is still at version; version 0 creates
	// the record only if it does not exist.
	CAS(ctx context.Context, key Key, value []byte, version Version) error
}

// NewVoters returns the voters of profile: the KV of every datacenter and
// the arbiter, if arb is not nil, in warm_standby; the one global Consul in
// stretch. The Consul clients must not allow stale reads: what a voter
// returns is its vote.
func NewVoters(profile config.Profile, clients *consul.Clients, arb *arbiter.Client) ([]Voter, error) {
	if clients == nil {
		return nil, errors.New("no consul clients")
	}

	kvs := clients.Voters()

	for _, c := range kvs {
		if c.AllowStale() {
			return nil, fmt.Errorf("consul client of %s allows stale reads", c.Datacenter())
		}
	}

	switch profile {
	case config.ProfileStretch:
		if arb != nil {
			return nil, fmt.Errorf("profile %s has no arbiter", profile)
		}

		if len(kvs) != 1 {
			return nil, fmt.Errorf("profile %s needs one consul voter, got %d", profile, len(kvs))
		}

		return []Voter{NewKVVoter(StretchName, kvs[0])}, nil
	case config.ProfileWarmStandby:
		voters := make([]Voter, 0, len(kvs)+1)
		for _, c := range kvs {
			voters = append(voters, NewKVVoter(c.Datacenter(), c))
		}

		if arb != nil {
			voters = append(voters, NewArbiterVoter(arb))
		}

		return voters, nil
	}

	return nil, fmt.Errorf("unknown profile %q", profile)
}

type kvVoter struct {
	name   string
	client *consul.Client
}

// NewKVVoter returns the voter over the Consul KV of client.
func NewKVVoter(name string, client *consul.Client) Voter {
	return &kvVoter{name: name, client: client}
}

func (v *kvVoter) Name() string { return v.name }

func (v *kvVoter) Kind() Kind { return KindKV }

func (v *kvVoter) Get(ctx context.Context, key Key) (Entry, error) {
	k, err := kvKey(key)
	if err != nil {
		return Entry{}, err
	}

	e, _, err := v.client.KVGet(ctx, k)

	switch {
	case errors.Is(err, consul.ErrKeyNotFound):
		return Entry{}, fmt.Errorf("%w: %w", ErrNotFound, err)
	case err != nil:
		return Entry{}, err
	}

	return Entry{Value: e.Value, Version: Version(e.ModifyIndex)}, nil
}

func (v *kvVoter) CAS(ctx context.Context, key Key, value []byte, version Version) error {
	k, err := kvKey(key)
	if err != nil {
		return err
	}

	ok, err := v.client.KVCAS(ctx, k, value, uint64(version))

	switch {
	case errors.Is(err, consul.ErrOutcomeUnknown):
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("%s %s at index %d: %w", v.name, k, version, ErrVersionMismatch)
	}

	return nil
}

func kvKey(key Key) (string, error) {
	switch key {
	case KeyState:
		return kv.StateKey, nil
	case KeyLease:
		return kv.LeaseKey, nil
	}

	return "", fmt.Errorf("unknown key %q", key)
}

type arbiterVoter struct {
	client *arbiter.Client
}

// NewArbiterVoter returns the voter over the arbiter of client. Its CAS
// never creates a record, see [ErrCreateRefused].
func NewArbiterVoter(client *arbiter.Client) Voter {
	return &arbiterVoter{client: client}
}

func (v *arbiterVoter) Name() string { return ArbiterName }

func (v *arbiterVoter) Kind() Kind { return KindArbiter }

func (v *arbiterVoter) Get(ctx context.Context, key Key) (Entry, error) {
	name, err := arbiterRecord(key)
	if err != nil {
		return Entry{}, err
	}

	rec, err := v.client.Get(ctx, name)
	if err != nil {
		return Entry{}, arbiterError(key, err)
	}

	return Entry{Value: rec.Value, Version: Version(rec.Etag)}, nil
}

func (v *arbiterVoter) CAS(ctx context.Context, key Key, value []byte, version Version) error {
	name, err := arbiterRecord(key)
	if err != nil {
		return err
	}

	if version == 0 {
		return fmt.Errorf("arbiter record %s: %w", name, ErrCreateRefused)
	}

	if _, err := v.client.CAS(ctx, name, uint64(version), value, false); err != nil {
		return arbiterError(key, err)
	}

	return nil
}

func arbiterRecord(key Key) (string, error) {
	switch key {
	case KeyState:
		return arbiter.RecordState, nil
	case KeyLease:
		return arbiter.RecordLease, nil
	}

	return "", fmt.Errorf("unknown key %q", key)
}

// arbiterError maps an error of the arbiter on the record of key. An
// arbiter without state does not vote, whatever it answers.
func arbiterError(key Key, err error) error {
	switch {
	case errors.Is(err, arbiter.ErrUninitialized),
		key == KeyState && errors.Is(err, arbiter.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrUninitialized, err)
	case errors.Is(err, arbiter.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, arbiter.ErrEtagMismatch):
		return fmt.Errorf("%w: %w", ErrVersionMismatch, err)
	case errors.Is(err, arbiter.ErrOutcomeUnknown):
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}

	return err
}
