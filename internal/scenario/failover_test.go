package scenario_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

const skipped = dcctlv1.Operation_Step_SKIPPED

// failoverForward are the calls of a failover from dc_a, which answers, to dc_b.
var failoverForward = []string{
	"failover stop dc_a from 3", "failover stop dc_a from 2", "failover stop dc_a from 0",
	"demote dc_a", "no_primary dc_a,dc_c", "promote dc_b",
	"start dc_b to 1", "start dc_b to 2", "start dc_b to all",
	"repoint dc_c",
}

// failoverLost are the calls of a failover from dc_a, which does not answer, to dc_b.
var failoverLost = []string{
	"failover stop dc_a from 3", "no_primary dc_a,dc_c", "promote dc_b",
	"start dc_b to 1", "start dc_b to 2", "start dc_b to all",
	"repoint dc_c",
}

// unconfirmedStop is the warning of a stop of the apps of dc_a that did not end.
const unconfirmedStop = "the stop of the apps of the old active datacenter is not confirmed"

// isolation confirms the isolation of the old active datacenter, as every failover does.
var isolation = &dcctlv1.Confirmation{Isolation: true}

func failoverTo(dc string, c *dcctlv1.Confirmation) *dcctlv1.FailoverDatacenterRequest {
	return &dcctlv1.FailoverDatacenterRequest{Name: "datacenters/" + dc, Confirmation: c, RequestId: "0b7d2c1e-3f4a-4c5b-8d6e-7f8091a2b3c4"}
}

func (w *world) failoverPlan(c *dcctlv1.Confirmation) operation.Plan {
	w.t.Helper()

	req := failoverTo(dcB, c)

	v, err := w.preflight().Run(w.t.Context(), preflight.Request{
		Action: preflight.Failover, Params: preflight.Params{Datacenter: dcB},
		Confirmations: check.Confirmations{MaxDiscardBytes: c.GetMaxDiscardBytes()},
	})
	require.NoError(w.t, err)

	plan := w.fo.Plan
	if w.stretch {
		plan = w.sfo.Plan
	}

	p, err := plan(req, initiator, preflight.Result{Verdict: v, Status: w.status()})
	require.NoError(w.t, err)

	return p
}

// runFailover runs the failover to dc_b with c on a running engine and returns it as it
// ended.
func (w *world) runFailover(c *dcctlv1.Confirmation) *dcctlv1.Operation {
	w.t.Helper()

	e := w.engine()
	stop := start(w.t, e)

	defer stop()

	synctest.Wait()

	_, err := e.Start(w.t.Context(), w.failoverPlan(c))
	require.NoError(w.t, err)

	synctest.Wait()

	return w.op()
}

// loseA makes dc_a lost: its KV, its Patroni cluster and its Nomad do not answer.
func (w *world) loseA() {
	w.voters[0].set(func(v *fakeVoter) { v.getErr = unreachable() })
	w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].unread = true })
	w.infra.fail("failover stop dc_a from 3", fmt.Errorf("stop apps of dc_a: %w: %w", apps.ErrNotListed, apps.ErrTimeout))
}

// failedOver is the record a failover to dc_b writes over generation 5, its timeline
// written.
func failedOver(op *dcctlv1.Operation, forced bool) kv.State {
	return kv.State{
		Generation: 6, Revision: 1, ActiveDatacenter: dcB, OpID: operationID(op), Forced: forced, PatroniTimeline: 6,
		NeedsRecovery: []string{dcA},
	}
}

func TestFailover_sourceAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode())

		for _, st := range op.GetSteps() {
			assert.Equal(t, succeeded, st.GetState(), "%s: %s", st.GetType(), st.GetDetail())
			assert.Empty(t, st.GetWarnings(), st.GetType())
		}

		assert.Equal(t, failoverForward, w.infra.allCalls())

		a, b, c := w.infra.dc(dcA), w.infra.dc(dcB), w.infra.dc(dcC)
		assert.True(t, noneRunning(a))
		assert.Equal(t, patroni.RoleStandbyLeader, a.role, "dc_a is demoted")
		assert.Equal(t, dcB, a.upstream)
		assert.True(t, allRunning(b))
		assert.Equal(t, patroni.RolePrimary, b.role)
		assert.Equal(t, int64(6), b.timeline)
		assert.Equal(t, dcB, c.upstream)

		for _, s := range w.voterStates() {
			assert.Equal(t, failedOver(op, false), withoutTime(s))
		}

		for _, v := range w.voters[:2] {
			l := v.lease(t)
			require.NotNil(t, l, v.name)
			assert.Equal(t, dcB, l.Holder)
		}

		assert.Equal(t, 1, w.verifier.calls)
		assert.True(t, w.lockFree())
	})
}

