package operation_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

// trace records what the steps did, in order.
type trace struct {
	mu     sync.Mutex
	events []string
}

func (tr *trace) add(event string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	tr.events = append(tr.events, event)
}

func (tr *trace) all() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return slices.Clone(tr.events)
}

// step returns a step of type t that records "run <name>" and ends with err,
// and whose rollback records "undo <name>".
func (tr *trace) step(t dcctlv1.Operation_Step_Type, name string, err error) operation.Step {
	return operation.Step{
		Type: t,
		Run: func(context.Context, *operation.Exec) error {
			tr.add("run " + name)

			return err
		},
		Rollback: func(context.Context, *operation.Exec) error {
			tr.add("undo " + name)

			return nil
		},
	}
}

func switchPlan(steps ...operation.Step) operation.Plan {
	return operation.Plan{
		Type:      dcctlv1.Operation_SWITCH,
		Target:    "dc_b",
		Request:   &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"},
		RequestID: "4f8f5c5e-8a0b-4a43-9b66-3f0d7d1d2f6e",
		Initiator: &dcctlv1.Initiator{User: "admin", Address: "10.0.0.9"},
		Preflight: preflight.Request{
			Action: preflight.Switch, Params: preflight.Params{Datacenter: "dc_b"},
			Confirmations: check.Confirmations{MaxDiscardBytes: 50},
		},
		Mode:               dcctlv1.Operation_QUORUM,
		PreviousGeneration: 5,
		NewGeneration:      6,
		Steps:              steps,
	}
}

func stepStates(op *dcctlv1.Operation) []dcctlv1.Operation_Step_State {
	out := make([]dcctlv1.Operation_Step_State, 0, len(op.GetSteps()))
	for _, s := range op.GetSteps() {
		out = append(out, s.GetState())
	}

	return out
}

const (
	pending   = dcctlv1.Operation_Step_PENDING
	succeeded = dcctlv1.Operation_Step_SUCCEEDED
	failed    = dcctlv1.Operation_Step_FAILED
	skipped   = dcctlv1.Operation_Step_SKIPPED
)

func TestStartRunsStepsInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		skip := operation.Step{Type: dcctlv1.Operation_Step_VERIFY, Run: func(_ context.Context, x *operation.Exec) error {
			tr.add("run verify")
			x.Skip("nothing to verify")

			return nil
		}}
		warn := operation.Step{Type: dcctlv1.Operation_Step_APPS_START, Stage: 1, Run: func(_ context.Context, x *operation.Exec) error {
			tr.add("run start")
			x.Warn("app x is not healthy")

			return nil
		}}

		first, err := e.Start(t.Context(), switchPlan(
			tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil), warn, skip,
		))
		require.NoError(t, err)

		assert.Equal(t, dcctlv1.Operation_RUNNING, first.GetState())
		assert.Equal(t, int64(1), first.GetRevision())
		assert.Equal(t, []dcctlv1.Operation_Step_State{pending, pending, pending}, stepStates(first))
		assert.Equal(t, "datacenters/dc_b", first.GetTarget())
		assert.Equal(t, "node-b", first.GetLeader().GetNode())
		assert.NotNil(t, first.GetSwitchDatacenter())

		synctest.Wait()

		assert.Equal(t, []string{"run stop", "run start", "run verify"}, tr.all())

		a, b := w.op("dc_a"), w.op("dc_b")
		require.NotNil(t, a)
		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, a.GetState())
		assert.Equal(t, []dcctlv1.Operation_Step_State{succeeded, succeeded, skipped}, stepStates(a))
		assert.Equal(t, []string{"app x is not healthy"}, a.GetSteps()[1].GetWarnings())
		assert.Equal(t, "nothing to verify", a.GetSteps()[2].GetDetail())
		assert.NotNil(t, a.GetEndTime())
		// One write at the start, two per step and one at the end.
		assert.Equal(t, int64(1+2*3+1), a.GetRevision())
		assert.Equal(t, a.GetRevision(), b.GetRevision())
		assert.True(t, w.lockFree())

		assert.Equal(t, []dcctlv1.Operation_State{dcctlv1.Operation_SUCCEEDED}, w.recorder.operations())
		assert.Equal(t, []string{"start", "end"}, w.journal.events(dcctlv1.JournalEntry_OPERATION))
	})
}

func TestFailureBeforePointOfNoReturnRollsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		promote := tr.step(dcctlv1.Operation_Step_PATRONI_PROMOTE, "promote", nil)
		promote.PointOfNoReturn = true

		_, err := e.Start(t.Context(), switchPlan(
			tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil),
			tr.step(dcctlv1.Operation_Step_PATRONI_DEMOTE, "demote", nil),
			tr.step(dcctlv1.Operation_Step_PATRONI_CATCHUP, "catchup", errors.New("lag does not shrink")),
			promote,
		))
		require.NoError(t, err)

		synctest.Wait()

		assert.Equal(t, []string{"run stop", "run demote", "run catchup", "undo catchup", "undo demote", "undo stop"}, tr.all())

		op := w.op("dc_b")
		assert.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		assert.Equal(t, []dcctlv1.Operation_Step_State{succeeded, succeeded, failed, pending}, stepStates(op))
		assert.Contains(t, op.GetError().GetDetail(), "lag does not shrink")
		assert.Equal(t, "rolled back", op.GetSteps()[0].GetDetail())
		assert.True(t, w.lockFree())
		assert.Equal(t, []dcctlv1.Operation_State{dcctlv1.Operation_FAILED}, w.recorder.operations())
	})
}

func TestFailureAtPointOfNoReturnInterrupts(t *testing.T) {
	tests := []struct {
		name string
		// failAt is the index of the failing step; step 1 is the point of no
		// return.
		failAt int
	}{
		{name: "point_of_no_return", failAt: 1},
		{name: "after_it", failAt: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				e := w.engine(self)
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				var tr trace

				types := []dcctlv1.Operation_Step_Type{
					dcctlv1.Operation_Step_PATRONI_DEMOTE, dcctlv1.Operation_Step_PATRONI_PROMOTE, dcctlv1.Operation_Step_APPS_START,
				}
				steps := make([]operation.Step, 0, len(types))

				for i, typ := range types {
					var err error
					if i == tt.failAt {
						err = errors.New("refused")
					}

					steps = append(steps, tr.step(typ, typ.String(), err))
				}

				steps[1].PointOfNoReturn = true

				_, err := e.Start(t.Context(), switchPlan(steps...))
				require.NoError(t, err)

				synctest.Wait()

				for _, ev := range tr.all() {
					assert.NotContains(t, ev, "undo")
				}

				op := w.op("dc_a")
				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
				assert.Equal(t, failed, op.GetSteps()[tt.failAt].GetState())
				assert.Nil(t, op.GetEndTime())
				assert.True(t, w.lockFree())
				assert.Empty(t, w.recorder.operations())
			})
		})
	}
}

func TestRollbackFailureInterrupts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		stopApps := tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)
		stopApps.Rollback = func(context.Context, *operation.Exec) error { return errors.New("start refused") }

		_, err := e.Start(t.Context(), switchPlan(
			stopApps, tr.step(dcctlv1.Operation_Step_PATRONI_DEMOTE, "demote", errors.New("demote refused")),
		))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op("dc_a")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "rollback failed")
		assert.Contains(t, op.GetSteps()[0].GetDetail(), "rollback failed: start refused")
		assert.True(t, w.lockFree())
	})
}

