package fencing_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/fencing"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

var allJobs = []string{"opensips", "engine", "freeswitch"}

func TestRunCondition(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
		edit  func(s *collect.Snapshot)
		// wantStops are the jobs stopped, in any order.
		wantStops []string
	}{
		{name: "other_active_running", wantStops: allJobs},
		{
			name: "own_active",
			setup: func(w *world) {
				for _, voter := range []string{dcA, dcB, coord.ArbiterName} {
					w.setState(voter, kv.State{Generation: 6, ActiveDatacenter: dcB})
				}
			},
		},
		{
			name: "no_majority",
			setup: func(w *world) {
				w.kvs[dcA].setDown(true)
				w.arb.setDown(true)
			},
		},
		{
			name: "uninitialized",
			setup: func(w *world) {
				for _, k := range w.kvs {
					k.mu.Lock()
					delete(k.entries, kv.StateKey)
					k.mu.Unlock()
				}

				w.arb.set(nil)
			},
		},
		{
			// A forced record is effective while a record of a higher generation that
			// names this datacenter may be on the voter that does not answer.
			name: "undetermined_forced_below",
			setup: func(w *world) {
				w.setState(dcA, kv.State{Generation: 5, ActiveDatacenter: dcA, Forced: true})
				w.setState(coord.ArbiterName, kv.State{Generation: 6, ActiveDatacenter: dcB})
				w.kvs[dcB].setDown(true)
			},
		},
		{
			name: "conflict",
			setup: func(w *world) {
				w.setState(dcA, kv.State{Generation: 6, ActiveDatacenter: dcA, Forced: true})
				w.setState(dcB, kv.State{Generation: 6, ActiveDatacenter: dcB, Forced: true})
			},
		},
		{name: "op_running", setup: func(w *world) { w.setOp(dcA, dcctlv1.Operation_RUNNING) }},
		{name: "op_interrupted", setup: func(w *world) { w.setOp(dcB, dcctlv1.Operation_INTERRUPTED) }},
		{name: "op_invalid", setup: func(w *world) { w.kvs[dcB].put(kv.OpKey, []byte("{")) }},
		{
			name:      "op_finished",
			setup:     func(w *world) { w.setOp(dcA, dcctlv1.Operation_SUCCEEDED) },
			wantStops: allJobs,
		},
		{
			// The data read a cycle ago shows the jobs running, but Nomad does not answer.
			name: "nomad_unreachable",
			edit: func(s *collect.Snapshot) {
				n := &s.Datacenters[1].Nomad
				n.Reachable, n.Reason, n.Err = false, source.ReasonRefused, errDown
				n.UnreachableSince = s.ObserveTime
				n.ObserveTime = s.ObserveTime.Add(-interval)
			},
		},
		{
			name: "nomad_stale",
			edit: func(s *collect.Snapshot) {
				s.Datacenters[1].Nomad.ObserveTime = s.ObserveTime.Add(-3*(interval+requestTimeout) - time.Second)
			},
		},
		{
			name: "all_stopped",
			setup: func(w *world) {
				for _, id := range allJobs {
					w.nomad.mu.Lock()
					w.nomad.jobs[id].Stop = true
					w.nomad.allocs[id] = nil
					w.nomad.mu.Unlock()
				}
			},
		},
		{
			// Only the KV of this datacenter answers, with a forced record naming dc_a;
			// the KV of dc_a and the arbiter, a majority, name this datacenter.
			name: "unseen_majority",
			setup: func(w *world) {
				w.setState(dcA, kv.State{Generation: 6, ActiveDatacenter: dcB})
				w.setState(coord.ArbiterName, kv.State{Generation: 6, ActiveDatacenter: dcB})
				w.setState(dcB, kv.State{Generation: 5, ActiveDatacenter: dcA, Forced: true})
				w.kvs[dcA].setDown(true)
				w.arb.setDown(true)
			},
		},
		{
			// opensips is stopped but its node does not answer; the rest is stopped.
			name: "only_unknown_left",
			setup: func(w *world) {
				w.nomad.mu.Lock()
				defer w.nomad.mu.Unlock()

				for _, id := range allJobs {
					w.nomad.jobs[id].Stop = true
					w.nomad.allocs[id] = nil
				}

				w.nomad.allocs["opensips"] = []nomad.Alloc{runningAlloc("opensips")}
				w.nomad.allocs["opensips"][0].ClientStatus = "unknown"
			},
		},
		{
			name:      "invalid_stage_running",
			setup:     func(w *world) { w.nomad.add(nomad.Job{ID: "legacy", Type: "service", StageErr: nomad.ErrInvalidStage}) },
			wantStops: append([]string{"legacy"}, allJobs...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				if tt.setup != nil {
					tt.setup(w)
				}

				stop := w.start()
				defer stop()

				w.publish(tt.edit)
				settle()

				assert.ElementsMatch(t, tt.wantStops, w.nomad.stopCalls())

				wantRuns := 0
				if len(tt.wantStops) > 0 {
					wantRuns = 1
				}

				assert.Len(t, w.entries(), wantRuns)
				assert.Len(t, w.stops(), wantRuns)
			})
		})
	}
}

