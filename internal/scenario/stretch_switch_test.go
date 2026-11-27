package scenario_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// stretchForward are the calls of a switch of stretch from dc_a to dc_b that succeeds.
var stretchForward = []string{
	"stop dc_a from 3", "stop dc_a from 2", "stop dc_a from 0",
	"switchover dc_b to b1",
	"start dc_b to 1", "start dc_b to 2", "start dc_b to all",
	"rejoin dc_a",
}

var stretchStops = []string{"stop dc_a from 3", "stop dc_a from 2", "stop dc_a from 0"}

// startA are the calls of the rollback once every stage of dc_a stopped.
var startA = []string{"start dc_a to 1", "start dc_a to 2", "start dc_a to all"}

// stretchSwitched is the record a switch of stretch to dc_b writes over generation 5,
// with the timeline of the new leader.
func stretchSwitched(op *dcctlv1.Operation, timeline int64) kv.State {
	return kv.State{Generation: 6, Revision: 1, ActiveDatacenter: dcB, OpID: operationID(op), PatroniTimeline: timeline}
}

func TestStretchSwitch_succeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)

		op := w.run()

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, st := range op.GetSteps() {
			assert.Equal(t, succeeded, st.GetState(), "%s: %s", st.GetType(), st.GetDetail())
			assert.Empty(t, st.GetWarnings(), st.GetType())
		}

		assert.Equal(t, stretchForward, w.infra.allCalls())
		assert.True(t, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_SWITCHOVER, 0).GetPointOfNoReturn())

		assert.True(t, noneRunning(w.infra.dc(dcA)))
		assert.True(t, allRunning(w.infra.dc(dcB)))
		assert.True(t, noneRunning(w.infra.dc(dcC)))
		assert.Equal(t, "b1", w.infra.pgLeader())
		assert.Equal(t, int64(6), w.infra.pgMember("b1").timeline)

		a1 := w.infra.pgMember("a1")
		assert.Equal(t, patroni.RoleReplica, a1.role, "the old leader rejoins as a replica")
		assert.True(t, a1.streaming)
		assert.Equal(t, int64(6), a1.timeline)

		assert.Equal(t, stretchSwitched(op, 6), withoutTime(w.voters[0].state(t)))

		l := w.voters[0].lease(t)
		require.NotNil(t, l)
		assert.Equal(t, dcB, l.Holder)
		assert.True(t, w.lockFree())
	})
}

// Patroni moved the leader to dc_b without dcctl: the switch moves the apps only, and
// the step of Patroni is skipped.
func TestStretchSwitch_leaderInTargetSkipsSwitchover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.infra.pg(func(c *stretchCluster) {
			c.lead("b2")
			c.member("a1").streaming = true
			c.member("a1").timeline = 6
		})

		op := w.run()

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		st := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_SWITCHOVER, 0)
		assert.Equal(t, skipped, st.GetState())
		assert.Contains(t, st.GetDetail(), "b2 already runs in dc_b")

		assert.Equal(t, slices.DeleteFunc(slices.Clone(stretchForward), func(c string) bool { return c == "switchover dc_b to b1" }),
			w.infra.allCalls())
		assert.Equal(t, "b2", w.infra.pgLeader())
		assert.Equal(t, stretchSwitched(op, 6), withoutTime(w.voters[0].state(t)), "the timeline of the leader is recorded")
	})
}

