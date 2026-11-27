package scenario_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// activeOf returns the active datacenter of the record the voters of view hold.
func activeOf(view coord.View) (string, error) {
	base := coord.Base(view, false)
	if base == nil {
		return "", errors.New("the active datacenter is not known")
	}

	return base.State.ActiveDatacenter, nil
}

// standbyChecks decides demote and repoint of dc as their sets do: the role of the leader
// of dc, primary or standby leader, and patroni.divergence against the active
// datacenter, which needs max_discard_bytes with a known amount and blocks without one.
func (f fakePreflight) standbyChecks(ctx context.Context, action preflight.Action, dc string) ([]check.Check, error) {
	view, err := f.view(ctx)
	if err != nil {
		return nil, err
	}

	role := check.Check{ID: check.PatroniDCPrimary, Result: check.Pass, Effect: check.Block, Datacenter: dc}
	want := patroni.RolePrimary

	if action == preflight.Repoint {
		role.ID, want = check.PatroniDCStandby, patroni.RoleStandbyLeader
	}

	if d := f.w.dc(dc); d.unread || d.role != want {
		role.Result = check.Fail
	}

	active, err := activeOf(view)
	if err != nil {
		return []check.Check{role, {ID: check.CoordDCNotActive, Result: check.Fail, Effect: check.Block, Datacenter: dc}}, nil
	}

	return []check.Check{role, f.divergence(dc, active)}, nil
}

// divergence is patroni.divergence of dc from active.
func (f fakePreflight) divergence(dc, active string) check.Check {
	c := check.Check{
		ID: check.PatroniDivergence, Result: check.Pass, Effect: check.Confirm, Confirmation: check.ConfirmMaxDiscardBytes,
		Datacenter: dc,
	}

	d, err := fakePatroni{f.w}.divergence(dc, active)

	switch {
	case err != nil:
		c.Result = check.Fail
	case d.Diverged:
		c.Result, c.DiscardBytes = check.Fail, &d.Bytes
	}

	return c
}

// resumeStandby is what a resume of op, a demote or repoint whose Patroni step did not
// succeed, decides anew: patroni.divergence of its target.
func (f fakePreflight) resumeStandby(ctx context.Context, op *dcctlv1.Operation) ([]check.Check, error) {
	step := map[dcctlv1.Operation_Type]dcctlv1.Operation_Step_Type{
		dcctlv1.Operation_DEMOTE:  dcctlv1.Operation_Step_PATRONI_DEMOTE,
		dcctlv1.Operation_REPOINT: dcctlv1.Operation_Step_PATRONI_REPOINT,
	}[op.GetType()]

	if step == dcctlv1.Operation_Step_TYPE_UNSPECIFIED || !preflight.Unfinished(op, step) {
		return nil, nil
	}

	view, err := f.view(ctx)
	if err != nil {
		return nil, err
	}

	active, err := activeOf(view)
	if err != nil {
		return nil, err
	}

	target := strings.TrimPrefix(op.GetTarget(), "datacenters/")

	// A demotion that took discards nothing more.
	if d := f.w.dc(target); op.GetType() == dcctlv1.Operation_DEMOTE && d.role == patroni.RoleStandbyLeader && d.upstream == active {
		return nil, nil
	}

	return []check.Check{f.divergence(target, active)}, nil
}

// oldPrimary makes dc_c a primary on timeline 4 that wrote diverged bytes past the point
// dc_a left that timeline, 400.
func (w *world) oldPrimary(diverged int64) {
	w.infra.do(func(dcs map[string]*dcState) {
		d := dcs[dcC]
		d.role, d.upstream, d.timeline, d.history, d.location = patroni.RolePrimary, "", 4, ancestry(4), 400+diverged
	})
}

// divergeBy makes d, an old primary or a standby on timeline 4, hold diverged bytes past
// the point dc_a left that timeline.
func divergeBy(diverged int64) func(d *dcState) {
	return func(d *dcState) {
		if d.role == patroni.RolePrimary {
			d.location = 400 + diverged
		} else {
			d.received = 400 + diverged
		}
	}
}

