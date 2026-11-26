package operation_test

import (
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestOrphanedOperationInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(runningOp("op1", 4))

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		for _, dc := range []string{"dc_a", "dc_b"} {
			op := w.op(dc)
			assert.Equal(t, dcctlv1.Operation_INTERRUPTED, op.GetState(), dc)
			assert.Equal(t, int64(5), op.GetRevision(), dc)
			assert.Equal(t, "node-x", op.GetLeader().GetNode(), "the leader stays as it was")
			assert.Contains(t, op.GetError().GetDetail(), "no live session holds dcctl/lock")
		}

		lost := w.journal.system(dcctlv1.JournalEntry_LOCK_LOST)
		if assert.Len(t, lost, 1) {
			assert.Equal(t, "operations/op1", lost[0].GetOperation())
		}

		assert.True(t, w.lockFree())
	})
}

func TestRunningOperationWithLiveLockKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.setOp(runningOp("op1", 4))
		w.consuls["dc_a"].holdLock(t, kv.Lock{Instance: "node-x", Operation: "op1"})

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		assert.Equal(t, dcctlv1.Operation_RUNNING, w.op("dc_a").GetState())
		assert.Equal(t, int64(4), w.op("dc_b").GetRevision())
		assert.Empty(t, w.journal.system(dcctlv1.JournalEntry_LOCK_LOST))
	})
}

func TestRestartLeavesOperationOfNewLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		mine := runningOp("op1", 4)
		mine.Leader = selfLeader()
		w.setOp(mine)

		// node-x takes the operation over between the two reads of the sweep.
		w.consuls["dc_a"].holdLock(t, kv.Lock{Instance: "node-x", Operation: "op1"})
		w.consuls["dc_b"].holdLock(t, kv.Lock{Instance: "node-x", Operation: "op1"})

		var reads atomic.Int32

		hook := func(key string) error {
			if key == kv.OpKey && reads.Add(1) == 3 {
				resumed := runningOp("op1", 6)
				resumed.Leader = &dcctlv1.Instance{Node: "node-x", Datacenter: "datacenters/dc_a"}
				w.setOp(resumed)
			}

			return nil
		}

		for _, c := range w.consuls {
			c.set(func(f *fakeConsul) { f.onGet = hook })
		}

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		for _, dc := range []string{"dc_a", "dc_b"} {
			op := w.op(dc)
			assert.Equal(t, dcctlv1.Operation_RUNNING, op.GetState(), dc)
			assert.Equal(t, int64(6), op.GetRevision(), dc)
			assert.Equal(t, "node-x", op.GetLeader().GetNode(), dc)
		}
	})
}

func TestOrphanJournalsUnreachableKVs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		w.consuls["dc_a"].setOp(t, runningOp("op1", 4))
		w.consuls["dc_b"].set(func(f *fakeConsul) { f.down = true })

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		assert.Equal(t, dcctlv1.Operation_INTERRUPTED, w.op("dc_a").GetState())

		lost := w.journal.system(dcctlv1.JournalEntry_LOCK_LOST)
		require.Len(t, lost, 1)
		assert.Equal(t, []any{"dc_b"}, lost[0].GetParams().AsMap()["unreachable"])
		assert.Equal(t, []any{"dc_a"}, lost[0].GetParams().AsMap()["held"])
	})
}

func TestSweepJudgesByHighestRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		// The copy in the leader's datacenter missed the interruption.
		w.consuls["dc_a"].setOp(t, runningOp("op1", 4))
		w.consuls["dc_b"].setOp(t, interruptedOp("op1", 5))

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		assert.Equal(t, int64(4), w.op("dc_a").GetRevision())
		assert.Equal(t, int64(5), w.op("dc_b").GetRevision())
		assert.Empty(t, w.journal.system(dcctlv1.JournalEntry_LOCK_LOST))
		assert.Equal(t, 0, w.consuls["dc_a"].nextID, "the lock is not taken for an interrupted operation")
	})
}

func TestSweepReconcilesCopiesOfOneRevision(t *testing.T) {
	for _, runningIn := range []string{"dc_a", "dc_b"} {
		t.Run("running_in_"+runningIn, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)

				// A late write of the old leader and the interruption by
				// another instance reached different copies at revision 5.
				other := map[string]string{"dc_a": "dc_b", "dc_b": "dc_a"}[runningIn]
				w.consuls[runningIn].setOp(t, runningOp("op1", 5))
				w.consuls[other].setOp(t, interruptedOp("op1", 5))

				stop := start(t, w.engine(self))

				defer stop()

				synctest.Wait()

				a, b := w.op("dc_a"), w.op("dc_b")
				assert.Equal(t, dcctlv1.Operation_INTERRUPTED, a.GetState())
				assert.Equal(t, int64(6), a.GetRevision())
				assert.True(t, proto.Equal(a, b), "both copies hold the same change")
				assert.Len(t, w.journal.system(dcctlv1.JournalEntry_ALIGN_OPERATION), 1)
				assert.Empty(t, w.journal.system(dcctlv1.JournalEntry_LOCK_LOST))
				assert.True(t, w.lockFree())
			})
		})
	}
}

func TestSweepLeavesFinishedCopyToAlign(t *testing.T) {
	for _, finishedIn := range []string{"dc_a", "dc_b"} {
		t.Run("finished_in_"+finishedIn, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)

				// The final write of the old leader and the interruption by
				// another instance reached different copies at revision 5.
				done := interruptedOp("op1", 5)
				done.State = dcctlv1.Operation_SUCCEEDED

				other := map[string]string{"dc_a": "dc_b", "dc_b": "dc_a"}[finishedIn]
				w.consuls[finishedIn].setOp(t, done)
				w.consuls[other].setOp(t, interruptedOp("op1", 5))

				stop := start(t, w.engine(self))

				defer stop()

				synctest.Wait()

				for _, dc := range []string{"dc_a", "dc_b"} {
					assert.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op(dc).GetState(), dc)
					assert.Equal(t, int64(5), w.op(dc).GetRevision(), dc)
				}

				assert.Len(t, w.journal.system(dcctlv1.JournalEntry_ALIGN_OPERATION), 1)
				assert.True(t, w.lockFree())
			})
		})
	}
}

func TestSweepLeavesCopiesOfLiveLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		progress := runningOp("op1", 5)
		progress.Steps[1].Detail = "lag 100"

		w.consuls["dc_a"].setOp(t, runningOp("op1", 5))
		w.consuls["dc_b"].setOp(t, progress)
		w.consuls["dc_a"].holdLock(t, kv.Lock{Instance: "node-x", Operation: "op1"})

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		assert.Equal(t, int64(5), w.op("dc_a").GetRevision())
		assert.Equal(t, int64(5), w.op("dc_b").GetRevision())
	})
}
