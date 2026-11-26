package scenario_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

const (
	succeeded = dcctlv1.Operation_Step_SUCCEEDED
	failed    = dcctlv1.Operation_Step_FAILED
	pending   = dcctlv1.Operation_Step_PENDING
)

// forward are the calls of a switch from dc_a to dc_b that succeeds.
var forward = []string{
	"stop dc_a from 3", "stop dc_a from 2", "stop dc_a from 0",
	"demote dc_a", "catchup dc_b", noPrimary, "no_primary dc_a", "promote dc_b",
	"start dc_b to 1", "start dc_b to 2", "start dc_b to all",
	"repoint dc_c", "follow dc_a",
}

// restoreA are the calls of the rollback once dc_a was demoted.
var restoreA = []string{"no_primary dc_b,dc_c", "promote dc_a", "start dc_a to 1", "start dc_a to 2", "start dc_a to all"}

// noPrimary is the call of the step that checks that no cluster is primary.
const noPrimary = "no_primary dc_a,dc_b"

// upTo returns forward up to and including call.
func upTo(t *testing.T, call string) []string {
	t.Helper()

	i := slices.Index(forward, call)
	require.GreaterOrEqual(t, i, 0, call)

	return slices.Clone(forward[:i+1])
}

// run runs the switch to dc_b on a running engine and returns it as it ended.
func (w *world) run() *dcctlv1.Operation {
	w.t.Helper()

	e := w.engine()
	stop := start(w.t, e)

	defer stop()

	synctest.Wait()

	first, err := e.Start(w.t.Context(), w.plan())
	require.NoError(w.t, err)
	assert.Equal(w.t, dcctlv1.Operation_RUNNING, first.GetState())

	synctest.Wait()

	return w.op()
}

func (w *world) setVoters(s kv.State) {
	for _, v := range w.voters {
		v.setState(w.t, s)
	}
}

func (w *world) voterStates() []kv.State {
	out := make([]kv.State, 0, len(w.voters))
	for _, v := range w.voters {
		out = append(out, v.state(w.t))
	}

	return out
}

// assertActive checks that every voter names dc active at generation.
func (w *world) assertActive(generation int64, dc string) {
	w.t.Helper()

	for _, s := range w.voterStates() {
		assert.Equal(w.t, generation, s.Generation)
		assert.Equal(w.t, dc, s.ActiveDatacenter)
	}
}

func TestSwitch_succeeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		op := w.run()

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		for _, st := range op.GetSteps() {
			assert.Equal(t, succeeded, st.GetState(), "%s: %s", st.GetType(), st.GetDetail())
			assert.Empty(t, st.GetWarnings(), st.GetType())
		}

		assert.Equal(t, forward, w.infra.allCalls())

		a, b, c := w.infra.dc(dcA), w.infra.dc(dcB), w.infra.dc(dcC)
		assert.True(t, noneRunning(a))
		assert.True(t, allRunning(b))
		assert.Equal(t, patroni.RolePrimary, b.role)
		assert.Equal(t, int64(6), b.timeline)
		assert.Equal(t, patroni.RoleStandbyLeader, a.role)
		assert.Equal(t, dcB, a.upstream)
		assert.Equal(t, dcB, c.upstream)
		assert.Equal(t, int64(6), c.timeline)

		id := operationID(op)
		for _, s := range w.voterStates() {
			assert.Equal(t, kv.State{Generation: 6, Revision: 1, ActiveDatacenter: dcB, OpID: id, PatroniTimeline: 6},
				withoutTime(s))
		}

		for _, v := range w.voters[:2] {
			l := v.lease(t)
			require.NotNil(t, l, v.name)
			assert.Equal(t, dcB, l.Holder)
			assert.Equal(t, int64(6), l.Generation)
		}

		assert.Equal(t, 1, w.verifier.calls)
		assert.True(t, w.lockFree())
	})
}