// historyUnknown makes d a cluster on timeline 6 whose history does not tell where it left
// timeline 5 of dc_a.
func historyUnknown(d *dcState) { d.timeline, d.history = 6, ancestry(5) }

// historyKnown undoes historyUnknown.
func historyKnown(d *dcState) { d.timeline, d.history = 4, ancestry(4) }

// atSecondRead runs fn at the second read of the divergence of dc: the first is the
// recheck before the step, the second the one of the step itself.
func (w *world) atSecondRead(dc string, fn func(d *dcState)) {
	w.infra.hook("divergence "+dc, func(context.Context) error {
		w.infra.hook("divergence "+dc, func(context.Context) error {
			w.infra.do(func(dcs map[string]*dcState) { fn(dcs[dc]) })

			return nil
		})

		return nil
	})
}

// atFirstRead runs fn at the first read of the divergence of dc, the recheck before the
// step.
func (w *world) atFirstRead(dc string, fn func(d *dcState)) {
	w.infra.hook("divergence "+dc, func(context.Context) error {
		w.infra.do(func(dcs map[string]*dcState) { fn(dcs[dc]) })

		return nil
	})
}

func demoteOf(dc string, maxDiscard int64) *dcctlv1.DemoteDatacenterRequest {
	return &dcctlv1.DemoteDatacenterRequest{
		Name: "datacenters/" + dc, Confirmation: &dcctlv1.Confirmation{MaxDiscardBytes: maxDiscard},
		RequestId: "3c2b1a09-8f7e-4d6c-9b5a-4e3d2c1b0a99",
	}
}

func repointOf(dc string, maxDiscard int64) *dcctlv1.RepointDatacenterRequest {
	return &dcctlv1.RepointDatacenterRequest{
		Name: "datacenters/" + dc, Confirmation: &dcctlv1.Confirmation{MaxDiscardBytes: maxDiscard},
		RequestId: "7a6b5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c5d",
	}
}

// standbyPlan plans action, demote or repoint, on dc_c with maxDiscard on the verdict of
// the preflight.
func (w *world) standbyPlan(action preflight.Action, maxDiscard int64) operation.Plan {
	w.t.Helper()

	v, err := w.preflight().Run(w.t.Context(), preflight.Request{
		Action: action, Params: preflight.Params{Datacenter: dcC}, Confirmations: check.Confirmations{MaxDiscardBytes: maxDiscard},
	})
	require.NoError(w.t, err)

	pre := preflight.Result{Verdict: v, Status: w.status()}

	plan := func() (operation.Plan, error) { return w.repoint.Plan(repointOf(dcC, maxDiscard), initiator, pre) }
	if action == preflight.Demote {
		plan = func() (operation.Plan, error) { return w.demote.Plan(demoteOf(dcC, maxDiscard), initiator, pre) }
	}

	p, err := plan()
	require.NoError(w.t, err)

	return p
}

// startPlan starts p on a running engine and returns the operation as it ended, or the
// refusal of the start.
func (w *world) startPlan(p operation.Plan) (*dcctlv1.Operation, error) {
	w.t.Helper()

	e := w.engine()
	stop := start(w.t, e)

	defer stop()

	synctest.Wait()

	if _, err := e.Start(w.t.Context(), p); err != nil {
		return nil, err
	}

	synctest.Wait()

	return w.op(), nil
}

// runStandby runs action on dc_c with maxDiscard and returns it as it ended.
func (w *world) runStandby(action preflight.Action, maxDiscard int64) *dcctlv1.Operation {
	w.t.Helper()

	op, err := w.startPlan(w.standbyPlan(action, maxDiscard))
	require.NoError(w.t, err)

	return op
}

// assertUntouched checks that the voters still hold generation 5 of dc_a as it was.
func (w *world) assertUntouched() {
	w.t.Helper()

	for _, s := range w.voterStates() {
		assert.Equal(w.t, kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 5}, withoutTime(s))
	}
}

