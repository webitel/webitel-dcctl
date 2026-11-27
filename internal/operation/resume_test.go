package operation_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
)

type resumerFunc func(ctx context.Context, op *dcctlv1.Operation) ([]operation.Step, error)

func (f resumerFunc) Resume(ctx context.Context, op *dcctlv1.Operation) ([]operation.Step, error) {
	return f(ctx, op)
}

// switchSteps are the steps of [interruptedOp] recorded by tr.
func switchSteps(tr *trace, resumed map[string]bool) []operation.Step {
	steps := []operation.Step{
		tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil),
		tr.step(dcctlv1.Operation_Step_PATRONI_DEMOTE, "demote", nil),
		tr.step(dcctlv1.Operation_Step_PATRONI_PROMOTE, "promote", nil),
	}
	steps[0].Stage = 1
	steps[2].PointOfNoReturn = true

	for i, name := range []string{"stop", "demote", "promote"} {
		run := steps[i].Run
		steps[i].Run = func(ctx context.Context, x *operation.Exec) error {
			resumed[name] = x.Resumed()

			return run(ctx, x)
		}
	}

	return steps
}

func TestRestartInterruptsAndResumeContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		// The instance crashed while it ran the second step; dc_b missed the
		// last change.
		crashed := interruptedOp("op1", 4)
		crashed.State = dcctlv1.Operation_RUNNING
		crashed.Leader = selfLeader()
		crashed.Steps[1].State = dcctlv1.Operation_Step_RUNNING
		w.consuls["dc_a"].setOp(t, crashed)

		behind := interruptedOp("op1", 3)
		behind.State = dcctlv1.Operation_RUNNING
		behind.Leader = selfLeader()
		behind.Steps[1].State = pending
		w.consuls["dc_b"].setOp(t, behind)

		var tr trace

		resumed := map[string]bool{}
		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			return switchSteps(&tr, resumed), nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		for _, dc := range []string{"dc_a", "dc_b"} {
			op := w.op(dc)
			assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState(), dc)
			assert.Equal(t, int64(5), op.GetRevision(), dc)
			assert.Equal(t, failed, op.GetSteps()[1].GetState(), dc)
			assert.Contains(t, op.GetSteps()[1].GetDetail(), "its effect may have happened", dc)
		}

		first, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.NoError(t, err)
		assert.Equal(t, dcctlv1.Operation_RUNNING, first.GetState())
		assert.Equal(t, int64(6), first.GetRevision())

		synctest.Wait()

		assert.Equal(t, []string{"run demote", "run promote"}, tr.all())
		assert.Equal(t, map[string]bool{"demote": true, "promote": false}, resumed)

		op := w.op("dc_b")
		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState())
		assert.Equal(t, []dcctlv1.Operation_Step_State{succeeded, succeeded, succeeded}, stepStates(op))
		assert.Equal(t, int64(6+2*2+1), op.GetRevision())
		assert.Equal(t, op.GetRevision(), w.op("dc_a").GetRevision())
		assert.True(t, w.lockFree())
		assert.Equal(t, []string{"interrupt", "resume", "end"}, w.journal.events(dcctlv1.JournalEntry_OPERATION))
	})
}

func TestResumeContinuesFromHighestRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.consuls["dc_a"].setOp(t, interruptedOp("op1", 3))
		w.consuls["dc_b"].setOp(t, interruptedOp("op1", 9))

		var tr trace

		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			return switchSteps(&tr, map[string]bool{}), nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.NoError(t, err)
		assert.Equal(t, int64(10), first.GetRevision())
		assert.Equal(t, "node-b", first.GetLeader().GetNode())

		synctest.Wait()

		assert.Equal(t, w.op("dc_a").GetRevision(), w.op("dc_b").GetRevision())
	})
}