// dc_a is lost: the KV of dc_b and the arbiter are a majority, the stop of dc_a is not
// confirmed and the steps that need dc_a are warnings.
func TestFailover_sourceLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.loseA()

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode())
		assert.Equal(t, failoverLost, w.infra.allCalls())

		stop3 := stepOf(t, op, dcctlv1.Operation_Step_APPS_STOP, 3)
		assert.Equal(t, succeeded, stop3.GetState())
		assert.Equal(t, []string{"the Nomad of the old active datacenter does not answer, its apps are not stopped"}, stop3.GetWarnings())
		assert.Contains(t, stop3.GetDetail(), apps.ErrTimeout.Error())

		for _, stage := range []int32{2, 1} {
			assert.Equal(t, skipped, stepOf(t, op, dcctlv1.Operation_Step_APPS_STOP, stage).GetState(), "stage %d", stage)
		}

		demote := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0)
		assert.Equal(t, skipped, demote.GetState())
		require.Len(t, demote.GetWarnings(), 1)
		assert.Contains(t, demote.GetWarnings()[0], "dc_a is not read, it is not demoted")
		assert.Equal(t, []string{"dc_a is not read, it is not known whether it is primary"},
			stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings())
		assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_VERIFY, 0).GetWarnings(), unconfirmedStop)

		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)

		for _, v := range w.voters[1:] {
			assert.Equal(t, failedOver(op, false), withoutTime(v.state(t)), v.name)
		}
	})
}

// The Nomad of dc_a answers and a job of stage 3 does not stop in time: the lower stages
// are stopped still, each within the failover stop timeout.
func TestFailover_stopGoesOnWhileNomadAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.fail("failover stop dc_a from 3", fmt.Errorf("stage 3: %w", apps.ErrTimeout))

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, failoverForward, w.infra.allCalls(), "every stage of dc_a gets its stop")
		assert.True(t, noneRunning(w.infra.dc(dcA)))
		assert.Equal(t, []string{unconfirmedStop}, stepOf(t, op, dcctlv1.Operation_Step_APPS_STOP, 3).GetWarnings())

		for _, stage := range []int32{2, 1} {
			assert.Equal(t, succeeded, stepOf(t, op, dcctlv1.Operation_Step_APPS_STOP, stage).GetState(), "stage %d", stage)
		}

		assert.Contains(t, stepOf(t, op, dcctlv1.Operation_Step_VERIFY, 0).GetWarnings(), unconfirmedStop)
	})
}

// Two datacenters without an arbiter and dc_a lost: the generation is written forced.
func TestFailover_forced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newTwoVoterWorld(t)
		w.loseA()

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_FORCED, op.GetMode())
		assert.Equal(t, failoverLost, w.infra.allCalls())
		assert.Equal(t, failedOver(op, true), withoutTime(w.voters[1].state(t)))
	})
}

// A partial advance: only the KV of dc_b answers, with generation 6 of dc_a that the
// voters that do not answer may hold. The failover writes generation 7 over it, forced.
func TestFailover_fromUndeterminedRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.loseA()
		w.voters[1].setState(t, kv.State{Generation: 6, ActiveDatacenter: dcA, OpID: "partial", PatroniTimeline: 5})
		w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })

		p := w.failoverPlan(isolation)
		assert.Equal(t, int64(6), p.PreviousGeneration)
		assert.Equal(t, int64(7), p.NewGeneration)
		assert.Equal(t, dcA, p.PreviousActive)
		assert.Equal(t, dcctlv1.Operation_FORCED, p.Mode)

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, failoverLost, w.infra.allCalls())
		assert.Equal(t, kv.State{
			Generation: 7, Revision: 1, ActiveDatacenter: dcB, OpID: operationID(op), Forced: true, PatroniTimeline: 6,
			NeedsRecovery: []string{dcA},
		}, withoutTime(w.voters[1].state(t)))
	})
}