func TestSwitch_failureBeforePointOfNoReturnRollsBack(t *testing.T) {
	stops := []string{"stop dc_a from 3", "stop dc_a from 2", "stop dc_a from 0"}

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
			name:  "stop_second_stage",
			setup: func(w *world) { w.infra.fail("stop dc_a from 2", apps.ErrTimeout) },
			step:  dcctlv1.Operation_Step_APPS_STOP, stage: 2,
			calls: []string{"stop dc_a from 3", "stop dc_a from 2", "start dc_a to 2", "start dc_a to all"},
		},
		{
			name:  "stop_last_stage",
			setup: func(w *world) { w.infra.fail("stop dc_a from 0", apps.ErrTimeout) },
			step:  dcctlv1.Operation_Step_APPS_STOP, stage: 1,
			calls: slices.Concat(stops, []string{"start dc_a to 1", "start dc_a to 2", "start dc_a to all"}),
		},
		{
			name:  "demote_still_primary",
			setup: func(w *world) { w.infra.fail("demote dc_a", pgctl.ErrTimeout) },
			step:  dcctlv1.Operation_Step_PATRONI_DEMOTE,
			calls: slices.Concat(stops, []string{"demote dc_a"}, restoreA),
		},
		{
			name: "demote_still_primary_outcome_unknown",
			setup: func(w *world) {
				w.infra.fail("demote dc_a", fmt.Errorf("%w: %w", pgctl.ErrOutcomeUnknown, pgctl.ErrTimeout))
			},
			step:  dcctlv1.Operation_Step_PATRONI_DEMOTE,
			calls: slices.Concat(stops, []string{"demote dc_a"}, restoreA),
		},
		{
			name:  "catchup",
			setup: func(w *world) { w.infra.fail("catchup dc_b", pgctl.ErrTimeout) },
			step:  dcctlv1.Operation_Step_PATRONI_CATCHUP,
			calls: slices.Concat(upTo(t, "catchup dc_b"), restoreA),
		},
		{
			name:  "no_primary_not_read",
			setup: func(w *world) { w.infra.fail(noPrimary, fmt.Errorf("dc_b not read: %w", pgctl.ErrTimeout)) },
			step:  dcctlv1.Operation_Step_PATRONI_NO_PRIMARY,
			calls: slices.Concat(upTo(t, noPrimary), restoreA),
		},
		{
			// dc_a came back as primary: nothing else is, so it stays primary.
			name: "no_primary_finds_a",
			setup: func(w *world) {
				w.infra.hook(noPrimary, func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RolePrimary })

					return nil
				})
			},
			step:  dcctlv1.Operation_Step_PATRONI_NO_PRIMARY,
			calls: slices.Concat(upTo(t, noPrimary), restoreA),
		},
		{
			// Two payloads of revision 1 without a majority: the base is unsettled and
			// nothing is written.
			name: "generation_base_unsettled",
			setup: func(w *world) {
				w.infra.hook(noPrimary, func(context.Context) error {
					w.voters[0].setState(w.t, kv.State{Generation: 5, Revision: 1, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 7})
					w.voters[2].setState(w.t, kv.State{Generation: 5, Revision: 1, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 8})

					return nil
				})
			},
			step:  dcctlv1.Operation_Step_GENERATION,
			calls: slices.Concat(upTo(t, noPrimary), restoreA),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)

				op := w.run()

				assert.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
				assert.Equal(t, tt.calls, w.infra.allCalls())
				assert.Equal(t, failed, stepOf(t, op, tt.step, tt.stage).GetState())
				assert.True(t, kv.StepRolledBack(stepOf(t, op, dcctlv1.Operation_Step_APPS_STOP, 3)), "the apps of dc_a start again")

				a, b := w.infra.dc(dcA), w.infra.dc(dcB)
				assert.True(t, allRunning(a))
				assert.Equal(t, patroni.RolePrimary, a.role)
				assert.True(t, noneRunning(b))
				assert.Equal(t, dcA, b.upstream)
				w.assertActive(5, dcA)
				assert.True(t, w.lockFree())
			})
		})
	}
}