func TestDemote_notDiverged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.oldPrimary(0)

		op := w.runStandby(preflight.Demote, 0)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_DEMOTE, op.GetType())
		assert.Equal(t, int64(5), op.GetPreviousGeneration())
		assert.Zero(t, op.GetNewGeneration(), "a demotion writes no generation")
		assert.Equal(t, dcctlv1.Operation_MODE_UNSPECIFIED, op.GetMode())

		for _, st := range op.GetSteps() {
			assert.Equal(t, succeeded, st.GetState(), "%s: %s", st.GetType(), st.GetDetail())
			assert.Empty(t, st.GetWarnings(), st.GetType())
		}

		assert.True(t, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0).GetPointOfNoReturn())
		assert.Equal(t, []string{"demote dc_c", "recover dc_c"}, w.infra.allCalls())

		c := w.infra.dc(dcC)
		assert.Equal(t, patroni.RoleStandbyLeader, c.role)
		assert.Equal(t, dcA, c.upstream)
		assert.Equal(t, int64(5), c.timeline, "dc_c follows dc_a on its timeline")
		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcA).role, "the active primary is never touched")
		w.assertUntouched()
		assert.True(t, w.lockFree())
	})
}

// A divergence within max_discard_bytes demotes, and the loss is a warning.
func TestDemote_divergenceConfirmed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.oldPrimary(400)

		op := w.runStandby(preflight.Demote, 400)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, int64(400), op.GetConfirmation().GetMaxDiscardBytes())

		demote := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0)
		require.Len(t, demote.GetWarnings(), 1)
		assert.Contains(t, demote.GetWarnings()[0], "discards 400 bytes")
		assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcC).role)
	})
}

// A divergence without max_discard_bytes, or with a smaller one, is refused before
// anything starts; one that is not known has no bound and is refused too.
func TestDemote_divergenceRefused(t *testing.T) {
	tests := []struct {
		name       string
		maxDiscard int64
		unknown    bool
		reason     apierr.Reason
	}{
		{name: "not_confirmed", reason: apierr.ReasonConfirmationRequired},
		{name: "smaller_bound", maxDiscard: 399, reason: apierr.ReasonConfirmationRequired},
		{name: "not_known", maxDiscard: 1 << 40, unknown: true, reason: apierr.ReasonPreflightFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.oldPrimary(400)

				if tt.unknown {
					w.infra.do(func(dcs map[string]*dcState) { historyUnknown(dcs[dcC]) })
				}

				_, err := w.startPlan(w.standbyPlan(preflight.Demote, tt.maxDiscard))
				require.Error(t, err)
				assert.Equal(t, tt.reason, apierr.ReasonOf(err))
				assert.Empty(t, w.infra.allCalls())
				assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcC).role)
				assert.Nil(t, w.op(), "no operation starts")
			})
		})
	}
}

// The divergence is decided again before the demotion: one that grew past the bound, or
// that is no longer known, fails the operation without running the step.
func TestDemote_divergenceRecheckedBeforeTheStep(t *testing.T) {
	tests := []struct {
		name   string
		change func(d *dcState)
		reason apierr.Reason
	}{
		{name: "grows", change: divergeBy(900), reason: apierr.ReasonConfirmationRequired},
		{name: "not_known", change: historyUnknown, reason: apierr.ReasonPreflightFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.oldPrimary(400)
				w.atFirstRead(dcC, tt.change)

				op := w.runStandby(preflight.Demote, 400)

				require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
				assert.Equal(t, tt.reason, apierr.Reason(op.GetError().GetReason()))
				demote := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0)
				assert.Equal(t, failed, demote.GetState())
				assert.Contains(t, demote.GetDetail(), "not run")
				assert.Empty(t, w.infra.allCalls(), "nothing is demoted")
				assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcC).role)
			})
		})
	}
}