// A standby that received more WAL than dc_b, or an amount that is not known, needs
// not_max_lsn at the start; the promotion then discards it with a warning.
func TestFailover_discardConfirmed(t *testing.T) {
	notMaxLSN := &dcctlv1.Confirmation{Isolation: true, NotMaxLsn: true}

	tests := []struct {
		name    string
		change  func(dcs map[string]*dcState)
		warning string
	}{
		{
			name:    "more_wal",
			change:  func(dcs map[string]*dcState) { dcs[dcC].received = 2000 },
			warning: "the promotion of dc_b discards WAL: dc_c received up to 1000 bytes more WAL than dc_b",
		},
		{
			name:    "standby_wal_unknown",
			change:  func(dcs map[string]*dcState) { dcs[dcC].receivedUnknown = true },
			warning: "the promotion of dc_b discards WAL: the WAL received is not known of dc_c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.infra.do(func(dcs map[string]*dcState) {
					dcs[dcB].received = 1000
					tt.change(dcs)
				})

				e := w.engine()
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				_, err := e.Start(t.Context(), w.failoverPlan(isolation))
				require.Error(t, err, "not_max_lsn is not confirmed")

				_, err = e.Start(t.Context(), w.failoverPlan(notMaxLSN))
				require.NoError(t, err)

				synctest.Wait()

				op := w.op()
				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
				assert.Equal(t, []string{tt.warning}, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings())
				assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)
			})
		})
	}
}

// dc_c overtakes dc_b after the start, whose preflight needed no not_max_lsn: the
// operator did not confirm the loss, whatever the request sent, and dc_b is not promoted.
func TestFailover_standbyOvertakesTargetInterrupts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.do(func(dcs map[string]*dcState) {
			dcs[dcB].received = 1000
			dcs[dcC].received = 500
		})
		w.infra.hook("no_primary dc_a,dc_c", func(context.Context) error {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].received = 2000 })

			return nil
		})

		op := w.runFailover(&dcctlv1.Confirmation{Isolation: true, NotMaxLsn: true})

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "not_max_lsn is not confirmed: dc_c received up to 1000 bytes more WAL than dc_b")
		assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetState())
		assert.NotContains(t, w.infra.allCalls(), "promote dc_b")
		w.assertActive(6, dcB)
	})
}

// dc_c overtakes dc_b after the start and the promotion is interrupted: the resume
// confirms not_max_lsn on what dc_c received now and promotes dc_b.
func TestFailover_resumeAfterStandbyOvertakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.do(func(dcs map[string]*dcState) {
			dcs[dcB].received = 1000
			dcs[dcC].received = 500
		})
		w.infra.hook("no_primary dc_a,dc_c", func(context.Context) error {
			w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].received = 2000 })

			return nil
		})

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), w.failoverPlan(isolation))
		require.NoError(t, err)

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		op := w.resume(e, operationID(first), operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true, NotMaxLSN: true}})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)
	})
}

// With two datacenters there is no standby to compare the WAL of dc_b with: what it
// received needs no confirmation even when it is not known.
func TestFailover_twoDatacentersCompareNoWAL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.do(func(dcs map[string]*dcState) {
			dcs[dcA].unread = true
			dcs[dcB].receivedUnknown = true
			delete(dcs, dcC)
		})

		st := w.status()
		st.Datacenters = st.Datacenters[:2]

		p, err := w.fo.Plan(failoverTo(dcB, isolation), initiator, preflight.Result{Status: st})
		require.NoError(t, err)

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err = e.Start(t.Context(), p)
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, []string{"dc_a is not read, it is not known whether it is primary"},
			stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings(), "no warning of discarded WAL")
		assert.NotContains(t, w.infra.allCalls(), "repoint dc_c")
	})
}