func TestSwitch_interrupts(t *testing.T) {
	foreign := kv.State{Generation: 6, ActiveDatacenter: dcC, OpID: "other"}

	tests := []struct {
		name  string
		setup func(w *world)
		step  dcctlv1.Operation_Step_Type
		stage int32
		calls []string
		// detail is in the error of the operation.
		detail string
		// generation and active are what the voters hold after.
		generation int64
		active     string
	}{
		{
			name: "promote_fails", detail: pgctl.ErrTimeout.Error(),
			setup:      func(w *world) { w.infra.fail("promote dc_b", pgctl.ErrTimeout) },
			step:       dcctlv1.Operation_Step_PATRONI_PROMOTE,
			calls:      upTo(t, "promote dc_b"),
			generation: 6, active: dcB,
		},
		{
			name: "apps_start_fails", detail: apps.ErrTimeout.Error(),
			setup: func(w *world) { w.infra.fail("start dc_b to 2", apps.ErrTimeout) },
			step:  dcctlv1.Operation_Step_APPS_START, stage: 2,
			calls:      upTo(t, "start dc_b to 2"),
			generation: 6, active: dcB,
		},
		{
			name: "demote_outcome_unknown", detail: "the operator decides",
			setup: func(w *world) {
				w.infra.hook("demote dc_a", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = "" })

					return fmt.Errorf("%w: %w", pgctl.ErrOutcomeUnknown, pgctl.ErrTimeout)
				})
			},
			step:       dcctlv1.Operation_Step_PATRONI_DEMOTE,
			calls:      upTo(t, "demote dc_a"),
			generation: 5, active: dcA,
		},
		{
			name: "demote_leaves_leader_down", detail: `leader of dc_a is "uninitialized"`,
			setup: func(w *world) {
				w.infra.hook("demote dc_a", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RoleUninitialized })

					return pgctl.ErrTimeout
				})
			},
			step:       dcctlv1.Operation_Step_PATRONI_DEMOTE,
			calls:      upTo(t, "demote dc_a"),
			generation: 5, active: dcA,
		},
		{
			// The guard of the next stage sees another active datacenter.
			name: "generation_changed_under_stop", detail: "the step acts under generation 5 of dc_a",
			setup: func(w *world) {
				w.infra.hook("stop dc_a from 3", func(context.Context) error {
					w.setVoters(foreign)

					return nil
				})
			},
			step: dcctlv1.Operation_Step_APPS_STOP, stage: 2,
			calls:      []string{"stop dc_a from 3"},
			generation: 6, active: dcC,
		},
		{
			// dc_b took over by itself: promoting dc_a again would make two primaries.
			name: "catchup_finds_target_primary", detail: "leader dc_b1 of dc_b is primary",
			setup: func(w *world) {
				w.infra.hook("catchup dc_b", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcB].role = patroni.RolePrimary })

					return nil
				})
			},
			step:       dcctlv1.Operation_Step_PATRONI_CATCHUP,
			calls:      upTo(t, "catchup dc_b"),
			generation: 5, active: dcA,
		},
		{
			name: "no_primary_finds_standby_primary", detail: "leader dc_c1 of dc_c is primary",
			setup: func(w *world) {
				w.infra.hook(noPrimary, func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].role = patroni.RolePrimary })

					return nil
				})
			},
			step:       dcctlv1.Operation_Step_PATRONI_NO_PRIMARY,
			calls:      upTo(t, noPrimary),
			generation: 5, active: dcA,
		},
		{
			name: "promote_finds_source_primary", detail: "leader dc_a1 of dc_a is primary",
			setup: func(w *world) {
				w.infra.hook("no_primary dc_a", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RolePrimary })

					return nil
				})
			},
			step:       dcctlv1.Operation_Step_PATRONI_PROMOTE,
			calls:      upTo(t, "no_primary dc_a"),
			generation: 6, active: dcB,
		},
		{
			// The guard of the promotion sees another active datacenter.
			name: "promote_guard_refuses", detail: "the step acts under generation 6 of dc_b",
			setup: func(w *world) {
				w.infra.hook("no_primary dc_a", func(context.Context) error {
					w.setVoters(foreign)

					return nil
				})
			},
			step:       dcctlv1.Operation_Step_PATRONI_PROMOTE,
			calls:      upTo(t, "no_primary dc_a"),
			generation: 6, active: dcC,
		},
		{
			// Another writer advanced the generation before this one: dc_a must not start.
			name: "generation_superseded", detail: "the voters hold generation 6 of dc_c",
			setup: func(w *world) {
				w.infra.hook(noPrimary, func(context.Context) error {
					w.setVoters(foreign)

					return nil
				})
			},
			step:       dcctlv1.Operation_Step_GENERATION,
			calls:      upTo(t, noPrimary),
			generation: 6, active: dcC,
		},
		{
			// Another writer gets to the KVs between the read and the CAS: the record of
			// this switch stays on the arbiter only.
			name: "generation_cas_race", detail: coord.ErrUncommitted.Error(),
			setup: func(w *world) {
				b, err := kv.EncodeState(foreign, nil)
				require.NoError(w.t, err)

				for _, v := range w.voters[:2] {
					v.set(func(v *fakeVoter) {
						v.beforeCAS = func(v *fakeVoter, value []byte) {
							var s kv.State
							if json.Unmarshal(value, &s) == nil && s.Generation == 6 && s.ActiveDatacenter == dcB {
								v.store(coord.KeyState, b)
								v.beforeCAS = nil
							}
						}
					})
				}
			},
			step:       dcctlv1.Operation_Step_GENERATION,
			calls:      upTo(t, noPrimary),
			generation: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)

				op := w.run()

				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				assert.Contains(t, op.GetError().GetDetail(), tt.detail)
				assert.Equal(t, tt.calls, w.infra.allCalls(), "nothing is rolled back")
				assert.Equal(t, failed, stepOf(t, op, tt.step, tt.stage).GetState())
				assert.False(t, kv.StepRolledBack(stepOf(t, op, tt.step, tt.stage)))

				for _, s := range w.voterStates()[:2] {
					assert.Equal(t, tt.generation, s.Generation)

					if tt.active != "" {
						assert.Equal(t, tt.active, s.ActiveDatacenter)
					}
				}

				assert.True(t, w.lockFree())
			})
		})
	}
}