func TestUnknownOutcomeInterrupts(t *testing.T) {
	tests := []struct {
		name string
		run  operation.StepFunc
	}{
		{
			name: "outcome_unknown",
			run: func(context.Context, *operation.Exec) error {
				return fmt.Errorf("%w: promote request timed out", operation.ErrOutcomeUnknown)
			},
		},
		{
			name: "undetermined_generation",
			run: func(ctx context.Context, x *operation.Exec) error {
				_, err := x.Write(ctx, "advance", func(context.Context) (coord.WriteResult, error) {
					return coord.WriteResult{Commitment: coord.Undetermined}, coord.ErrUndetermined
				})

				return err
			},
		},
		{
			name: "uncommitted_generation",
			run: func(ctx context.Context, x *operation.Exec) error {
				_, err := x.Write(ctx, "advance", func(context.Context) (coord.WriteResult, error) {
					return coord.WriteResult{Commitment: coord.Uncommitted}, coord.ErrUncommitted
				})

				return err
			},
		},
		{
			name: "operator_decides",
			run: func(context.Context, *operation.Exec) error {
				return operation.Interrupt(errors.New("dc_c is primary"))
			},
		},
		{
			name: "patroni_outcome_unknown",
			run: func(context.Context, *operation.Exec) error {
				return fmt.Errorf("promote dc_b: %w", pgctl.ErrOutcomeUnknown)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				e := w.engine(self)
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				var tr trace

				_, err := e.Start(t.Context(), switchPlan(
					tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil),
					operation.Step{Type: dcctlv1.Operation_Step_GENERATION, Run: tt.run},
				))
				require.NoError(t, err)

				synctest.Wait()

				assert.Equal(t, []string{"run stop"}, tr.all())
				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_a").GetState())
				assert.True(t, w.lockFree())
			})
		})
	}
}

func TestInterruptKeepsTheErrorAsDetail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		_, err := e.Start(t.Context(), switchPlan(
			tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil),
			operation.Step{Type: dcctlv1.Operation_Step_PATRONI_NO_PRIMARY, Run: func(context.Context, *operation.Exec) error {
				return operation.Interrupt(errors.New("dc_c is primary"))
			}},
		))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op("dc_a")

		assert.Equal(t, []string{"run stop"}, tr.all(), "nothing is rolled back")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Equal(t, "dc_c is primary", op.GetError().GetDetail())
		assert.Equal(t, "dc_c is primary", op.GetSteps()[1].GetDetail())
	})
}

func TestConfirmationRecheckFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		discard := int64(100)
		demote := tr.step(dcctlv1.Operation_Step_PATRONI_DEMOTE, "demote", nil)
		demote.Recheck = func(context.Context) ([]check.Check, error) {
			return []check.Check{{
				ID: "patroni.divergence", Result: check.Fail, Effect: check.Confirm,
				Confirmation: check.ConfirmMaxDiscardBytes, DiscardBytes: &discard,
			}}, nil
		}

		_, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil), demote))
		require.NoError(t, err)

		synctest.Wait()

		assert.Equal(t, []string{"run stop", "undo stop"}, tr.all())

		op := w.op("dc_a")
		assert.Equal(t, dcctlv1.Operation_FAILED, op.GetState())
		assert.Equal(t, failed, op.GetSteps()[1].GetState())
		assert.Equal(t, "CONFIRMATION_REQUIRED", op.GetError().GetReason())
		assert.Equal(t, "dcctl.confirmation.required", op.GetError().GetId())
		assert.True(t, w.lockFree())
	})
}

func TestLockLostInterrupts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		wait := operation.Step{Type: dcctlv1.Operation_Step_PATRONI_CATCHUP, Run: func(ctx context.Context, x *operation.Exec) error {
			<-ctx.Done()

			return x.Guard(ctx)
		}}

		_, err := e.Start(t.Context(), switchPlan(wait, tr.step(dcctlv1.Operation_Step_PATRONI_PROMOTE, "promote", nil)))
		require.NoError(t, err)

		synctest.Wait()
		w.consuls["dc_a"].expireAll()
		time.Sleep(10 * time.Second)
		synctest.Wait()

		assert.Empty(t, tr.all())

		op := w.op("dc_b")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), "lock lost")
		assert.Len(t, w.journal.system(dcctlv1.JournalEntry_LOCK_LOST), 1)
		assert.True(t, w.lockFree())
	})
}

func TestLockLostKeepsNewerRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var name string

		wait := operation.Step{Type: dcctlv1.Operation_Step_PATRONI_CATCHUP, Run: func(ctx context.Context, x *operation.Exec) error {
			<-ctx.Done()

			// Another instance interrupted the operation and resumed it while
			// this one still finished its step.
			newer := x.Operation()
			newer.Revision, newer.Leader = 99, &dcctlv1.Instance{Node: "node-x", Datacenter: "datacenters/dc_a"}
			w.setOp(newer)

			return x.Guard(ctx)
		}}

		first, err := e.Start(t.Context(), switchPlan(wait))
		require.NoError(t, err)

		name = first.GetName()

		synctest.Wait()
		w.consuls["dc_a"].expireAll()
		time.Sleep(10 * time.Second)
		synctest.Wait()

		for _, dc := range []string{"dc_a", "dc_b"} {
			op := w.op(dc)
			assert.Equal(t, name, op.GetName())
			assert.Equal(t, int64(99), op.GetRevision(), dc)
			assert.Equal(t, dcctlv1.Operation_RUNNING, op.GetState(), dc)
		}

		// The instance that wrote the newer change journals the operation and
		// the lost lock.
		assert.Empty(t, w.journal.system(dcctlv1.JournalEntry_LOCK_LOST))
		assert.Equal(t, []string{"start"}, w.journal.events(dcctlv1.JournalEntry_OPERATION))
	})
}

func TestStopInterrupts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		synctest.Wait()

		wait := operation.Step{Type: dcctlv1.Operation_Step_PATRONI_CATCHUP, Run: func(ctx context.Context, _ *operation.Exec) error {
			<-ctx.Done()

			return ctx.Err()
		}}

		_, err := e.Start(t.Context(), switchPlan(wait))
		require.NoError(t, err)

		synctest.Wait()
		stop()

		op := w.op("dc_a")
		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState())
		assert.Contains(t, op.GetError().GetDetail(), operation.ErrStopping.Error())
		assert.True(t, w.lockFree())

		_, err = e.Start(t.Context(), switchPlan(wait))
		require.ErrorIs(t, err, operation.ErrNotRunning)
	})
}

func TestStartRefusedByPreflight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.preflight.verdict = check.Verdict{
			Outcome: check.Blocked,
			Checks:  []check.Check{{ID: "patroni.target_healthy", Result: check.Fail, Effect: check.Block, Datacenter: "dc_b"}},
			Failed:  []check.Check{{ID: "patroni.target_healthy", Result: check.Fail, Effect: check.Block, Datacenter: "dc_b"}},
		}
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		_, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)))
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		synctest.Wait()

		assert.Empty(t, tr.all())
		assert.Nil(t, w.op("dc_a"))
		assert.Equal(t, 0, w.consuls["dc_a"].nextID, "no lock session is created")
	})
}

func TestStartRefusedWhileOperationRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		other := interruptedOp("other", 3)
		w.consuls["dc_a"].setOp(t, other)

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		_, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)))

		var running *coord.OperationRunningError
		require.ErrorAs(t, err, &running)
		assert.Equal(t, "operations/other", running.Operation.GetName())

		synctest.Wait()

		assert.Empty(t, tr.all())
		assert.True(t, w.lockFree())
		assert.Nil(t, w.op("dc_b"))
	})
}

// requestOp is a finished switch started by the request of switchPlan, with
// its request_id.
func requestOp(req *dcctlv1.SwitchDatacenterRequest) *dcctlv1.Operation {
	op := interruptedOp("earlier", 4)
	op.State = dcctlv1.Operation_SUCCEEDED
	op.RequestId = switchPlan().RequestID
	op.Request = &dcctlv1.Operation_SwitchDatacenter{SwitchDatacenter: req}

	return op
}