// dc_a is read as primary with 500 bytes of WAL dc_b did not receive: the failover
// demotes it only within the max_discard_bytes the preflight asked for.
func TestFailover_demoteWithinConfirmedLoss(t *testing.T) {
	withLoss := func(w *world) {
		w.infra.do(func(dcs map[string]*dcState) {
			dcs[dcA].location = 1500
			dcs[dcB].received = 1000
			dcs[dcC].received = 1000
		})
	}
	confirmed := &dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 500}

	t.Run("covered", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			withLoss(w)

			op := w.runFailover(confirmed)

			require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
			assert.Equal(t, failoverForward, w.infra.allCalls())
			assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcA).role)
			assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)
		})
	})

	t.Run("grown_past_the_confirmation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			withLoss(w)
			w.infra.hook("failover stop dc_a from 0", func(context.Context) error {
				w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].location = 1800 })

				return nil
			})

			op := w.runFailover(confirmed)

			assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
			assert.Contains(t, op.GetError().GetDetail(), "max_discard_bytes of 800 demotes it")

			demote := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0)
			assert.Equal(t, skipped, demote.GetState())
			assert.Equal(t, []string{
				"dc_a is primary and not demoted, its demotion discards 800 bytes of WAL, 500 are confirmed: max_discard_bytes of 800 demotes it",
			}, demote.GetWarnings())
			assert.NotContains(t, w.infra.allCalls(), "demote dc_a")
			assert.NotContains(t, w.infra.allCalls(), "promote dc_b")
		})
	})

	t.Run("not_confirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			withLoss(w)

			e := w.engine()
			stop := start(t, e)

			defer stop()

			synctest.Wait()

			_, err := e.Start(t.Context(), w.failoverPlan(isolation))
			assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))
			assert.Empty(t, w.infra.allCalls())
		})
	})

	// The resume before the promotion asks for the bound again, on what dc_a holds then.
	t.Run("resumed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			withLoss(w)

			e, stop, id := w.crashFailover("failover stop dc_a from 2", confirmed)

			defer stop()

			_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: id, Initiator: initiator, Confirmations: check.Confirmations{Isolation: true}})
			assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))

			op := w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true, MaxDiscardBytes: 500}})

			require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
			assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcA).role)
		})
	})
}

// The demotion step is skipped and the promotion interrupted; the resume does not run the
// skipped step again, so the promotion demotes dc_a itself within the bound the resume
// confirmed.
func TestFailover_resumeDemotesAtPromote(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
		conf  *dcctlv1.Confirmation
		bound int64
	}{
		{
			// The loss grows past the confirmation before the demotion step.
			name: "loss_grew",
			setup: func(w *world) {
				w.infra.hook("failover stop dc_a from 0", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].location = 1800 })

					return nil
				})
			},
			conf:  &dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 500},
			bound: 800,
		},
		{
			// dc_a is not read at the demotion step and is read as primary at the promotion.
			name: "read_after_the_demotion_step",
			setup: func(w *world) {
				w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].unread = true })
				w.infra.hook("no_primary dc_a,dc_c", func(context.Context) error {
					w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].unread = false })

					return nil
				})
			},
			conf:  isolation,
			bound: 500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.infra.do(func(dcs map[string]*dcState) {
					dcs[dcA].location = 1500
					dcs[dcB].received = 1000
					dcs[dcC].received = 1000
				})
				tt.setup(w)

				e := w.engine()
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				first, err := e.Start(t.Context(), w.failoverPlan(tt.conf))
				require.NoError(t, err)

				synctest.Wait()
				require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())
				require.Equal(t, skipped, stepOf(t, w.op(), dcctlv1.Operation_Step_PATRONI_DEMOTE, 0).GetState())

				op := w.resume(e, operationID(first), operation.ResumeRequest{
					Confirmations: check.Confirmations{Isolation: true, MaxDiscardBytes: tt.bound},
				})

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
				assert.Equal(t, skipped, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0).GetState(), "the step is not run again")
				assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcA).role)
				assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)
			})
		})
	}
}

