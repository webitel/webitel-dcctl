package journal_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

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

// gate is a Consul client that can be cut off, as a datacenter that does not
// answer.
type gate struct {
	journal.Client

	down atomic.Bool
}

func (g *gate) KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error) {
	if g.down.Load() {
		return nil, consul.QueryMeta{}, unreachable()
	}

	return g.Client.KVGet(ctx, key)
}

func (g *gate) KVList(ctx context.Context, prefix string) ([]consul.KVEntry, error) {
	if g.down.Load() {
		return nil, unreachable()
	}

	return g.Client.KVList(ctx, prefix)
}

func (g *gate) KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error) {
	if g.down.Load() {
		return false, unreachable()
	}

	return g.Client.KVCAS(ctx, key, value, modifyIndex)
}

func (g *gate) KVDeleteCAS(ctx context.Context, key string, modifyIndex uint64) (bool, error) {
	if g.down.Load() {
		return false, unreachable()
	}

	return g.Client.KVDeleteCAS(ctx, key, modifyIndex)
}

func TestIntegrationJournal(t *testing.T) {
	ctx := t.Context()
	addrA, addrB := startConsul(t), startConsul(t)

	clients, err := consul.New(consul.Options{
		Profile:     config.ProfileWarmStandby,
		NodeMetaKey: "webitel_datacenter",
		Servers: []config.ConsulServer{
			{Address: addrA, Datacenter: "dc_a"},
			{Address: addrB, Datacenter: "dc_b"},
		},
		SessionTTL: 10 * time.Second,
		LockDelay:  6 * time.Second,
		Timeout:    5 * time.Second,
	})
	require.NoError(t, err)

	named, err := coord.NamedClients(config.ProfileWarmStandby, clients)
	require.NoError(t, err)

	gateB := &gate{Client: named[1].Client}
	kvs := []journal.KV{{Name: "dc_a", Client: named[0].Client}, {Name: "dc_b", Client: gateB}}

	j := newJournal(t, 5, kvs...)

	// dc_b does not answer for the first entries.
	gateB.down.Store(true)

	for range 3 {
		got, err := j.Append(ctx, operationEntry())
		require.NoError(t, err)
		require.Equal(t, 1, got.Stored())
		require.Equal(t, journal.Failed, got.Writes[1].Outcome)
	}

	gateB.down.Store(false)

	for range 4 {
		got, err := j.Append(ctx, operationEntry())
		require.NoError(t, err)
		require.Equal(t, 2, got.Stored())
	}

	all := list(t, j, 0, "", "")
	require.Len(t, all.GetJournalEntries(), 7, "dc_a holds all of them")

	aligned, err := j.Align(ctx)
	require.NoError(t, err)
	assert.Equal(t, []journal.Alignment{{DC: "dc_a"}, {DC: "dc_b", Copied: 1}}, aligned,
		"dc_b gets the newest of the entries it missed, up to max_entries")

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			got, err := j.Trim(ctx)
			assert.NoError(t, err)

			for _, tr := range got {
				assert.NoError(t, tr.Err)
			}
		})
	}

	wg.Wait()

	left := list(t, j, 0, "", "")
	assert.Equal(t, ids(all)[:5], ids(left), "concurrent trimming keeps exactly the newest max_entries")

	for _, n := range named {
		entries, err := n.Client.KVList(ctx, kv.JournalPrefix)
		require.NoError(t, err)
		assert.Len(t, entries, 5, n.Name)
	}

	_, err = j.Append(ctx, alarmEntry())
	require.NoError(t, err)

	alarms := list(t, j, 0, `type = "ALARM"`, "")
	require.Len(t, alarms.GetJournalEntries(), 1)
	assert.Equal(t, dcctlv1.JournalEntry_ERROR, alarms.GetJournalEntries()[0].GetResult())
}