func TestStartReplaysRequestID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		earlier := requestOp(&dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
		w.setOp(earlier)

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		op, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)))
		require.NoError(t, err)
		assert.Equal(t, "operations/earlier", op.GetName())
		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState())

		synctest.Wait()

		assert.Empty(t, tr.all(), "nothing runs again")
		assert.True(t, w.lockFree())
		assert.Equal(t, "operations/earlier", w.op("dc_a").GetName())
	})
}

func TestStartRequestIDOfAnotherRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(requestOp(&dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_c"}))

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		_, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)))
		assert.Equal(t, codes.AlreadyExists, status.Code(err), "error = %v", err)
		assert.Equal(t, "REQUEST_ID_MISMATCH", reasonOf(err))

		synctest.Wait()

		assert.Empty(t, tr.all())
		assert.True(t, w.lockFree())
		assert.Equal(t, "operations/earlier", w.op("dc_a").GetName())
	})
}

func TestStartNotRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		for _, c := range w.consuls {
			c.set(func(f *fakeConsul) {
				f.casErr = func(key string) error {
					if key == kv.OpKey {
						return unreachable()
					}

					return nil
				}
			})
		}

		var tr trace

		_, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil)))

		var notRecorded *operation.NotRecordedError
		require.ErrorAs(t, err, &notRecorded)

		st := statusOf(err)
		assert.Equal(t, codes.Unavailable, st.Code())
		assert.Equal(t, "KV_UNAVAILABLE", reasonOf(err))
		assert.Equal(t, notRecorded.Name+" is not started: no copy of dcctl/op took it", st.Message(),
			"the errors of the writes stay out of the answer")

		for _, entry := range w.journal.entries {
			assert.NotContains(t, entry.GetError().GetDetail(), "fake:8500", "nor in the journal")
		}

		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				assert.Equal(t, "dc_a,dc_b", info.GetMetadata()["datacenters"])
				assert.Equal(t, notRecorded.Name, info.GetMetadata()["operation"])
			}
		}

		synctest.Wait()

		assert.Empty(t, tr.all())
		assert.True(t, w.lockFree())
	})
}

func TestNotRunningIsUnavailable(t *testing.T) {
	w := newWorld(t)
	e := w.engine(self)

	_, err := e.Start(t.Context(), switchPlan(operation.Step{
		Type: dcctlv1.Operation_Step_VERIFY, Run: func(context.Context, *operation.Exec) error { return nil },
	}))
	require.ErrorIs(t, err, operation.ErrNotRunning)
	assert.Equal(t, codes.Unavailable, statusOf(err).Code())
}

func TestUnrecordedChangeNotPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		sub := e.Subscribe("")
		defer sub.Close()

		cut := operation.Step{Type: dcctlv1.Operation_Step_APPS_STOP, Run: func(context.Context, *operation.Exec) error {
			for _, c := range w.consuls {
				c.set(func(f *fakeConsul) { f.casErr = func(string) error { return unreachable() } })
			}

			return nil
		}}

		_, err := e.Start(t.Context(), switchPlan(cut))
		require.NoError(t, err)

		synctest.Wait()

		var revisions []int64

		for len(sub.C()) > 0 {
			revisions = append(revisions, (<-sub.C()).GetRevision())
		}

		assert.Equal(t, []int64{1, 2}, revisions, "only the changes a copy took")
		assert.Equal(t, int64(2), w.op("dc_a").GetRevision())
		assert.True(t, w.lockFree())
	})
}

func TestStartRefusedWhenStateChanged(t *testing.T) {
	tests := []struct {
		name     string
		previous int64
		active   string
	}{
		{name: "generation", previous: 4, active: "dc_a"},
		{name: "active_datacenter", previous: 5, active: "dc_b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				e := w.engine(self)
				stop := start(t, e)

				defer stop()

				synctest.Wait()

				var tr trace

				// The voters hold generation 5 of dc_a.
				p := switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil))
				p.PreviousGeneration, p.PreviousActive = tt.previous, tt.active

				_, err := e.Start(t.Context(), p)

				require.ErrorIs(t, err, operation.ErrStateChanged)
				assert.Equal(t, codes.FailedPrecondition, statusOf(err).Code())
				assert.Equal(t, "PREFLIGHT_FAILED", reasonOf(err))
				assert.True(t, slices.ContainsFunc(statusOf(err).Details(), func(d any) bool {
					p, ok := d.(*dcctlv1.Preflight)

					return ok && len(p.GetChecks()) == 1 && p.GetChecks()[0].GetId() == "coord.base_unchanged"
				}), "the refusal has the Preflight detail")

				synctest.Wait()

				assert.Empty(t, tr.all())
				assert.Nil(t, w.op("dc_a"))
				assert.True(t, w.lockFree())
			})
		})
	}
}