// dc_a left timeline 5 at 1000 and wrote up to 1100 on timeline 6; dc_b stayed on
// timeline 5 with 1200 received: the demotion discards 100 bytes, not none.
func TestFailover_demoteLossAcrossTimelines(t *testing.T) {
	onTimeline6 := func(w *world, leftAt int64) {
		w.infra.do(func(dcs map[string]*dcState) {
			dcs[dcA].timeline, dcs[dcA].leftAt, dcs[dcA].location = 6, leftAt, 1100
			dcs[dcB].received = 1200
			dcs[dcC].received = 1200
		})
	}

	t.Run("confirmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			onTimeline6(w, 1000)

			e := w.engine()
			stop := start(t, e)

			defer stop()

			synctest.Wait()

			_, err := e.Start(t.Context(), w.failoverPlan(isolation))
			assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))

			_, err = e.Start(t.Context(), w.failoverPlan(&dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 100}))
			require.NoError(t, err)

			synctest.Wait()

			require.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op().GetState(), w.op().GetError().GetDetail())
			assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcA).role)
		})
	})

	t.Run("history_unknown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			onTimeline6(w, 0)

			e := w.engine()
			stop := start(t, e)

			defer stop()

			synctest.Wait()

			_, err := e.Start(t.Context(), w.failoverPlan(isolation))
			assert.Equal(t, apierr.ReasonPreflightFailed, apierr.ReasonOf(err), "an amount that is not known has no bound")
			assert.Empty(t, w.infra.allCalls())
		})
	})
}

// A standby that diverged from dc_b is not repointed: it waits for repoint with the
// discard confirmed.
func TestFailover_divergedStandbyIsNotRepointed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].diverged = 300 })

		op := w.runFailover(isolation)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, []string{"dc_c is not repointed to dc_b, it diverged by 300 bytes: repoint it with the discard confirmed"},
			stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0).GetWarnings())
		assert.NotContains(t, w.infra.allCalls(), "repoint dc_c")
		assert.Equal(t, dcA, w.infra.dc(dcC).upstream)
	})
}

func TestFailover_interrupts(t *testing.T) {
	foreign := kv.State{Generation: 7, ActiveDatacenter: dcC, OpID: "other"}

	tests := []struct {
		name  string
		conf  *dcctlv1.Confirmation
		setup func(w *world)
		step  dcctlv1.Operation_Step_Type
		stage int32
		// detail is in the error of the operation.
		detail string
		calls  []string
	}{
		{
			// The demotion fails and leaves dc_a primary: dc_b is not promoted next to it.
			// Every demotion fails and leaves dc_a primary: dc_b is not promoted next to it.
			name: "source_stays_primary", conf: isolation,
			setup: func(w *world) {
				var fail func(context.Context) error

				fail = func(context.Context) error {
					w.infra.hook("demote dc_a", fail)

					return pgctl.ErrTimeout
				}

				w.infra.hook("demote dc_a", fail)
			},
			step:   dcctlv1.Operation_Step_PATRONI_PROMOTE,
			detail: "dc_a is not demoted",
			calls:  slices.Concat(failoverForward[:slices.Index(failoverForward, "demote dc_a")+1], []string{"demote dc_a"}),
		},
		{
			name: "standby_primary", conf: isolation,
			setup: func(w *world) {
				w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].role = patroni.RolePrimary })
			},
			step:   dcctlv1.Operation_Step_PATRONI_PROMOTE,
			detail: "leader dc_c1 of dc_c is primary",
			calls:  failoverForward[:slices.Index(failoverForward, "no_primary dc_a,dc_c")+1],
		},
		{
			// dc_b streams although dc_a does not answer dcctl: dc_a is alive.
			name: "target_streams_from_lost_source", conf: isolation,
			setup: func(w *world) {
				w.loseA()
				w.infra.do(func(dcs map[string]*dcState) { dcs[dcB].streaming = true })
			},
			step:   dcctlv1.Operation_Step_PATRONI_PROMOTE,
			detail: "dc_a is not read and dc_b still streams from its upstream",
			calls:  failoverLost[:slices.Index(failoverLost, "no_primary dc_a,dc_c")+1],
		},
		{
			// dc_c streams although dc_a does not answer dcctl: dc_a is alive.
			name: "standby_streams_from_lost_source", conf: isolation,
			setup: func(w *world) {
				w.loseA()
				w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].streaming = true })
			},
			step:   dcctlv1.Operation_Step_PATRONI_PROMOTE,
			detail: "dc_a is not read and dc_c still streams from its upstream",
			calls:  failoverLost[:slices.Index(failoverLost, "no_primary dc_a,dc_c")+1],
		},
		{
			// The guard of the next stage sees another active datacenter: nothing rolls back.
			name: "generation_changed_under_stop", conf: isolation,
			setup: func(w *world) {
				w.infra.hook("failover stop dc_a from 3", func(context.Context) error {
					w.setVoters(foreign)

					return nil
				})
			},
			step: dcctlv1.Operation_Step_APPS_STOP, stage: 2,
			detail: "the step acts under generation 6 of dc_b",
			calls:  []string{"failover stop dc_a from 3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)

				op := w.runFailover(tt.conf)

				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				assert.Contains(t, op.GetError().GetDetail(), tt.detail)
				assert.Equal(t, tt.calls, w.infra.allCalls(), "nothing is rolled back")
				assert.Equal(t, failed, stepOf(t, op, tt.step, tt.stage).GetState())

				for _, st := range op.GetSteps() {
					assert.False(t, kv.StepRolledBack(st), "%s stage %d", st.GetType(), st.GetStage())
				}

				assert.NotEqual(t, patroni.RolePrimary, w.infra.dc(dcB).role, "dc_b is not promoted")
				assert.True(t, w.lockFree())
			})
		})
	}
}

