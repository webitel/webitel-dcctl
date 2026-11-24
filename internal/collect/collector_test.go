package collect_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

func TestCycle_hungSourceBoundedByRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_b"].jobsCall.set(nil, true)

		c := newCollector(t, w)
		begin := time.Now()

		start(t, c)

		s := waitSeq(c, 1)
		assert.Equal(t, requestTimeout, s.CycleDuration)
		assert.Equal(t, begin.Add(requestTimeout), s.ObserveTime)

		b, ok := s.Datacenter("dc_b")
		require.True(t, ok)
		assert.False(t, b.Nomad.Reachable)
		assert.Equal(t, source.ReasonTimeout, b.Nomad.Reason)
		assert.True(t, b.Services.Reachable)

		a, ok := s.Datacenter("dc_a")
		require.True(t, ok)
		assert.True(t, a.Nomad.Reachable)
	})
}

func TestCycle_snapshotHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCollector(t, newWorld(config.ProfileWarmStandby))
		start(t, c)

		s := waitSeq(c, 1)
		assert.Equal(t, uint64(1), s.Seq)
		assert.Equal(t, collect.Instance{Datacenter: "dc_a", Node: "dcctl-a1"}, s.Observer)
		assert.Equal(t, config.ProfileWarmStandby, s.Profile)
		assert.Nil(t, s.Arbiter, "no arbiter is configured")
		require.Len(t, s.Datacenters, 2)
		assert.Equal(t, "dc_a", s.Datacenters[0].Name)
		assert.Equal(t, "dc_b", s.Datacenters[1].Name)
		require.Len(t, s.Voters, 2)
		require.Len(t, s.Clusters, 2)
	})
}

func TestCycle_unreachableKeepsLastData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_a"].version = "1.7.2"

		c := newCollector(t, w)
		start(t, c)

		first := waitSeq(c, 1)
		require.True(t, first.Datacenters[0].Nomad.Reachable)

		w.nomad["dc_a"].versionCall.set(errRefused, false)

		second := waitSeq(c, 2)
		n := second.Datacenters[0].Nomad
		assert.False(t, n.Reachable)
		assert.Equal(t, source.ReasonRefused, n.Reason)
		require.ErrorIs(t, n.Err, errRefused)
		assert.Equal(t, first.ObserveTime.Add(interval), n.UnreachableSince)
		assert.Equal(t, "1.7.2", n.Data.Version, "last known data")
		assert.Equal(t, first.ObserveTime, n.ObserveTime, "time of the last known data")

		third := waitSeq(c, 3)
		assert.Equal(t, n.UnreachableSince, third.Datacenters[0].Nomad.UnreachableSince)

		w.nomad["dc_a"].versionCall.set(nil, false)

		fourth := waitSeq(c, 4)
		n = fourth.Datacenters[0].Nomad
		assert.True(t, n.Reachable)
		assert.Empty(t, n.Reason)
		assert.NoError(t, n.Err)
		assert.True(t, n.UnreachableSince.IsZero())
		assert.Equal(t, fourth.ObserveTime, n.ObserveTime)
	})
}

func TestCycle_nomadFailureIsNotHiddenByCanceledSiblings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_a"].jobsCall.set(nil, true)
		w.nomad["dc_a"].nodesCall.set(errRefused, false)

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)
		assert.Equal(t, source.ReasonRefused, s.Datacenters[0].Nomad.Reason)
		assert.Zero(t, s.CycleDuration, "the cycle does not wait for the rest of a failed source")
	})
}

func TestCycle_nomad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		f := w.nomad["dc_a"]
		f.managed = []nomad.Job{{ID: "storage", Stage: 1}, {ID: "engine", Stage: 2}}
		f.unmanaged = []nomad.Job{{ID: "node-exporter"}}
		f.allocs = map[string][]nomad.Alloc{"engine": {{ID: "a1", ClientStatus: "running"}}}
		f.nodes = []nomad.Node{{ID: "n1"}}

		c := newCollector(t, w)
		start(t, c)

		n := waitSeq(c, 1).Datacenters[0].Nomad
		require.True(t, n.Reachable)
		assert.Equal(t, "1.10.0", n.Data.Version)
		assert.Equal(t, f.managed, n.Data.Managed)
		assert.Equal(t, f.unmanaged, n.Data.Unmanaged)
		assert.Equal(t, f.nodes, n.Data.Nodes)
		assert.Equal(t, map[string][]nomad.Alloc{"storage": nil, "engine": f.allocs["engine"]}, n.Data.Allocs)
		assert.Len(t, f.allocsCall.calls(), 2, "allocations of managed jobs only")
	})
}