func TestResumeRefused(t *testing.T) {
	unknownType := interruptedOp("op1", 3)
	unknownType.Steps[2].Type = 99

	unspecified := interruptedOp("op1", 3)
	unspecified.Steps[2].Type = dcctlv1.Operation_Step_TYPE_UNSPECIFIED

	succeededOp := interruptedOp("op1", 3)
	succeededOp.State = dcctlv1.Operation_SUCCEEDED

	// The rollback of the first step failed after the third step failed.
	rolledBack := interruptedOp("op1", 3)
	rolledBack.Steps[0].Detail = kv.RollbackDetail("", errors.New("start refused"))
	rolledBack.Steps[2].State = failed

	demote := interruptedOp("op1", 3)
	demote.Type = dcctlv1.Operation_DEMOTE

	tests := []struct {
		name string
		// stored is nil for an operation no copy holds.
		stored *dcctlv1.Operation
		want   error
		code   codes.Code
		reason string
	}{
		{name: "unknown_step_type", stored: unknownType, want: operation.ErrUnknownStepType, code: codes.FailedPrecondition, reason: "PREFLIGHT_FAILED"},
		{name: "unspecified_step_type", stored: unspecified, want: operation.ErrUnknownStepType, code: codes.FailedPrecondition, reason: "PREFLIGHT_FAILED"},
		{name: "rolled_back", stored: rolledBack, want: operation.ErrRolledBack, code: codes.FailedPrecondition, reason: "PREFLIGHT_FAILED"},
		{name: "no_resumer", stored: demote, want: operation.ErrNoResumer, code: codes.Unimplemented},
		{name: "succeeded", stored: succeededOp, code: codes.FailedPrecondition, reason: "OPERATION_NOT_INTERRUPTED"},
		{name: "not_found", code: codes.NotFound, reason: "OPERATION_NOT_FOUND"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				if tt.stored != nil {
					w.setOp(tt.stored)
				}

				var tr trace

				w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
					return switchSteps(&tr, map[string]bool{}), nil
				})

				e := w.engine(self)
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
				require.Error(t, err)

				if tt.want != nil {
					require.ErrorIs(t, err, tt.want)
				}

				assert.Equal(t, tt.code, statusOf(err).Code())
				assert.Equal(t, tt.reason, reasonOf(err))

				synctest.Wait()

				assert.Empty(t, tr.all())
				assert.True(t, w.lockFree())

				if tt.stored != nil {
					assert.Equal(t, tt.stored.GetRevision(), w.op("dc_a").GetRevision())
				}
			})
		})
	}
}

func TestAbandonRevertsUncommittedGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(interruptedOp("op1", 4))

		// Only the arbiter took generation 6: uncommitted.
		w.voters[2] = newVoter(t, coord.ArbiterName, coord.KindArbiter,
			kv.State{Generation: 6, ActiveDatacenter: "dc_b", OpID: "op1"})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		op, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.NoError(t, err)

		assert.Equal(t, dcctlv1.Operation_ABANDONED, op.GetState())
		assert.Equal(t, int64(5), op.GetRevision())
		assert.Equal(t, dcctlv1.Operation_ABANDONED, w.op("dc_a").GetState())
		assert.Equal(t, kv.State{Generation: 5, ActiveDatacenter: "dc_a", OpID: "old"}.Generation, w.voters[2].state(t).Generation)
		assert.Equal(t, "dc_a", w.voters[2].state(t).ActiveDatacenter)
		assert.True(t, w.lockFree())
		assert.Equal(t, []dcctlv1.Operation_State{dcctlv1.Operation_ABANDONED}, w.recorder.operations())
		assert.Equal(t, []string{"revert", "abandon"}, w.journal.events(dcctlv1.JournalEntry_OPERATION))

		revert := w.journal.event("revert")
		require.Len(t, revert, 1)
		assert.Equal(t, "generation 6 reverted to 5 on arbiter", revert[0].GetDetail())
		assert.Equal(t, dcctlv1.JournalEntry_OK, revert[0].GetResult())
	})
}

func TestAbandonUndeterminedNeedsConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(interruptedOp("op1", 4))

		// dc_b does not answer and may hold generation 6 too.
		w.voters[1].setUnreachable()
		w.voters[2] = newVoter(t, coord.ArbiterName, coord.KindArbiter,
			kv.State{Generation: 6, ActiveDatacenter: "dc_b", OpID: "op1"})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.ErrorIs(t, err, coord.ErrConfirmationRequired)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Equal(t, "CONFIRMATION_REQUIRED", reasonOf(err))

		revert := w.journal.event("revert")
		require.Len(t, revert, 1)
		assert.Contains(t, revert[0].GetDetail(), "generation 6 is not reverted")
		assert.Equal(t, dcctlv1.JournalEntry_WARNING, revert[0].GetResult())
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_a").GetState())
		assert.True(t, w.lockFree())

		op, err := e.Abandon(t.Context(), operation.AbandonRequest{
			Operation: "op1", Confirmations: check.Confirmations{Isolation: true}, Initiator: &dcctlv1.Initiator{User: "admin"},
		})
		require.NoError(t, err)
		assert.Equal(t, dcctlv1.Operation_ABANDONED, op.GetState())
		assert.Equal(t, int64(5), w.voters[2].state(t).Generation)
	})
}

func TestAbandonRefusesNotInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		running := interruptedOp("op1", 4)
		running.State = dcctlv1.Operation_RUNNING
		w.setOp(running)
		w.consuls["dc_a"].holdLock(t, kv.Lock{Instance: "node-x", Operation: "op1"})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		w.consuls["dc_a"].expireAll()

		_, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})

		var notInterrupted *operation.NotInterruptedError
		require.ErrorAs(t, err, &notInterrupted)
		assert.Equal(t, dcctlv1.Operation_RUNNING, notInterrupted.State)
	})
}

func TestResumeRefusedWithAnotherUnfinishedOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.consuls["dc_a"].setOp(t, interruptedOp("op1", 3))
		// A forced failover without dc_a left another operation in dc_b.
		w.consuls["dc_b"].setOp(t, interruptedOp("op2", 2))

		var tr trace

		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			return switchSteps(&tr, map[string]bool{}), nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})

		var running *coord.OperationRunningError
		require.ErrorAs(t, err, &running)
		assert.Equal(t, "OPERATION_RUNNING", reasonOf(err))

		synctest.Wait()

		assert.Empty(t, tr.all())
		assert.Equal(t, int64(3), w.op("dc_a").GetRevision())
		assert.Equal(t, "operations/op2", w.op("dc_b").GetName())
		assert.True(t, w.lockFree())
	})
}

func TestResumeStopsOnAnotherOperationFoundOnWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.consuls["dc_a"].setOp(t, interruptedOp("op1", 3))
		w.consuls["dc_b"].setOp(t, interruptedOp("op2", 2))

		var tr trace

		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			return switchSteps(&tr, map[string]bool{}), nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		// dc_b does not answer the read under the lock, then answers the write.
		var reads atomic.Int32

		w.consuls["dc_b"].set(func(f *fakeConsul) {
			f.onGet = func(key string) error {
				if key == kv.OpKey && reads.Add(1) == 1 {
					return unreachable()
				}

				return nil
			}
		})

		_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})

		var running *coord.OperationRunningError
		require.ErrorAs(t, err, &running)
		assert.Equal(t, "operations/op2", running.Operation.GetName())

		synctest.Wait()

		assert.Empty(t, tr.all())

		op := w.op("dc_a")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Equal(t, int64(5), op.GetRevision(), "resumed at 4 and interrupted again at 5")
		assert.Equal(t, "operations/op2", w.op("dc_b").GetName())
		assert.True(t, w.lockFree())
	})
}

func TestResumeRefusedAfterFailedRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		var tr trace

		build := func(catchupErr error) []operation.Step {
			stopApps := tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)
			stopApps.Rollback = func(context.Context, *operation.Exec) error {
				tr.add("undo stop")

				return errors.New("start refused")
			}

			return []operation.Step{
				stopApps,
				tr.step(dcctlv1.Operation_Step_PATRONI_DEMOTE, "demote", nil),
				tr.step(dcctlv1.Operation_Step_PATRONI_CATCHUP, "catchup", catchupErr),
			}
		}

		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			return build(nil), nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), switchPlan(build(errors.New("lag does not shrink"))...))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op("dc_a")
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Equal(t, "rollback failed: start refused", op.GetSteps()[0].GetDetail())
		assert.Equal(t, "rolled back", op.GetSteps()[1].GetDetail())
		assert.Equal(t, "lag does not shrink; rolled back", op.GetSteps()[2].GetDetail())

		ran := len(tr.all())

		_, err = e.Resume(t.Context(), operation.ResumeRequest{
			Operation: strings.TrimPrefix(first.GetName(), "operations/"), Initiator: &dcctlv1.Initiator{User: "admin"},
		})
		require.ErrorIs(t, err, operation.ErrRolledBack)
		assert.Equal(t, "PREFLIGHT_FAILED", reasonOf(err))

		synctest.Wait()

		assert.Len(t, tr.all(), ran, "no step runs again")
		assert.Equal(t, op.GetRevision(), w.op("dc_a").GetRevision())
		assert.True(t, w.lockFree())
	})
}

func TestAbandonJournalsRevertThatDidNotFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(interruptedOp("op1", 4))

		arb := newVoter(t, coord.ArbiterName, coord.KindArbiter, kv.State{Generation: 6, ActiveDatacenter: "dc_b", OpID: "op1"})
		arb.casErr = unreachable()
		w.voters[2] = arb

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.ErrorIs(t, err, coord.ErrRevertIncomplete)

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_a").GetState())
		assert.Equal(t, []string{"revert"}, w.journal.events(dcctlv1.JournalEntry_OPERATION))
		assert.Equal(t, []dcctlv1.JournalEntry_Result{dcctlv1.JournalEntry_ERROR}, w.journal.results(dcctlv1.JournalEntry_OPERATION))
		assert.True(t, w.lockFree())
	})
}

func TestAbandonNotRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		op := interruptedOp("op1", 4)
		op.NewGeneration = 0
		w.setOp(op)

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		for _, c := range w.consuls {
			c.set(func(f *fakeConsul) { f.casErr = func(string) error { return unreachable() } })
		}

		_, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.Error(t, err)

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_a").GetState())
		assert.Equal(t, []string{"abandon"}, w.journal.events(dcctlv1.JournalEntry_OPERATION))
		assert.Equal(t, []dcctlv1.JournalEntry_Result{dcctlv1.JournalEntry_ERROR}, w.journal.results(dcctlv1.JournalEntry_OPERATION))
	})
}

func TestAbandonKeepsCommittedGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(interruptedOp("op1", 4))

		// A majority took generation 6: it is committed.
		taken := kv.State{Generation: 6, ActiveDatacenter: "dc_b", OpID: "op1"}
		w.voters[1] = newVoter(t, "dc_b", coord.KindKV, taken)
		w.voters[2] = newVoter(t, coord.ArbiterName, coord.KindArbiter, taken)

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		op, err := e.Abandon(t.Context(), operation.AbandonRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.NoError(t, err)
		assert.Equal(t, dcctlv1.Operation_ABANDONED, op.GetState())
		assert.Equal(t, int64(6), w.voters[2].state(t).Generation)

		revert := w.journal.event("revert")
		require.Len(t, revert, 1)
		assert.Equal(t, "generation 6 is committed and stays", revert[0].GetDetail())
		assert.Equal(t, dcctlv1.JournalEntry_WARNING, revert[0].GetResult())
	})
}

// A step reads the confirmations its run was accepted with: none at the start, the
// isolation the resume confirmed after it.
func TestExecConfirmedOfTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		var (
			seen []check.Confirmations
			runs atomic.Int32
		)

		steps := func() []operation.Step {
			return []operation.Step{{Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, Run: func(_ context.Context, x *operation.Exec) error {
				seen = append(seen, x.Confirmed())

				if runs.Add(1) == 1 {
					return operation.Interrupt(errors.New("dc_a is not read"))
				}

				return nil
			}}}
		}

		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			return steps(), nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		first, err := e.Start(t.Context(), switchPlan(steps()...))
		require.NoError(t, err)

		synctest.Wait()
		require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_b").GetState())

		w.preflight.mu.Lock()
		w.preflight.verdict = check.Verdict{Outcome: check.Allowed, Confirmed: check.Confirmations{Isolation: true}}
		w.preflight.mu.Unlock()

		_, err = e.Resume(t.Context(), operation.ResumeRequest{
			Operation: strings.TrimPrefix(first.GetName(), "operations/"), ToForced: true,
			Confirmations: check.Confirmations{Isolation: true}, Initiator: &dcctlv1.Initiator{User: "admin"},
		})
		require.NoError(t, err)

		synctest.Wait()

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op("dc_b").GetState())
		assert.Equal(t, []check.Confirmations{{}, {Isolation: true}}, seen)
	})
}