// The step measures the divergence once more, closing the race with the recheck: one
// that grew past the bound, or that is no longer known, interrupts the operation before
// the demotion, and a resume that confirms what is measured then demotes.
func TestDemote_divergenceMeasuredAtTheStep(t *testing.T) {
	tests := []struct {
		name   string
		change func(d *dcState)
		detail string
		// resume is max_discard_bytes of the resume, which the preflight then asks for.
		resume int64
	}{
		{name: "grows", change: divergeBy(900), detail: "max_discard_bytes of 900", resume: 900},
		{name: "not_known", change: historyUnknown, detail: "is not known", resume: 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.oldPrimary(400)
				w.atSecondRead(dcC, tt.change)

				e := w.engine()
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				first, err := e.Start(t.Context(), w.standbyPlan(preflight.Demote, 400))
				require.NoError(t, err)

				synctest.Wait()

				op := w.op()
				require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				demote := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0)
				assert.Equal(t, failed, demote.GetState())
				assert.Contains(t, demote.GetDetail(), tt.detail)
				assert.Empty(t, w.infra.allCalls(), "nothing is demoted")
				assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcC).role)

				w.infra.do(func(dcs map[string]*dcState) { historyKnown(dcs[dcC]) })

				_, err = e.Resume(t.Context(), operation.ResumeRequest{
					Operation: operationID(first), Initiator: initiator, Confirmations: check.Confirmations{MaxDiscardBytes: tt.resume - 1},
				})
				require.Error(t, err)
				assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err), "the resume measures anew")

				op = w.resume(e, operationID(first), operation.ResumeRequest{Confirmations: check.Confirmations{MaxDiscardBytes: tt.resume}})
				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
				assert.Equal(t, patroni.RoleStandbyLeader, w.infra.dc(dcC).role)
			})
		})
	}
}

// Every failure at or after the demotion leaves the decision to the operator; the
// active datacenter must be primary when the step demotes.
func TestDemote_interrupts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world)
		calls []string
	}{
		{
			name: "active_not_primary",
			setup: func(w *world) {
				w.infra.hook("divergence dc_c", func(context.Context) error {
					w.infra.hook("divergence dc_c", func(context.Context) error {
						w.infra.do(func(dcs map[string]*dcState) { dcs[dcA].role = patroni.RoleStandbyLeader })

						return nil
					})

					return nil
				})
			},
		},
		{
			name:  "demote_fails",
			setup: func(w *world) { w.infra.fail("demote dc_c", fmt.Errorf("demote: %w", pgctl.ErrTimeout)) },
			calls: []string{"demote dc_c"},
		},
		{
			name: "generation_changes_under_the_step",
			setup: func(w *world) {
				w.infra.hook("divergence dc_c", func(context.Context) error {
					w.setVoters(kv.State{Generation: 6, ActiveDatacenter: dcB, OpID: "other", PatroniTimeline: 6})

					return nil
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.oldPrimary(0)

				p := w.standbyPlan(preflight.Demote, 0)

				tt.setup(w)

				op, err := w.startPlan(p)
				require.NoError(t, err)
				require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				assert.Equal(t, failed, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0).GetState())
				assert.Equal(t, tt.calls, w.infra.allCalls())
				assert.True(t, w.lockFree())
			})
		})
	}
}

// Members that do not follow after the demotion are a warning: the old primary is a
// standby already, the rest needs a rebuild by hand.
func TestDemote_followIsAWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.oldPrimary(0)
		w.infra.do(func(dcs map[string]*dcState) { dcs[dcC].lagging = true })

		op := w.runStandby(preflight.Demote, 0)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

		follow := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_FOLLOW, 0)
		assert.Equal(t, succeeded, follow.GetState())
		require.Len(t, follow.GetWarnings(), 1)
		assert.Contains(t, follow.GetWarnings()[0], "rebuild by hand")
		assert.Contains(t, follow.GetWarnings()[0], "dc_c2")
	})
}

// crashStandby runs plan until the primitive of key, then crashes the instance there.
// It returns a restarted engine, where the operation is interrupted, with its stop and
// the ID of the operation.
func (w *world) crashStandby(key string, p operation.Plan) (e *operation.Engine, stop func(), id string) {
	w.t.Helper()

	crashed := w.crashAt(key)

	e = w.engine()
	stop = start(w.t, e)

	synctest.Wait()

	first, err := e.Start(w.t.Context(), p)
	require.NoError(w.t, err)

	synctest.Wait()

	stop()

	saved := crashed()
	require.NotNil(w.t, saved, "the operation reached %s", key)
	w.setRawOps(saved)

	e = w.engine()
	stop = start(w.t, e)

	synctest.Wait()

	require.Equal(w.t, dcctlv1.Operation_INTERRUPTED, w.op().GetState())

	return e, stop, operationID(first)
}