func TestRunJournalAndMetric(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		stop := w.start()
		defer stop()

		w.publish(func(s *collect.Snapshot) {
			s.Clusters = []collect.Cluster{{
				Datacenters: []string{dcB},
				Patroni: collect.Source[collect.Patroni]{
					Reachable: true, ObserveTime: s.ObserveTime,
					Data: collect.Patroni{
						Leader: "pg-b",
						Members: []collect.Member{{
							Member:     patroni.Member{Name: "pg-b", Role: patroni.ClusterRoleLeader},
							Datacenter: dcB,
						}},
					},
				},
			}}
		})
		settle()

		for _, id := range allJobs {
			assert.True(t, w.nomad.stopped(id), id)
		}

		entries := w.entries()
		require.Len(t, entries, 1)

		e := entries[0]
		assert.Equal(t, dcctlv1.JournalEntry_SYSTEM, e.GetType())
		assert.Equal(t, dcctlv1.JournalEntry_FENCE_APPS, e.GetSystemAction())
		assert.Equal(t, dcctlv1.JournalEntry_OK, e.GetResult())
		assert.True(t, e.GetInitiator().GetSystem())
		assert.Empty(t, e.GetInitiator().GetUser())
		assert.Equal(t, node, e.GetInitiator().GetInstance().GetNode())
		assert.Equal(t, dcB, e.GetInitiator().GetInstance().GetDatacenter())
		assert.Equal(t, dcB, e.GetDatacenter())
		assert.Nil(t, e.GetError())

		params := e.GetParams().AsMap()
		assert.Equal(t, dcA, params["active_datacenter"])
		assert.InDelta(t, 5, params["generation"], 0)
		assert.Equal(t, "primary", params["patroni_role"])
		assert.Equal(t, []any{
			map[string]any{"job": "opensips", "stage": float64(3), "state": "stopped"},
			map[string]any{"job": "engine", "stage": float64(2), "state": "stopped"},
			map[string]any{"job": "freeswitch", "stage": float64(1), "state": "stopped"},
		}, params["jobs"])

		points := w.stops()
		require.Len(t, points, 1)
		assert.Equal(t, int64(1), points[0].Value)
		assert.Equal(t, attribute.NewSet(attribute.String("webitel.datacenter.name", dcB)), points[0].Attributes)
	})
}

func TestRunGuardRefusesMidRun(t *testing.T) {
	tests := []struct {
		name   string
		change func(w *world)
	}{
		{
			name: "own_active",
			change: func(w *world) {
				for _, voter := range []string{dcA, dcB, coord.ArbiterName} {
					w.setState(voter, kv.State{Generation: 6, ActiveDatacenter: dcB})
				}
			},
		},
		{name: "operation_started", change: func(w *world) { w.setOp(dcA, dcctlv1.Operation_RUNNING) }},
		{name: "voters_unreachable", change: func(w *world) { w.kvs[dcA].setDown(true); w.arb.setDown(true) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.nomad.onStop = func(job string) {
					if job == "opensips" {
						tt.change(w)
					}
				}

				stop := w.start()
				defer stop()

				w.publish(nil)
				settle()

				assert.Equal(t, []string{"opensips"}, w.nomad.stopCalls())
				assert.False(t, w.nomad.stopped("engine"))
				assert.False(t, w.nomad.stopped("freeswitch"))

				entries := w.entries()
				require.Len(t, entries, 1)
				assert.Equal(t, dcctlv1.JournalEntry_WARNING, entries[0].GetResult())
				assert.NotEmpty(t, entries[0].GetError().GetDetail())

				points := w.stops()
				require.Len(t, points, 1)
				assertErrorType(t, "_OTHER", points[0].Attributes)
			})
		})
	}
}