func TestStretchSwitch_failureBeforeGenerationRollsBack(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
		step  dcctlv1.Operation_Step_Type
		stage int32
		calls []string
	}{
		{
			name:  "stop_first_stage",
			setup: func(w *world) { w.infra.fail("stop dc_a from 3", apps.ErrTimeout) },
			step:  dcctlv1.Operation_Step_APPS_STOP, stage: 3,
			calls: []string{"stop dc_a from 3", "start dc_a to all"},
		},
		{
			name:  "stop_last_stage",
			setup: func(w *world) { w.infra.fail("stop dc_a from 0", apps.ErrTimeout) },
			step:  dcctlv1.Operation_Step_APPS_STOP, stage: 1,
			calls: slices.Concat(stretchStops, startA),
		},
		{
			// Another writer changes the base between the read of the step and the write:
			// nothing is written.
			name: "generation_base_changed",
			setup: func(w *world) {
				w.infra.hook("stop dc_a from 0", func(context.Context) error {
					reads := 0

					w.voters[0].set(func(v *fakeVoter) {
						v.onGet = func(v *fakeVoter) {
							if reads++; reads == 2 {
								b, err := kv.EncodeState(kv.State{Generation: 5, Revision: 1, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 9}, nil)
								require.NoError(w.t, err)
								v.store(coord.KeyState, b)
							}
						}
					})

					return nil
				})
			},
			step:  dcctlv1.Operation_Step_GENERATION,
			calls: slices.Concat(stretchStops, startA),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newStretchWorld(t)
				tt.setup(w)

				op := w.run()

				assert.Equal(t, dcctlv1.Operation_FAILED, op.GetState(), op.GetError().GetDetail())
				assert.Equal(t, tt.calls, w.infra.allCalls())
				assert.Equal(t, failed, stepOf(t, op, tt.step, tt.stage).GetState())
				assert.True(t, kv.StepRolledBack(stepOf(t, op, dcctlv1.Operation_Step_APPS_STOP, 3)), "the apps of dc_a start again")

				assert.True(t, allRunning(w.infra.dc(dcA)))
				assert.True(t, noneRunning(w.infra.dc(dcB)))
				assert.Equal(t, "a1", w.infra.pgLeader())
				assert.Equal(t, int64(5), w.voters[0].state(t).Generation)
				assert.Equal(t, dcA, w.voters[0].state(t).ActiveDatacenter)
				assert.True(t, w.lockFree())
			})
		})
	}
}