func TestCycle_kv(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		state := &consul.KVEntry{Key: kv.StateKey, Value: []byte(`{"generation":3}`), ModifyIndex: 7}
		w.kv["dc_a"].entries[kv.StateKey] = state
		w.kv["dc_b"].set(errRefused, false)

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)
		require.Len(t, s.Voters, 2)
		assert.Equal(t, "dc_a", s.Voters[0].Datacenter)
		assert.True(t, s.Voters[0].KV.Reachable, "a missing key is not a failure")
		assert.Equal(t, collect.KV{State: state, Meta: leader}, s.Voters[0].KV.Data)
		assert.False(t, s.Voters[1].KV.Reachable)
		assert.Equal(t, source.ReasonRefused, s.Voters[1].KV.Reason)
	})
}

// A Consul server that lost its leader still answers a stale read: the voter is reachable
// and tells it has no leader.
func TestCycle_kvWithoutConsulLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.kv["dc_b"].meta = consul.QueryMeta{KnownLeader: false, LastContact: 20 * time.Second}
		w.kv["dc_b"].entries[kv.StateKey] = &consul.KVEntry{Key: kv.StateKey, Value: []byte(`{}`)}

		c := newCollector(t, w)
		start(t, c)

		v := waitSeq(c, 1).Voters[1].KV
		require.True(t, v.Reachable)
		assert.Equal(t, consul.QueryMeta{KnownLeader: false, LastContact: 20 * time.Second}, v.Data.Meta)
		assert.NotNil(t, v.Data.State)
	})
}

func TestCycle_arbiter(t *testing.T) {
	state := arbiter.Record{Value: []byte(`{"generation":3}`), Etag: 11}

	tests := []struct {
		name    string
		arbiter *fakeArbiter
		want    collect.Arbiter
	}{
		{
			name:    "lease_missing",
			arbiter: &fakeArbiter{records: map[string]arbiter.Record{arbiter.RecordState: state}},
			want:    collect.Arbiter{Initialized: true, State: &state},
		},
		{
			name:    "uninitialized",
			arbiter: &fakeArbiter{uninitialized: true},
			want:    collect.Arbiter{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(config.ProfileWarmStandby)
				w.arbiter = tt.arbiter

				c := newCollector(t, w)
				start(t, c)

				s := waitSeq(c, 1)
				require.NotNil(t, s.Arbiter)
				assert.True(t, s.Arbiter.Reachable)
				assert.Equal(t, tt.want, s.Arbiter.Data)
			})
		})
	}
}

func TestCycle_hungArbiterIsUnreachableNotMissing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.arbiter = &fakeArbiter{}
		w.arbiter.set(nil, true)

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)
		require.NotNil(t, s.Arbiter, "a configured arbiter is always in the snapshot")
		assert.False(t, s.Arbiter.Reachable)
		assert.Equal(t, source.ReasonTimeout, s.Arbiter.Reason)
	})
}

// A client that reports the end of the cycle as context.Canceled still leaves a reason.
func TestCycle_canceledByTheCycleIsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_a"].versionCall.set(nil, true)
		w.nomad["dc_b"].versionCall.setDelay(time.Second)
		w.nomad["dc_b"].nodesCall.set(context.Canceled, false)

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)
		for _, d := range s.Datacenters {
			assert.False(t, d.Nomad.Reachable, d.Name)
			assert.Equal(t, source.ReasonTimeout, d.Nomad.Reason, d.Name)
		}
	})
}

