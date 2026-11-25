package collect_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
)

func TestCollect_readsAfreshWithoutLastKnownData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_a"].version = "1.7.2"

		c := newCollector(t, w)
		start(t, c)

		loop := waitSeq(c, 1)
		require.True(t, loop.Datacenters[0].Nomad.Reachable)

		w.nomad["dc_a"].versionCall.set(errRefused, false)

		fresh, err := c.Collect(t.Context())
		require.NoError(t, err)

		n := fresh.Datacenters[0].Nomad
		assert.False(t, n.Reachable)
		assert.Equal(t, source.ReasonRefused, n.Reason)
		require.ErrorIs(t, n.Err, errRefused)
		assert.Empty(t, n.Data.Version, "no data of an earlier cycle")
		assert.True(t, n.ObserveTime.IsZero())
		assert.True(t, fresh.Datacenters[1].Nomad.Reachable)
		assert.Zero(t, fresh.Seq)
		assert.Equal(t, collect.Instance{Datacenter: "dc_a", Node: "dcctl-a1"}, fresh.Observer)

		assert.Same(t, loop, c.Snapshot(), "the fresh snapshot is not published")
	})
}

func TestCollect_boundedByRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.patroni[1].membersCall.set(nil, true)

		c := newCollector(t, w)
		begin := time.Now()

		s, err := c.Collect(t.Context())
		require.NoError(t, err)

		assert.Equal(t, requestTimeout, time.Since(begin))
		assert.False(t, s.Clusters[1].Patroni.Reachable)
		assert.Equal(t, source.ReasonTimeout, s.Clusters[1].Patroni.Reason)
	})
}

func TestCollect_canceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.patroni[0].membersCall.set(nil, true)

		c := newCollector(t, w)

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		s, err := c.Collect(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, s)
	})
}

func TestCollect_doesNotLogReachability(t *testing.T) {
	w := newWorld(config.ProfileWarmStandby)
	w.nomad["dc_a"].versionCall.set(errRefused, false)

	rec := logtest.NewRecorder()
	c := collect.New(collect.Config{
		Profile: w.profile,
		Status:  config.Status{Interval: interval, RequestTimeout: requestTimeout},
		Scope:   "pg",
	}, w.sources(), logging.New(logging.WithProvider(rec)))

	_, err := c.Collect(t.Context())
	require.NoError(t, err)

	assert.Empty(t, rec.Records(), "the loop logs when a source stops answering")
}