// The voters change between the plan and the write so that only the KV of dc_b
// answers: the generation is written forced, which the confirmed isolation allows.
func TestFailover_generationModeFollowsVoters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		p := w.failoverPlan(isolation)
		require.Equal(t, dcctlv1.Operation_QUORUM, p.Mode)

		w.voters[0].set(func(v *fakeVoter) { v.getErr = unreachable() })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })

		_, err := e.Start(t.Context(), p)
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.True(t, w.voters[1].state(t).Forced)
		assert.Equal(t, dcctlv1.Operation_FORCED, op.GetMode(), "the operation records the mode it wrote in")
	})
}

// The record of the failover reaches the KV of dc_a only; the resume confirms the
// isolation, as the API does, while the voters give a majority again: the generation is
// written by the quorum, not forced.
func TestFailover_resumeWithIsolationWritesByQuorum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.voters[1].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), w.failoverPlan(isolation))
		require.NoError(t, err)

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		w.voters[1].set(func(v *fakeVoter) { v.casErr = nil })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = nil })

		op := w.resume(e, operationID(first), isolated)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_QUORUM, op.GetMode(), "the resume asked for forced, the record is written by the quorum")

		for _, s := range w.voterStates() {
			assert.Equal(t, failedOver(op, false), withoutTime(s))
		}
	})
}

// crashFailover runs the failover with c until the primitive of key, then crashes the
// instance there. It returns a restarted engine, where the operation is interrupted,
// with its stop, and the ID of the operation.
func (w *world) crashFailover(key string, c *dcctlv1.Confirmation) (e *operation.Engine, stop func(), id string) {
	w.t.Helper()

	crashed := w.crashAt(key)

	e = w.engine()
	stop = start(w.t, e)

	synctest.Wait()

	first, err := e.Start(w.t.Context(), w.failoverPlan(c))
	require.NoError(w.t, err)

	synctest.Wait()

	stop()

	saved := crashed()
	require.NotNil(w.t, saved, "the failover reached %s", key)
	w.setRawOps(saved)

	e = w.engine()
	stop = start(w.t, e)

	synctest.Wait()

	require.Equal(w.t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

	return e, stop, operationID(first)
}

// crashFailoverAndResume is crashFailover, then the resume of the operation with req.
func (w *world) crashFailoverAndResume(key string, req operation.ResumeRequest) *dcctlv1.Operation {
	w.t.Helper()

	e, stop, id := w.crashFailover(key, isolation)

	defer stop()

	return w.resume(e, id, req)
}

// isolatedOnly confirms the isolation on a resume without asking for a forced write.
var isolatedOnly = operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true}}