// A demotion crashed at each step resumes; before the demotion succeeded the resume
// confirms the divergence anew.
func TestDemote_crashResumes(t *testing.T) {
	tests := []struct {
		key string
		req operation.ResumeRequest
	}{
		{key: "demote dc_c", req: operation.ResumeRequest{Confirmations: check.Confirmations{MaxDiscardBytes: 400}}},
		{key: "recover dc_c"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.oldPrimary(400)

				e, stop, id := w.crashStandby(tt.key, w.standbyPlan(preflight.Demote, 400))

				defer stop()

				op := w.resume(e, id, tt.req)

				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

				c := w.infra.dc(dcC)
				assert.Equal(t, patroni.RoleStandbyLeader, c.role)
				assert.Equal(t, dcA, c.upstream)
				assert.Equal(t, int64(5), c.timeline)
				w.assertUntouched()
			})
		})
	}
}

// A demotion crashed before it demoted is not resumed without the discard confirmed
// anew.
func TestDemote_crashResumeNeedsDiscard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.oldPrimary(400)

		e, stop, id := w.crashStandby("demote dc_c", w.standbyPlan(preflight.Demote, 400))

		defer stop()

		_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: id, Initiator: initiator})
		require.Error(t, err)
		assert.Equal(t, apierr.ReasonConfirmationRequired, apierr.ReasonOf(err))
		assert.Equal(t, patroni.RolePrimary, w.infra.dc(dcC).role)
	})
}

// A demotion interrupted after Patroni took it is resumed without the discard: dc_c, a
// standby of dc_a that is not rewound yet, tells no divergence, and the step does not
// measure it again.
func TestDemote_resumeAfterTheDemotionTook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.oldPrimary(400)

		e, stop, id := w.crashStandby("demote dc_c", w.standbyPlan(preflight.Demote, 400))

		defer stop()

		w.infra.do(func(dcs map[string]*dcState) {
			d := dcs[dcC]
			d.role, d.upstream, d.receivedUnknown = patroni.RoleStandbyLeader, dcA, true
		})

		op := w.resume(e, id, operation.ResumeRequest{})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Empty(t, stepOf(t, op, dcctlv1.Operation_Step_PATRONI_DEMOTE, 0).GetWarnings(), "no discard is measured")
		assert.Equal(t, int64(5), w.infra.dc(dcC).timeline, "dc_c follows dc_a")
	})
}

// detachedC makes dc_c a standby leader on timeline 4 that follows no datacenter, and
// received diverged bytes past the point dc_a left that timeline.
func (w *world) detachedC(diverged int64) {
	w.infra.do(func(dcs map[string]*dcState) {
		d := dcs[dcC]
		d.upstream, d.timeline, d.history, d.received = "", 4, ancestry(4), 400+diverged
	})
}

func TestRepoint_notDiverged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.detachedC(0)

		op := w.runStandby(preflight.Repoint, 0)

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcctlv1.Operation_REPOINT, op.GetType())
		assert.Zero(t, op.GetNewGeneration())

		repoint := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0)
		assert.Equal(t, succeeded, repoint.GetState())
		assert.Empty(t, repoint.GetWarnings())
		assert.Equal(t, []string{"repoint dc_c"}, w.infra.allCalls())
		assert.Equal(t, dcA, w.infra.dc(dcC).upstream)
		w.assertUntouched()
	})
}