func TestCycle_stretchServicesPerDatacenter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileStretch)
		a := consul.ServiceNode{ID: "rmq-a", NodeMeta: map[string]string{nodeMetaKey: "dc_a"}}
		b := consul.ServiceNode{ID: "rmq-b", NodeMeta: map[string]string{nodeMetaKey: "dc_b"}}
		w.services["dc_a"].byName = map[string][]consul.ServiceNode{"rabbitmq-dc_a": {a}, "engine": {a}}
		w.services["dc_b"].byName = map[string][]consul.ServiceNode{"rabbitmq-dc_b": {b}, "engine": {b}}

		c := newCollector(t, w)
		start(t, c)

		s := waitSeq(c, 1)
		require.Len(t, s.Voters, 1, "one voter in stretch")
		require.Len(t, s.Clusters, 1, "one cluster in stretch")
		assert.Equal(t, datacenters, s.Clusters[0].Datacenters)

		dcB, ok := s.Datacenter("dc_b")
		require.True(t, ok)
		require.True(t, dcB.Services.Reachable)
		assert.Equal(t, []consul.ServiceNode{b}, dcB.Services.Data.RabbitMQ)
		assert.Equal(t, []consul.ServiceNode{b}, dcB.Services.Data.Info["engine"])
		assert.Empty(t, dcB.Services.Data.Info["storage"])

		assert.ElementsMatch(t,
			[]string{"nomad-dc_b", "webitel-dcctl", "rabbitmq-dc_b", "engine", "storage"},
			w.services["dc_b"].names())
	})
}

func TestRun_nextCycleStartsIntervalAfterThePreviousEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_a"].versionCall.setDelay(time.Second)

		c := newCollector(t, w)
		begin := time.Now()

		start(t, c)

		s := waitSeq(c, 3)
		assert.Equal(t, time.Second, s.CycleDuration)

		step := time.Second + interval
		assert.Equal(t,
			[]time.Time{begin, begin.Add(step), begin.Add(2 * step)},
			w.nomad["dc_a"].versionCall.calls())
	})
}

func TestRun_cancelStopsWithoutPublishing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(config.ProfileWarmStandby)
		w.nomad["dc_a"].jobsCall.set(nil, true)

		c := newCollector(t, w)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			defer close(done)

			c.Run(ctx)
		}()

		synctest.Wait()
		cancel()
		<-done

		assert.Nil(t, c.Snapshot(), "an aborted cycle is not published")
	})
}

func TestWatch_signalsTheNextSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCollector(t, newWorld(config.ProfileWarmStandby))

		s, next := c.Watch()
		assert.Nil(t, s)

		start(t, c)
		<-next

		s, next = c.Watch()
		require.NotNil(t, s)
		assert.Equal(t, uint64(1), s.Seq)

		synctest.Wait()

		select {
		case <-next:
			t.Fatal("signaled before the next cycle")
		default:
		}

		time.Sleep(interval)
		synctest.Wait()

		select {
		case <-next:
		default:
			t.Fatal("not signaled after the next cycle")
		}
	})
}

func TestCheck_liveness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCollector(t, newWorld(config.ProfileWarmStandby))
		stale := 3 * (interval + requestTimeout)

		require.NoError(t, c.Check(t.Context()))

		time.Sleep(stale)
		require.NoError(t, c.Check(t.Context()), "at the threshold")

		time.Sleep(time.Second)
		require.Error(t, c.Check(t.Context()), "no cycle for longer than the threshold")

		start(t, c)
		waitSeq(c, 1)
		require.NoError(t, c.Check(t.Context()))
	})
}

func TestSnapshot_readersSeeWholeSnapshots(t *testing.T) {
	w := newWorld(config.ProfileWarmStandby)
	c := collect.New(collect.Config{
		Profile: w.profile,
		Status: config.Status{
			Interval:       time.Millisecond,
			RequestTimeout: time.Second,
			InfoServices:   []string{"engine"},
		},
		Scope: "pg",
	}, w.sources(), logging.New(logging.WithProvider(logtest.NewRecorder())))

	start(t, c)

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			var last uint64

			for last < 20 {
				s := waitSeq(c, last+1)
				assert.Greater(t, s.Seq, last)
				assert.Len(t, s.Datacenters, 2)
				assert.Len(t, s.Voters, 2)
				assert.Len(t, s.Clusters, 2)

				for _, d := range s.Datacenters {
					assert.Contains(t, d.Services.Data.Info, "engine")
				}

				last = s.Seq
			}
		})
	}

	wg.Wait()
}