// A resume that confirms the isolation sets the mode FORCED only while the generation
// is still to be written; one written already keeps the mode it was written in.
func TestResumeToForcedKeepsWrittenMode(t *testing.T) {
	tests := []struct {
		name string
		// written marks the generation step succeeded before the interruption.
		written bool
		want    dcctlv1.Operation_Mode
	}{
		{name: "generation_written", written: true, want: dcctlv1.Operation_QUORUM},
		{name: "generation_not_written", want: dcctlv1.Operation_FORCED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)

				var runs atomic.Int32

				promote := func(context.Context, *operation.Exec) error {
					if runs.Add(1) == 1 {
						return operation.Interrupt(errors.New("dc_a is not read"))
					}

					return nil
				}

				steps := func() []operation.Step {
					gen := operation.Step{Type: dcctlv1.Operation_Step_GENERATION, Run: func(context.Context, *operation.Exec) error { return nil }}
					if !tt.written {
						gen.Run = promote
					}

					return []operation.Step{gen, {Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, Run: promote}}
				}

				w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
					return steps(), nil
				})

				e := w.engine(self)
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				first, err := e.Start(t.Context(), switchPlan(steps()...))
				require.NoError(t, err)

				synctest.Wait()
				require.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_b").GetState())

				w.preflight.mu.Lock()
				w.preflight.verdict = check.Verdict{Outcome: check.Allowed, Confirmed: check.Confirmations{Isolation: true}}
				w.preflight.mu.Unlock()

				_, err = e.Resume(t.Context(), operation.ResumeRequest{
					Operation: strings.TrimPrefix(first.GetName(), "operations/"), ToForced: true,
					Confirmations: check.Confirmations{Isolation: true}, Initiator: &dcctlv1.Initiator{User: "admin"},
				})
				require.NoError(t, err)

				synctest.Wait()

				op := w.op("dc_b")
				assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState())
				assert.Equal(t, tt.want, op.GetMode())
			})
		})
	}
}

// A resumed step that had run before, at the point of no return, is refused by its
// recheck: its effect may have happened, so the operation is interrupted, not rolled
// back and failed.
func TestResumedRecheckRefusalInterruptsPastPointOfNoReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		op := interruptedOp("op1", 4)
		op.Steps[1].State, op.Steps[2].State = succeeded, failed

		for _, dc := range []string{"dc_a", "dc_b"} {
			w.consuls[dc].setOp(t, op)
		}

		var tr trace

		w.resumers[dcctlv1.Operation_SWITCH] = resumerFunc(func(context.Context, *dcctlv1.Operation) ([]operation.Step, error) {
			steps := switchSteps(&tr, map[string]bool{})
			steps[2].Recheck = func(context.Context) ([]check.Check, error) {
				return []check.Check{{ID: "patroni.divergence", Result: check.Fail, Effect: check.Confirm, Confirmation: check.ConfirmMaxDiscardBytes}}, nil
			}

			return steps, nil
		})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Resume(t.Context(), operation.ResumeRequest{Operation: "op1", Initiator: &dcctlv1.Initiator{User: "admin"}})
		require.NoError(t, err)

		synctest.Wait()

		got := w.op("dc_b")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, got.GetState())
		assert.Equal(t, failed, got.GetSteps()[2].GetState())
		assert.Empty(t, tr.all(), "nothing runs and nothing is rolled back")
		assert.True(t, w.lockFree())
	})
}