func TestRepoint_divergence(t *testing.T) {
	tests := []struct {
		name       string
		maxDiscard int64
		unknown    bool
		// reason is the refusal of the start; empty when the repoint runs.
		reason apierr.Reason
	}{
		{name: "confirmed", maxDiscard: 400},
		{name: "not_confirmed", reason: apierr.ReasonConfirmationRequired},
		{name: "smaller_bound", maxDiscard: 100, reason: apierr.ReasonConfirmationRequired},
		{name: "not_known", maxDiscard: 1 << 40, unknown: true, reason: apierr.ReasonPreflightFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.detachedC(400)

				if tt.unknown {
					w.infra.do(func(dcs map[string]*dcState) { historyUnknown(dcs[dcC]) })
				}

				op, err := w.startPlan(w.standbyPlan(preflight.Repoint, tt.maxDiscard))

				if tt.reason != "" {
					require.Error(t, err)
					assert.Equal(t, tt.reason, apierr.ReasonOf(err))
					assert.Empty(t, w.infra.allCalls())

					return
				}

				require.NoError(t, err)
				require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())

				repoint := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0)
				require.Len(t, repoint.GetWarnings(), 1)
				assert.Contains(t, repoint.GetWarnings()[0], "discards 400 bytes")
				assert.Equal(t, dcA, w.infra.dc(dcC).upstream)
			})
		})
	}
}

// What the step reads anew interrupts it before the repoint: a divergence past the bound
// or not known, or dc_c turned primary, which needs a demotion.
func TestRepoint_interrupts(t *testing.T) {
	tests := []struct {
		name   string
		change func(d *dcState)
		detail string
	}{
		{name: "divergence_grows", change: divergeBy(900), detail: "max_discard_bytes of 900"},
		{name: "divergence_not_known", change: historyUnknown, detail: "is not known"},
		{name: "turned_primary", change: func(d *dcState) { d.role = patroni.RolePrimary }, detail: "demoted, not repointed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.detachedC(400)
				w.atSecondRead(dcC, tt.change)

				op := w.runStandby(preflight.Repoint, 400)

				require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				repoint := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0)
				assert.Equal(t, failed, repoint.GetState())
				assert.Contains(t, repoint.GetDetail(), tt.detail)
				assert.Empty(t, w.infra.allCalls(), "nothing is repointed")
			})
		})
	}
}

// The same reads before the step fail the operation without running it.
func TestRepoint_recheckedBeforeTheStep(t *testing.T) {
	tests := []struct {
		name   string
		change func(d *dcState)
	}{
		{name: "divergence_grows", change: divergeBy(900)},
		{name: "divergence_not_known", change: historyUnknown},
		{name: "turned_primary", change: func(d *dcState) { d.role = patroni.RolePrimary }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.detachedC(400)
				w.atFirstRead(dcC, tt.change)

				op := w.runStandby(preflight.Repoint, 400)

				require.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
				repoint := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0)
				assert.Equal(t, failed, repoint.GetState())
				assert.Contains(t, repoint.GetDetail(), "not run")
				assert.Empty(t, w.infra.allCalls())
			})
		})
	}
}

// A repoint that does not complete in time, or whose outcome is not known, is a warning
// and the standby stays as it is; one the cluster refuses as a precondition leaves the
// decision to the operator.
func TestRepoint_failure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// warned is set when the repoint ends with a warning, interrupted otherwise.
		warned bool
	}{
		{name: "timeout", err: fmt.Errorf("repoint: %w", pgctl.ErrTimeout), warned: true},
		{name: "outcome_unknown", err: fmt.Errorf("repoint: %w: %w", pgctl.ErrOutcomeUnknown, pgctl.ErrTimeout), warned: true},
		{name: "precondition", err: fmt.Errorf("repoint: %w: leader is primary", pgctl.ErrPrecondition)},
		{name: "primary", err: &pgctl.PrimaryError{Datacenter: dcC, Where: "leader dc_c1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				w.detachedC(0)
				w.infra.fail("repoint dc_c", tt.err)

				op := w.runStandby(preflight.Repoint, 0)
				repoint := stepOf(t, op, dcctlv1.Operation_Step_PATRONI_REPOINT, 0)

				if tt.warned {
					require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
					require.Len(t, repoint.GetWarnings(), 1)
					assert.Contains(t, repoint.GetWarnings()[0], "dc_c is not repointed to dc_a")
				} else {
					require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
					assert.Equal(t, failed, repoint.GetState())
				}

				assert.Empty(t, w.infra.dc(dcC).upstream)
			})
		})
	}
}