// A failure once the generation may be written, or is, interrupts the switch: the
// generation names dc_b, so the apps of dc_a do not start again.
func TestStretchSwitch_interrupts(t *testing.T) {
	refused := fmt.Errorf("%w: 412 switchover is not possible", pgctl.ErrRefused)

	tests := []struct {
		name  string
		setup func(w *world)
		step  dcctlv1.Operation_Step_Type
		stage int32
		calls []string
		// detail is in the error of the operation.
		detail string
		// generation is the one the voter holds after the switch.
		generation int64
	}{
		{
			// The voter does not take the record: it is not committed.
			name: "generation_uncommitted",
			setup: func(w *world) {
				w.infra.hook("stop dc_a from 0", func(context.Context) error {
					w.voters[0].set(func(v *fakeVoter) { v.casErr = errors.New("cas refused") })

					return nil
				})
			},
			step: dcctlv1.Operation_Step_GENERATION, calls: stretchStops, detail: "not committed", generation: 5,
		},
		{
			// T2 §5.5 step 4: Patroni refused, the leader stays in dc_a.
			name:  "switchover_refused",
			setup: func(w *world) { w.infra.fail("switchover dc_b to b1", refused) },
			step:  dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: slices.Concat(stretchStops, []string{"switchover dc_b to b1"}),
			detail: `ended with the leader "a1"`, generation: 6,
		},
		{
			// T2 §5.5 step 4: the leader is in another datacenter.
			name:  "leader_elsewhere",
			setup: func(w *world) { w.infra.pg(func(c *stretchCluster) { c.lead("c1") }) },
			step:  dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: stretchStops,
			detail: "the leader c1 runs in dc_c", generation: 6,
		},
		{
			name: "leader_lost",
			setup: func(w *world) {
				w.infra.hook("stop dc_a from 0", func(context.Context) error {
					w.infra.pg(func(c *stretchCluster) { c.leader = "" })

					return nil
				})
			},
			step: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: stretchStops, detail: "no leader", generation: 6,
		},
		{
			name: "another_primary",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) { c.member("c1").role = patroni.RolePrimary })
			},
			step: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: stretchStops, detail: "member c1 of dc_c", generation: 6,
		},
		{
			name: "no_candidate",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) {
					c.member("b1").streaming = false
					c.member("b2").lag = nil
				})
			},
			step: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: stretchStops, detail: "no member of dc_b streams", generation: 6,
		},
		{
			name:  "config_not_read",
			setup: func(w *world) { w.infra.pg(func(c *stretchCluster) { c.maxLag = nil }) },
			step:  dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: stretchStops,
			detail: "maximum_lag_on_failover is not known", generation: 6,
		},
		{
			// The leader moves to c1 between the read of the step and the request: the
			// switchover is pinned to a1 and refused.
			name: "leader_moves_before_the_request",
			setup: func(w *world) {
				w.infra.hook("switchover dc_b to b1", func(context.Context) error {
					w.infra.pg(func(c *stretchCluster) { c.lead("c1") })

					return nil
				})
			},
			step: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, calls: slices.Concat(stretchStops, []string{"switchover dc_b to b1"}),
			detail: "leader is c1, not a1", generation: 6,
		},
		{
			name:  "start_target",
			setup: func(w *world) { w.infra.fail("start dc_b to 2", apps.ErrTimeout) },
			step:  dcctlv1.Operation_Step_APPS_START, stage: 2,
			calls:  slices.Concat(stretchStops, []string{"switchover dc_b to b1", "start dc_b to 1", "start dc_b to 2"}),
			detail: apps.ErrTimeout.Error(), generation: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newStretchWorld(t)
				tt.setup(w)

				op := w.run()

				require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				assert.Contains(t, op.GetError().GetDetail(), tt.detail)
				assert.Equal(t, tt.calls, w.infra.allCalls())
				assert.Equal(t, failed, stepOf(t, op, tt.step, tt.stage).GetState())

				for _, st := range op.GetSteps() {
					assert.False(t, kv.StepRolledBack(st), "%s stage %d", st.GetType(), st.GetStage())
				}

				assert.True(t, noneRunning(w.infra.dc(dcA)), "the apps of dc_a stay stopped")
				assert.Equal(t, tt.generation, w.voters[0].state(t).Generation)
				assert.True(t, w.lockFree())
			})
		})
	}
}

// firstOf returns the first step of op of type typ.
func firstOf(t *testing.T, op *dcctlv1.Operation, typ dcctlv1.Operation_Step_Type) *dcctlv1.Operation_Step {
	t.Helper()

	i := slices.IndexFunc(op.GetSteps(), func(st *dcctlv1.Operation_Step) bool { return st.GetType() == typ })
	require.GreaterOrEqual(t, i, 0, "operation has no step %s", typ)

	return op.GetSteps()[i]
}

func TestStretchSwitch_warnings(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
		step  dcctlv1.Operation_Step_Type
		want  string
	}{
		{
			// T3 §6.1 step 2.
			name:  "old_leader_does_not_rejoin",
			setup: func(w *world) { w.infra.fail("rejoin dc_a", fmt.Errorf("a1 not streaming: %w", pgctl.ErrTimeout)) },
			step:  dcctlv1.Operation_Step_PATRONI_FOLLOW, want: "dc_a needs attention",
		},
		{
			name:  "member_not_read",
			setup: func(w *world) { w.infra.pg(func(c *stretchCluster) { c.member("a2").unread = true }) },
			step:  dcctlv1.Operation_Step_PATRONI_FOLLOW, want: "member a2 of dc_a is not read",
		},
		{
			name: "instance_missing_from_cluster",
			setup: func(w *world) {
				w.infra.pg(func(c *stretchCluster) { c.missing = map[string][]string{dcA: {"10.0.1.9"}} })
			},
			step: dcctlv1.Operation_Step_PATRONI_FOLLOW, want: "instance on host 10.0.1.9 of dc_a is not in the Patroni cluster",
		},
		{
			// T2 §5.5 step 6.
			name: "verify",
			setup: func(w *world) {
				w.verifier.checks = []check.Check{{ID: check.VerifyJobs, Result: check.Fail, Detail: "job x is not healthy in dc_b"}}
			},
			step: dcctlv1.Operation_Step_VERIFY, want: "job x is not healthy in dc_b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newStretchWorld(t)
				tt.setup(w)

				op := w.run()

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

				st := firstOf(t, op, tt.step)
				assert.Equal(t, succeeded, st.GetState())
				require.Len(t, st.GetWarnings(), 1)
				assert.Contains(t, st.GetWarnings()[0], tt.want)
			})
		})
	}
}

