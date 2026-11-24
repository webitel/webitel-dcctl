package consul

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
)

func twoDatacenters(t *testing.T, profile config.Profile, a, b string) *Clients {
	t.Helper()

	c, err := New(Options{
		Profile:     profile,
		NodeMetaKey: "webitel_datacenter",
		Servers: []config.ConsulServer{
			{Address: a, Datacenter: "dc_a"},
			{Address: b, Datacenter: "dc_b"},
		},
		SessionTTL: 15 * time.Second,
		LockDelay:  15 * time.Second,
		Timeout:    500 * time.Millisecond,
	})
	require.NoError(t, err)

	return c
}

func TestStretchSharesOneCluster(t *testing.T) {
	ctx := t.Context()

	t.Run("dc_a_answers_through_the_dc_b_server", func(t *testing.T) {
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := twoDatacenters(t, config.ProfileStretch, refusedAddr(t), ok.addr())

		addrs, err := c.HealthyAddrs(ctx, "dc_a", "web", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.1.1:8080"}, addrs)
	})

	t.Run("warm_standby_keeps_a_cluster_per_datacenter", func(t *testing.T) {
		ok := newFake(t, respond(http.StatusOK, healthBody))
		c := twoDatacenters(t, config.ProfileWarmStandby, refusedAddr(t), ok.addr())

		_, err := c.HealthyAddrs(ctx, "dc_a", "web", "")

		reason, found := source.ReasonOf(err)
		require.True(t, found)
		assert.Equal(t, source.ReasonRefused, reason)
	})

	t.Run("tag_is_passed", func(t *testing.T) {
		var tag string

		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			tag = r.URL.Query().Get("tag")
			_, _ = w.Write([]byte(healthBody))
		})
		c := twoDatacenters(t, config.ProfileWarmStandby, f.addr(), f.addr())

		_, err := c.HealthyAddrs(ctx, "dc_a", "nomad", "http")
		require.NoError(t, err)
		assert.Equal(t, "http", tag)
	})
}

func TestVoters(t *testing.T) {
	t.Run("stretch_has_one", func(t *testing.T) {
		c := twoDatacenters(t, config.ProfileStretch, "10.1.0.89:8500", "10.2.0.89:8500")

		assert.Len(t, c.Voters(), 1)
		assert.Equal(t, []string{"10.1.0.89:8500", "10.2.0.89:8500"}, poolAddrs(c.Voters()[0].pool))
	})

	t.Run("warm_standby_has_one_per_datacenter", func(t *testing.T) {
		c := twoDatacenters(t, config.ProfileWarmStandby, "10.1.0.89:8500", "10.2.0.89:8500")

		voters := c.Voters()
		require.Len(t, voters, 2)
		assert.Equal(t, "dc_a", voters[0].Datacenter())
		assert.Equal(t, "dc_b", voters[1].Datacenter())
	})
}

func TestOutcomeUnknown(t *testing.T) {
	ctx := t.Context()

	var applied atomic.Int32

	first := newFake(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		applied.Add(1)
		<-r.Context().Done()
	})
	second := newFake(t, respond(http.StatusOK, "true"))
	c := newTestClient(t, first.addr(), second.addr())

	_, err := c.KVCAS(ctx, "dcctl/state", []byte("v"), 7)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	assert.Equal(t, int32(1), applied.Load())
	assert.Zero(t, second.hits.Load())

	reason, ok := source.ReasonOf(err)
	require.True(t, ok, "the error is still classified")
	assert.Equal(t, source.ReasonTimeout, reason)
}

func TestSessionGoneOnAcquire(t *testing.T) {
	first := newFake(t, respond(http.StatusInternalServerError, `rpc error: raft apply failed: invalid session "sid"`))
	second := newFake(t, respond(http.StatusOK, "true"))
	c := newTestClient(t, first.addr(), second.addr())

	_, err := c.Acquire(t.Context(), "dcctl/lock", nil, "sid")
	require.ErrorIs(t, err, ErrSessionNotFound)

	_, classified := source.ReasonOf(err)
	assert.False(t, classified)
	assert.Zero(t, second.hits.Load())
}

func TestStatusErrorKeepsTheChain(t *testing.T) {
	f := newFake(t, respond(http.StatusInternalServerError, "No cluster leader"))
	c := newTestClient(t, f.addr())

	_, err := c.Instances(t.Context(), "web", "", true)

	var se *source.StatusError

	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusInternalServerError, se.Code)
	assert.ErrorContains(t, err, "No cluster leader")
}

func poolAddrs(p *pool) []string {
	addrs := make([]string, len(p.endpoints))
	for i, ep := range p.endpoints {
		addrs[i] = ep.addr
	}

	return addrs
}