func TestRepoint_crashResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.detachedC(400)

		e, stop, id := w.crashStandby("repoint dc_c", w.standbyPlan(preflight.Repoint, 400))

		defer stop()

		op := w.resume(e, id, operation.ResumeRequest{Confirmations: check.Confirmations{MaxDiscardBytes: 400}})

		require.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState(), op.GetError().GetDetail())
		assert.Equal(t, dcA, w.infra.dc(dcC).upstream)
	})
}

func TestStandbyPlan(t *testing.T) {
	w := newWorld(t)
	w.oldPrimary(400)

	p := w.standbyPlan(preflight.Demote, 400)

	assert.Equal(t, dcctlv1.Operation_DEMOTE, p.Type)
	assert.Equal(t, dcC, p.Target)
	assert.Equal(t, int64(5), p.PreviousGeneration)
	assert.Equal(t, dcA, p.PreviousActive)
	assert.Zero(t, p.NewGeneration)
	assert.Equal(t, preflight.Request{
		Action: preflight.Demote, Params: preflight.Params{Datacenter: dcC}, Confirmations: check.Confirmations{MaxDiscardBytes: 400},
	}, p.Preflight)

	types := make([]dcctlv1.Operation_Step_Type, 0, len(p.Steps))
	for _, st := range p.Pending() {
		types = append(types, st.GetType())
	}

	assert.Equal(t, []dcctlv1.Operation_Step_Type{dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_PATRONI_FOLLOW}, types)

	p = w.standbyPlan(preflight.Repoint, 0)
	assert.Equal(t, dcctlv1.Operation_REPOINT, p.Type)
	require.Len(t, p.Steps, 1)
	assert.Equal(t, dcctlv1.Operation_Step_PATRONI_REPOINT, p.Steps[0].Type)
	assert.True(t, p.Steps[0].PointOfNoReturn)
}

func TestStandbyPlan_cannotPlan(t *testing.T) {
	tests := []struct {
		name string
		dc   string
		edit func(w *world, pre *preflight.Result)
	}{
		{name: "active", dc: dcA},
		{name: "not_configured", dc: "dc_x"},
		{name: "stretch", dc: dcC, edit: func(_ *world, pre *preflight.Result) { pre.Status.Profile = "stretch" }},
		{name: "active_not_known", dc: dcC, edit: func(_ *world, pre *preflight.Result) { pre.Status.View = coord.View{} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			pre := preflight.Result{Status: w.status()}

			if tt.edit != nil {
				tt.edit(w, &pre)
			}

			_, err := w.demote.Plan(demoteOf(tt.dc, 0), initiator, pre)
			require.ErrorIs(t, err, scenario.ErrCannotPlan)

			_, err = w.repoint.Plan(repointOf(tt.dc, 0), initiator, pre)
			require.ErrorIs(t, err, scenario.ErrCannotPlan)
		})
	}
}

func TestStandbyResume_refuses(t *testing.T) {
	w := newWorld(t)

	tests := []struct {
		name string
		op   *dcctlv1.Operation
	}{
		{name: "other_type", op: &dcctlv1.Operation{Type: dcctlv1.Operation_SWITCH, Target: "datacenters/dc_c", PreviousGeneration: 5}},
		{name: "no_target", op: &dcctlv1.Operation{Type: dcctlv1.Operation_DEMOTE, PreviousGeneration: 5}},
		{name: "no_generation", op: &dcctlv1.Operation{Type: dcctlv1.Operation_DEMOTE, Target: "datacenters/dc_c"}},
		{name: "not_configured", op: &dcctlv1.Operation{Type: dcctlv1.Operation_DEMOTE, Target: "datacenters/dc_x", PreviousGeneration: 5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := w.demote.Resume(t.Context(), tt.op)
			require.Error(t, err)
		})
	}

	steps, err := w.repoint.Resume(t.Context(), &dcctlv1.Operation{
		Type: dcctlv1.Operation_REPOINT, Target: "datacenters/dc_c", PreviousGeneration: 5,
	})
	require.NoError(t, err)
	require.Len(t, steps, 1)
}