// The preflight plans a failover from an undetermined record of the same
// generation as a forced effective one; the engine starts from the same record.
func TestFailoverStartsFromUndeterminedOfForcedGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.voters[0] = newVoter(t, "dc_a", coord.KindKV, kv.State{Generation: 6, ActiveDatacenter: "dc_a", OpID: "forced", Forced: true})
		w.voters[1].setUnreachable()
		w.voters[2] = newVoter(t, coord.ArbiterName, coord.KindArbiter, kv.State{Generation: 6, ActiveDatacenter: "dc_b", OpID: "sw"})

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var tr trace

		p := switchPlan(tr.step(dcctlv1.Operation_Step_PATRONI_PROMOTE, "promote", nil))
		p.Type, p.Request, p.Target = dcctlv1.Operation_FAILOVER, &dcctlv1.FailoverDatacenterRequest{Name: "datacenters/dc_a"}, "dc_a"
		p.Mode = dcctlv1.Operation_FORCED
		p.PreviousGeneration, p.PreviousActive, p.NewGeneration = 6, "dc_b", 7

		_, err := e.Start(t.Context(), p)
		require.NoError(t, err)

		synctest.Wait()

		assert.Equal(t, []string{"run promote"}, tr.all())
		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op("dc_a").GetState())
	})
}

// The start journals what the target and the other standbys received even when the
// check passed: the loss the failover accepts.
func TestStartJournalsLossEstimate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		facts := map[string]any{"target": map[string]any{"datacenter": "dc_b", "received_location": float64(1000)}}
		w.preflight.verdict = check.Verdict{Outcome: check.Allowed, Checks: []check.Check{{
			ID: check.PatroniTargetMaxLSN, Result: check.Pass, Effect: check.Confirm, Confirmation: check.ConfirmNotMaxLSN, Facts: facts,
		}}}

		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		_, err := e.Start(t.Context(), switchPlan(operation.Step{
			Type: dcctlv1.Operation_Step_VERIFY, Run: func(context.Context, *operation.Exec) error { return nil },
		}))
		require.NoError(t, err)

		synctest.Wait()

		started := w.journal.event("start")
		require.Len(t, started, 1)
		assert.Equal(t, facts, started[0].GetParams().AsMap()["loss_estimate"])
		assert.NotContains(t, started[0].GetParams().AsMap(), "confirmations", "a check that passed needs no confirmation")
	})
}

// A step records the mode it writes the generation in: the copies hold it at a new
// revision.
func TestExecSetMode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		var before int64

		_, err := e.Start(t.Context(), switchPlan(operation.Step{
			Type: dcctlv1.Operation_Step_GENERATION, Run: func(ctx context.Context, x *operation.Exec) error {
				before = x.Operation().GetRevision()

				if err := x.SetMode(ctx, dcctlv1.Operation_QUORUM); err != nil {
					return err
				}

				require.Equal(t, before, x.Operation().GetRevision(), "the same mode saves nothing")

				return x.SetMode(ctx, dcctlv1.Operation_FORCED)
			},
		}))
		require.NoError(t, err)

		synctest.Wait()

		op := w.op("dc_b")
		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, op.GetState())
		assert.Equal(t, dcctlv1.Operation_FORCED, op.GetMode())
		assert.Greater(t, op.GetRevision(), before+1)
	})
}