// The snapshot still names dc_a active, but the voters already name this datacenter:
// the guard refuses before anything is stopped, which is only logged.
func TestRunGuardRefusesStaleSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		stop := w.start()
		defer stop()

		w.publish(func(*collect.Snapshot) {
			for _, voter := range []string{dcA, dcB, coord.ArbiterName} {
				w.setState(voter, kv.State{Generation: 6, ActiveDatacenter: dcB})
			}
		})
		settle()

		assert.Empty(t, w.nomad.stopCalls())
		assert.Empty(t, w.entries())
		assert.Empty(t, w.stops())
	})
}

// A job whose node stops answering after its stop does not hold the lower stages.
func TestRunGoesOnPastLostNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.nomad.lost["opensips"] = true

		stop := w.start()
		defer stop()

		w.publish(nil)
		settle()

		assert.Equal(t, allJobs, w.nomad.stopCalls())

		entries := w.entries()
		require.Len(t, entries, 1)
		assert.Equal(t, dcctlv1.JournalEntry_WARNING, entries[0].GetResult())
		assert.Nil(t, entries[0].GetError())

		jobs, ok := entries[0].GetParams().AsMap()["jobs"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, jobs)
		assert.Equal(t, map[string]any{
			"job": "opensips", "stage": float64(3), "state": "stopped",
			"warnings": []any{"1 allocations unknown, their nodes do not answer"},
		}, jobs[0])

		points := w.stops()
		require.Len(t, points, 1)
		assert.Equal(t, attribute.NewSet(attribute.String("webitel.datacenter.name", dcB)), points[0].Attributes)

		// The next snapshot still shows the unknown allocation: no new stop.
		w.publish(nil)
		settle()

		assert.Len(t, w.entries(), 1)
	})
}

func TestRunStopsJobStartedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		stop := w.start()
		defer stop()

		w.publish(nil)
		settle()

		w.nomad.restart("engine")
		w.publish(nil)
		settle()

		assert.Equal(t, []string{"opensips", "engine", "freeswitch", "engine"}, w.nomad.stopCalls())
		assert.True(t, w.nomad.stopped("engine"))
		assert.Len(t, w.entries(), 2)

		points := w.stops()
		require.Len(t, points, 1)
		assert.Equal(t, int64(2), points[0].Value)
	})
}

// A run never overlaps another, and a snapshot collected while it ran, which may show
// the jobs it stopped still running, does not start one.
func TestRunNeverOverlaps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.nomad.block = make(chan struct{})

		stop := w.start()
		defer stop()

		w.publish(nil)
		synctest.Wait()

		w.publish(nil)
		w.publish(nil)
		synctest.Wait()

		assert.Equal(t, []string{"opensips"}, w.nomad.stopCalls())

		close(w.nomad.block)
		settle()

		assert.Equal(t, allJobs, w.nomad.stopCalls())
		assert.Equal(t, 1, w.nomad.maxInFlight)
		assert.Len(t, w.entries(), 1)
	})
}

func TestRunTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.nomad.stuck["engine"] = true

		stop := w.start()
		defer stop()

		w.publish(nil)
		settle()

		assert.False(t, w.nomad.stopped("freeswitch"))

		entries := w.entries()
		require.Len(t, entries, 1)
		assert.Equal(t, dcctlv1.JournalEntry_ERROR, entries[0].GetResult())
		assert.Contains(t, entries[0].GetError().GetDetail(), "engine")

		points := w.stops()
		require.Len(t, points, 1)
		assertErrorType(t, "timeout", points[0].Attributes)
	})
}

func TestRunStopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.nomad.stuck["engine"] = true

		stop := w.start()

		w.publish(nil)
		time.Sleep(10 * time.Second)
		stop()

		assert.False(t, w.nomad.stopped("freeswitch"))

		entries := w.entries()
		require.Len(t, entries, 1)
		assert.Equal(t, dcctlv1.JournalEntry_WARNING, entries[0].GetResult())
		assert.Contains(t, entries[0].GetDetail(), "interrupted")

		points := w.stops()
		require.Len(t, points, 1)
		assertErrorType(t, "_OTHER", points[0].Attributes)
	})
}

func TestNewRejectsMissingDependency(t *testing.T) {
	_, err := fencing.New(fencing.Config{}, fencing.Deps{}, nil)
	require.Error(t, err)
}

func assertErrorType(t *testing.T, want string, set attribute.Set) {
	t.Helper()

	got, ok := set.Value("error.type")
	require.True(t, ok, "no error.type in %v", set.ToSlice())
	assert.Equal(t, want, got.AsString())

	dc, ok := set.Value("webitel.datacenter.name")
	require.True(t, ok)
	assert.Equal(t, dcB, dc.AsString())
}
