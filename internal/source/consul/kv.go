package consul

import (
	"context"

	"github.com/hashicorp/consul/api"
)

// KVEntry is a key of the Consul KV with the index it was last modified at.
type KVEntry struct {
	Key         string
	Value       []byte
	ModifyIndex uint64
	// Session is the session holding the key as a lock; empty when none does.
	Session string
}

func kvEntry(p *api.KVPair) KVEntry {
	return KVEntry{Key: p.Key, Value: p.Value, ModifyIndex: p.ModifyIndex, Session: p.Session}
}

// KVGet returns the key with its ModifyIndex, or [ErrKeyNotFound].
func (c *Client) KVGet(ctx context.Context, key string) (*KVEntry, error) {
	var pair *api.KVPair

	err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		p, _, err := client.KV().Get(key, new(api.QueryOptions).WithContext(ctx))
		if err != nil {
			return err
		}

		pair = p

		return nil
	})
	if err != nil {
		return nil, err
	}

	if pair == nil {
		return nil, ErrKeyNotFound
	}

	e := kvEntry(pair)

	return &e, nil
}

// KVList returns the keys under prefix with their ModifyIndex; none is not an error.
func (c *Client) KVList(ctx context.Context, prefix string) ([]KVEntry, error) {
	var pairs api.KVPairs

	err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		p, _, err := client.KV().List(prefix, new(api.QueryOptions).WithContext(ctx))
		if err != nil {
			return err
		}

		pairs = p

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]KVEntry, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, kvEntry(p))
	}

	return out, nil
}

// KVCAS writes value only if the ModifyIndex of the key is still modifyIndex, and
// reports whether it did. Index 0 writes only when the key does not exist.
//
// A failure after the request was sent matches [ErrOutcomeUnknown]: re-read the key.
func (c *Client) KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error) {
	var ok bool

	err := c.pool.do(ctx, false, func(ctx context.Context, client *api.Client) error {
		done, _, err := client.KV().CAS(
			&api.KVPair{Key: key, Value: value, ModifyIndex: modifyIndex},
			new(api.WriteOptions).WithContext(ctx),
		)
		if err != nil {
			return err
		}

		ok = done

		return nil
	})

	return ok, err
}

// KVDeleteCAS deletes the key only if its ModifyIndex is still modifyIndex, and reports
// whether it did. A failure after the request was sent matches [ErrOutcomeUnknown].
func (c *Client) KVDeleteCAS(ctx context.Context, key string, modifyIndex uint64) (bool, error) {
	var ok bool

	err := c.pool.do(ctx, false, func(ctx context.Context, client *api.Client) error {
		done, _, err := client.KV().DeleteCAS(
			&api.KVPair{Key: key, ModifyIndex: modifyIndex},
			new(api.WriteOptions).WithContext(ctx),
		)
		if err != nil {
			return err
		}

		ok = done

		return nil
	})

	return ok, err
}
