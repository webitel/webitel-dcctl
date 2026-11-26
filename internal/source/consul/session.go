package consul

import (
	"context"

	"github.com/hashicorp/consul/api"
)

// CreateSession creates a session with the configured TTL and lock delay, behavior
// release and no checks, not even the node's serfHealth: only its TTL or
// [Client.DestroySession] ends it.
func (c *Client) CreateSession(ctx context.Context, name string) (string, error) {
	entry := &api.SessionEntry{
		Name:      name,
		TTL:       c.sessionTTL.String(),
		LockDelay: c.lockDelay,
		Behavior:  api.SessionBehaviorRelease,
	}

	var id string

	_, err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		sid, _, err := client.Session().CreateNoChecks(entry, new(api.WriteOptions).WithContext(ctx))
		if err != nil {
			return err
		}

		id = sid

		return nil
	})

	return id, err
}

// RenewSession resets the TTL of the session, or returns [ErrSessionNotFound].
func (c *Client) RenewSession(ctx context.Context, id string) error {
	var found bool

	_, err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		entry, _, err := client.Session().Renew(id, new(api.WriteOptions).WithContext(ctx))
		if err != nil {
			return err
		}

		found = entry != nil

		return nil
	})
	if err != nil {
		return err
	}

	if !found {
		return ErrSessionNotFound
	}

	return nil
}

// DestroySession invalidates the session, which releases its keys after the lock delay.
func (c *Client) DestroySession(ctx context.Context, id string) error {
	_, err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		_, err := client.Session().Destroy(id, new(api.WriteOptions).WithContext(ctx))

		return err
	})

	return err
}

// Acquire locks key for session, writing value, and reports whether it got the lock. A
// session that is gone gives [ErrSessionNotFound]. It is repeatable, so it fails over.
func (c *Client) Acquire(ctx context.Context, key string, value []byte, session string) (bool, error) {
	return c.lock(ctx, true, key, value, session, (*api.KV).Acquire)
}

// Release unlocks key held by session, writing value, and reports whether it did. A
// session that is gone gives [ErrSessionNotFound]; a failure after the request was sent
// matches [ErrOutcomeUnknown].
func (c *Client) Release(ctx context.Context, key string, value []byte, session string) (bool, error) {
	return c.lock(ctx, false, key, value, session, (*api.KV).Release)
}

func (c *Client) lock(
	ctx context.Context,
	idempotent bool,
	key string,
	value []byte,
	session string,
	op func(*api.KV, *api.KVPair, *api.WriteOptions) (bool, *api.WriteMeta, error),
) (bool, error) {
	var ok bool

	_, err := c.pool.do(ctx, idempotent, func(ctx context.Context, client *api.Client) error {
		done, _, err := op(
			client.KV(),
			&api.KVPair{Key: key, Value: value, Session: session},
			new(api.WriteOptions).WithContext(ctx),
		)
		if err != nil {
			return invalidSession(err)
		}

		ok = done

		return nil
	})

	return ok, err
}