// The instance crashes in each phase of the switch; after the restart the operation is
// interrupted, and the resume runs the step that was running again and finishes.
func TestStretchSwitch_crashResumes(t *testing.T) {
	for _, key := range []string{"stop dc_a from 2", "switchover dc_b to b1", "start dc_b to 2", "rejoin dc_a"} {
		t.Run(key, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newStretchWorld(t)

				op := w.crashAndResume(key)

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

				i := slices.Index(stretchForward, key)
				assert.Equal(t, slices.Concat(stretchForward[:i+1], stretchForward[i:]), w.infra.allCalls())

				assert.True(t, noneRunning(w.infra.dc(dcA)))
				assert.True(t, allRunning(w.infra.dc(dcB)))
				assert.Equal(t, "b1", w.infra.pgLeader())
				assert.Equal(t, stretchSwitched(op, 6), withoutTime(w.voters[0].state(t)), "the generation and its timeline are written once")
			})
		})
	}
}

// The switchover took effect before the crash: the resumed step finds the leader in dc_b
// and succeeds without another request, as the point of no return is passed.
func TestStretchSwitch_resumeAfterSwitchoverTookEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)

		var saved map[string][]byte

		w.infra.hook("switchover dc_b to b1", func(ctx context.Context) error {
			saved = w.rawOps()
			w.infra.pg(func(c *stretchCluster) { c.lead("b1") })
			<-ctx.Done()

			return context.Cause(ctx)
		})

		e := w.engine()
		stop := start(t, e)

		synctest.Wait()

		first, err := e.Start(t.Context(), w.plan())
		require.NoError(t, err)

		synctest.Wait()
		stop()

		require.NotNil(t, saved)
		w.setRawOps(saved)

		e = w.engine()
		stop = start(t, e)

		defer stop()

		synctest.Wait()

		op := w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		st := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_SWITCHOVER, 0)
		assert.Equal(t, succeeded, st.GetState(), "a resumed step that finds the leader in dc_b is not skipped")
		assert.Equal(t, 1, countCalls(w.infra.allCalls(), "switchover dc_b to b1"))
		assert.Equal(t, stretchSwitched(op, 6), withoutTime(w.voters[0].state(t)))
	})
}

func countCalls(calls []string, call string) int {
	n := 0

	for _, c := range calls {
		if c == call {
			n++
		}
	}

	return n
}

// The record of the switch is left undetermined on the voter; the resume finishes it.
func TestStretchSwitch_resumeUndeterminedGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		w.infra.hook("stop dc_a from 0", func(context.Context) error {
			w.voters[0].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })

			return nil
		})

		synctest.Wait()

		first, err := e.Start(t.Context(), w.plan())
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState(), op.GetError().GetDetail())
		require.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0).GetState())

		w.voters[0].set(func(v *fakeVoter) { v.casErr = nil })

		op = w.resume(e, operationID(first), operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, stretchForward, w.infra.allCalls())
		assert.Equal(t, stretchSwitched(op, 6), withoutTime(w.voters[0].state(t)))
	})
}
