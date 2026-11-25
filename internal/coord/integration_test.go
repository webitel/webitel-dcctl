package coord_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// integrationTTL is the shortest TTL Consul accepts; the lock delay is the
// shortest the locker accepts with it.
const (
	integrationTTL       = 10 * time.Second
	integrationLockDelay = 6 * time.Second
)

var integrationLock = coord.LockOptions{TTL: integrationTTL, LockDelay: integrationLockDelay, Timeout: 2 * time.Second}

// startConsul runs `consul agent -dev` in a container and returns its HTTP
// address. The test is skipped when Docker is not available.
func startConsul(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("integration test needs Docker")
	}

	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "hashicorp/consul:1.15",
			ExposedPorts: []string{"8500/tcp"},
			WaitingFor:   wait.ForHTTP("/v1/status/leader").WithPort("8500/tcp"),
			Cmd:          []string{"agent", "-dev", "-client", "0.0.0.0"},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "8500")
	require.NoError(t, err)

	return host + ":" + port.Port()
}

func integrationKVs(t *testing.T, profile config.Profile, servers ...config.ConsulServer) ([]coord.KV, *consul.Clients) {
	t.Helper()

	clients, err := consul.New(consul.Options{
		Profile:     profile,
		NodeMetaKey: "webitel_datacenter",
		Servers:     servers,
		SessionTTL:  integrationTTL,
		LockDelay:   integrationLockDelay,
		Timeout:     5 * time.Second,
	})
	require.NoError(t, err)

	kvs, err := coord.NewKVs(profile, clients)
	require.NoError(t, err)

	return kvs, clients
}

func integrationLocker(t *testing.T, kvs []coord.KV) *coord.Locker {
	t.Helper()

	l, err := coord.NewLocker(kvs, integrationLock, quietLog())
	require.NoError(t, err)

	return l
}

func TestIntegrationLockWarmStandby(t *testing.T) {
	ctx := t.Context()
	kvs, clients := integrationKVs(t, config.ProfileWarmStandby,
		config.ConsulServer{Address: startConsul(t), Datacenter: "dc_a"},
		config.ConsulServer{Address: startConsul(t), Datacenter: "dc_b"},
	)

	var (
		wg      sync.WaitGroup
		lockers = [2]*coord.Locker{integrationLocker(t, kvs), integrationLocker(t, kvs)}
		locks   [2]*coord.Lock
		errs    [2]error
	)

	for i, l := range lockers {
		wg.Go(func() {
			locks[i], errs[i] = l.Acquire(ctx, lockHolder(fmt.Sprintf("node-%d", i), fmt.Sprintf("op-%d", i)))
		})
	}

	wg.Wait()

	winner := slices.IndexFunc(errs[:], func(err error) bool { return err == nil })
	require.NotEqual(t, -1, winner, "one instance gets the lock: %v", errs)

	var busy *coord.LockBusyError
	require.ErrorAs(t, errs[1-winner], &busy)
	assert.Equal(t, fmt.Sprintf("node-%d", winner), busy.Holder.Instance)
	assert.Equal(t, []string{"dc_a", "dc_b"}, locks[winner].Held())

	released := locks[winner].Release(ctx)
	assert.Equal(t, map[string]coord.ReleaseOutcome{"dc_a": coord.ReleaseDone, "dc_b": coord.ReleaseDone}, outcomes(released))

	// Another holder in dc_b only: the lock taken in dc_a is given back.
	b, err := clients.Client("dc_b")
	require.NoError(t, err)

	other, err := b.CreateSession(ctx, "other")
	require.NoError(t, err)

	value, err := json.Marshal(lockHolder("node-x", "op-x"))
	require.NoError(t, err)

	ok, err := b.Acquire(ctx, kv.LockKey, value, other)
	require.NoError(t, err)
	require.True(t, ok)

	_, err = integrationLocker(t, kvs).Acquire(ctx, lockHolder("node-0", "op-2"))
	require.ErrorAs(t, err, &busy)
	assert.Equal(t, "dc_b", busy.Name)
	assert.Equal(t, "node-x", busy.Holder.Instance)

	a, err := clients.Client("dc_a")
	require.NoError(t, err)

	e, _, err := a.KVGet(ctx, kv.LockKey)
	require.NoError(t, err)
	assert.Empty(t, e.Session, "the lock in dc_a is released")
}