// The catchup fails and dc_c is primary meanwhile: the rollback does not promote dc_a.
func TestSwitch_rollbackDoesNotPromoteNextToPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.hook("catchup dc_b", func(context.Context) error {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].role = patroni.RolePrimary })

			return pgctl.ErrTimeout
		})

		op := w.run()

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "leader dc_c1 of dc_c is primary")
		assert.Equal(t, slices.Concat(upTo(t, "catchup dc_b"), []string{"no_primary dc_b,dc_c"}), w.infra.allCalls())
		assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcA).role)
		assert.True(t, noneRunning(w.infra.dc(dcA)))
		assert.True(t, w.lockFree())
	})
}

// dc_b is not read when the switch fails before the generation: the rollback promotes
// dc_a with a warning, as the switch did not promote dc_b.
func TestSwitch_rollbackWithTargetNotRead(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
		step  dcctlv1.Operation_Step_Type
		call  string
	}{
		{
			name: "catchup_target_lost",
			setup: func(w *world) {
				w.infra.hook("catchup dc_b", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcB].unread = true })

					return pgctl.ErrTimeout
				})
			},
			step: dcctlv1.Operation_Step_PATRONI_CATCHUP,
			call: "catchup dc_b",
		},
		{
			name: "no_primary_target_lost",
			setup: func(w *world) {
				w.infra.hook(noPrimary, func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcB].unread = true })

					return nil
				})
			},
			step: dcctlv1.Operation_Step_PATRONI_NO_PRIMARY,
			call: noPrimary,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)

				op := w.run()

				assert.Equal(t, dcctlv1.Operation_FAILED, op.GetState(), op.GetError().GetDetail())
				assert.Equal(t, slices.Concat(upTo(t, tt.call), restoreA), w.infra.allCalls())
				assert.Equal(t, failed, stepOf(t, op, tt.step, 0).GetState())
				assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0).GetWarnings(),
					"dc_b is not read, it is not known whether it is primary")

				a := w.infra.dc(dcA)
				assert.Equal(t, patroni.RolePrimary, a.role)
				assert.True(t, allRunning(a))
				w.assertActive(5, dcA)
				assert.True(t, w.lockFree())
			})
		})
	}
}

// A standby that is not read neither blocks the switch nor is taken as no primary.
func TestSwitch_unreadStandbyIsAWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].unread = true })

		op := w.run()

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, forward, w.infra.allCalls())

		want := []string{"dc_c is not read, it is not known whether it is primary"}
		assert.Equal(t, want, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_NO_PRIMARY, 0).GetWarnings())
		assert.Equal(t, want, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings())
		w.assertActive(6, dcB)
	})
}

func TestSwitch_lockLostInterrupts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.hook("catchup dc_b", func(ctx context.Context) error {
			<-ctx.Done()

			return context.Cause(ctx)
		})

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Start(t.Context(), w.plan())
		require.NoError(t, err)

		synctest.Wait()

		for _, c := range w.consuls {
			c.expireAll()
		}

		time.Sleep(10 * time.Second)
		synctest.Wait()

		op := w.op()
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), coord.ErrLockLost.Error())
		assert.Equal(t, upTo(t, "catchup dc_b"), w.infra.allCalls(), "nothing is rolled back")
		assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_CATCHUP, 0).GetState())
		w.assertActive(5, dcA)
	})
}

func TestSwitch_warnings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.fail("repoint dc_c", pgctl.ErrTimeout)
		w.infra.hook("follow dc_a", func(context.Context) error { return pgctl.ErrTimeout })
		w.verifier.checks = []check.Check{
			{ID: check.VerifyJobs, Result: check.Fail, Datacenter: dcC, Detail: "1 managed jobs of dc_c are not stopped"},
			{ID: check.VerifyState, Result: check.Pass},
		}

		op := w.run()

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState())
		assert.Equal(t, forward, w.infra.allCalls())

		repoint := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0)
		assert.Equal(t, succeeded, repoint.GetState())
		require.Len(t, repoint.GetWarnings(), 1)
		assert.Contains(t, repoint.GetWarnings()[0], "dc_c is not repointed to dc_b")

		follow := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FOLLOW, 0)
		require.Len(t, follow.GetWarnings(), 1)
		assert.Contains(t, follow.GetWarnings()[0], "dc_a needs attention")

		verify := stepOf(t, op, dcctlv1.Operation_Step_VERIFY, 0)
		assert.Equal(t, []string{"verify.jobs: 1 managed jobs of dc_c are not stopped"}, verify.GetWarnings())
	})
}

func operationID(op *dcctlv1.Operation) string {
	return op.GetName()[len("operations/"):]
}

func withoutTime(s kv.State) kv.State {
	s.UpdatedAt = time.Time{}

	return s
}