func TestFailover_crashResumes(t *testing.T) {
	tests := []struct {
		key string
		req operation.ResumeRequest
	}{
		// Before the promotion the resume confirms the isolation anew.
		{key: "failover stop dc_a from 2", req: isolatedOnly},
		{key: "demote dc_a", req: isolatedOnly},
		{key: "no_primary dc_a,dc_c", req: isolatedOnly},
		{key: "promote dc_b", req: isolatedOnly},
		{key: "start dc_b to 2"},
		{key: "repoint dc_c"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)

				op := w.crashFailoverAndResume(tt.key, tt.req)

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

				a, b := w.infra.dc(dcA), w.infra.dc(dcB)
				assert.True(t, noneRunning(a))
				assert.Equal(t, patroni.RoleStandbyLeader, a.role)
				assert.True(t, allRunning(b))
				assert.Equal(t, int64(6), b.timeline, "dc_b is promoted once")
				assert.Equal(t, dcB, w.infra.dc(dcC).upstream)

				for _, s := range w.voterStates() {
					assert.Equal(t, failedOver(op, false), withoutTime(s), "the generation and its timeline are written once")
				}
			})
		})
	}
}

// The record of the failover reaches the KV of dc_a only and the arbiter does not
// answer: the operation is interrupted, and the resume finishes the record.
func TestFailover_resumeFinishesUndeterminedGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.voters[1].set(func(v *fakeVoter) { v.casErr = coord.ErrOutcomeUnknown })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = unreachable() })

		e := w.engine()
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), w.failoverPlan(isolation))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0).GetState())
		assert.False(t, kv.StepRolledBack(stepOf(t, op, dcctlv1.Operation_Step_GENERATION, 0)))
		assert.Empty(t, w.infra.allCalls())
		assert.Equal(t, []string{dcA}, w.voters[0].state(t).NeedsRecovery)

		w.voters[1].set(func(v *fakeVoter) { v.casErr = nil })
		w.voters[2].set(func(v *fakeVoter) { v.getErr = nil })

		op = w.resume(e, operationID(first), isolatedOnly)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, failoverForward, w.infra.allCalls())

		for _, s := range w.voterStates() {
			assert.Equal(t, failedOver(op, false), withoutTime(s))
		}
	})
}

// dc_a is lost and the instance stops before the promotion: a resume without the
// isolation confirmed anew is refused, one with it promotes dc_b with a warning.
func TestFailover_resumePromoteNeedsIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.loseA()

		e, stop, id := w.crashFailover("no_primary dc_a,dc_c", isolation)

		defer stop()

		_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: id, Initiator: initiator})
		require.Error(t, err, "the isolation is not confirmed")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

		op := w.resume(e, id, isolatedOnly)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, []string{"dc_a is not read, it is not known whether it is primary"},
			stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings())
		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)
	})
}

// not_max_lsn confirmed at the start does not outlive a crash before the promotion: the
// resume decides it anew on what the standbys received, and promotes with it confirmed.
func TestFailover_resumeConfirmsNotMaxLSNAnew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.infra.do(func(dcs map[string]*dcState) {
			dcs[dcB].received = 1000
			dcs[dcC].received = 2000
		})

		e, stop, id := w.crashFailover("failover stop dc_a from 2", &dcctlv1.Confirmation{Isolation: true, NotMaxLsn: true})

		defer stop()

		_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: id, Initiator: initiator, Confirmations: check.Confirmations{Isolation: true}})
		require.Error(t, err, "not_max_lsn is not confirmed")

		op := w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{Isolation: true, NotMaxLSN: true}})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, []string{"the promotion of dc_b discards WAL: dc_c received up to 1000 bytes more WAL than dc_b"},
			stepOf(t, op, dcctlv1.Operation_Step_PATRONI_PROMOTE, 0).GetWarnings())
		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcB).role)
	})
}