func TestIntegrationLockStretchLost(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)
	kvs, clients := integrationKVs(t, config.ProfileStretch,
		config.ConsulServer{Address: addr, Datacenter: "dc_a"},
		config.ConsulServer{Address: addr, Datacenter: "dc_b"},
	)

	lk, err := integrationLocker(t, kvs).Acquire(ctx, lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	assert.Equal(t, []string{coord.StretchName}, lk.Held(), "one lock in the global consul")

	g := clients.Voters()[0]

	e, _, err := g.KVGet(ctx, kv.LockKey)
	require.NoError(t, err)
	require.NotEmpty(t, e.Session)

	// Past a TTL the renewal keeps the session alive.
	time.Sleep(integrationTTL + 2*time.Second)
	require.NoError(t, lk.Err())

	// An invalidated session is reported like an expired one.
	require.NoError(t, g.DestroySession(ctx, e.Session))

	select {
	case <-lk.Lost():
	case <-time.After(integrationTTL):
		require.Fail(t, "a lost session is reported")
	}

	require.ErrorIs(t, lk.Err(), coord.ErrLockLost)
	require.ErrorIs(t, lk.Err(), consul.ErrSessionNotFound)

	assert.Equal(t, map[string]coord.ReleaseOutcome{coord.StretchName: coord.ReleaseNotHeld}, outcomes(lk.Release(ctx)))
}

func TestIntegrationLockDelay(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)
	kvs, clients := integrationKVs(t, config.ProfileStretch,
		config.ConsulServer{Address: addr, Datacenter: "dc_a"},
	)

	lk, err := integrationLocker(t, kvs).Acquire(ctx, lockHolder("node-a", "op-a"))
	require.NoError(t, err)

	g := clients.Voters()[0]

	e, _, err := g.KVGet(ctx, kv.LockKey)
	require.NoError(t, err)
	require.NoError(t, g.DestroySession(ctx, e.Session))

	other := integrationLocker(t, kvs)

	_, err = other.Acquire(ctx, lockHolder("node-b", "op-b"))

	var busy *coord.LockBusyError
	require.ErrorAs(t, err, &busy)
	assert.True(t, busy.LockDelay)
	assert.Equal(t, kv.Lock{}, busy.Holder, "the holder whose session ended is not named")

	var got *coord.Lock

	assert.Eventually(t, func() bool {
		got, err = other.Acquire(ctx, lockHolder("node-b", "op-b"))

		return err == nil
	}, 2*integrationLockDelay, 500*time.Millisecond, "the lock is free after the lock delay")

	if got != nil {
		got.Release(ctx)
	}

	lk.Release(ctx)
}

func TestIntegrationOpRecord(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)
	kvs, clients := integrationKVs(t, config.ProfileWarmStandby,
		config.ConsulServer{Address: addr, Datacenter: "dc_a"},
	)

	a := clients.Voters()[0]

	var prev map[string]any
	require.NoError(t, json.Unmarshal(opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_a")), &prev))
	prev["future"] = "kept"

	raw, err := json.Marshal(prev)
	require.NoError(t, err)

	ok, err := a.KVCAS(ctx, kv.OpKey, raw, 0)
	require.NoError(t, err)
	require.True(t, ok)

	store, err := coord.NewOpStore(kvs, 5*time.Second, quietLog())
	require.NoError(t, err)

	got, err := store.Write(ctx, operation("1", dcctlv1.Operation_INTERRUPTED, "dc_a"))
	require.NoError(t, err)
	assert.Equal(t, []coord.OpWrite{{Name: "dc_a", Primary: true, Outcome: coord.OpWritten}}, got)

	v, err := store.Read(ctx)
	require.NoError(t, err)
	require.NotNil(t, v.Primary())
	assert.Equal(t, dcctlv1.Operation_INTERRUPTED, v.Primary().Operation.GetState())
	assert.Contains(t, string(v.Primary().Raw), `"future":"kept"`)

	var running *coord.OperationRunningError
	require.ErrorAs(t, v.CheckIdle(), &running)
}
