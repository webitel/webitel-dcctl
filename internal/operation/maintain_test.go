package operation_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestRunAlignsCopiesAndVoters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		done := interruptedOp("op1", 7)
		done.State = dcctlv1.Operation_SUCCEEDED
		w.consuls["dc_a"].setOp(t, done)
		w.consuls["dc_b"].setOp(t, runningOp("op1", 4))
		w.voters[2] = newVoter(t, coord.ArbiterName, coord.KindArbiter, kv.State{Generation: 4, ActiveDatacenter: "dc_b", OpID: "older"})

		stop := start(t, w.engine(self))

		defer stop()

		synctest.Wait()

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op("dc_b").GetState())
		assert.Equal(t, int64(7), w.op("dc_b").GetRevision())
		assert.Len(t, w.journal.system(dcctlv1.JournalEntry_ALIGN_OPERATION), 1)

		assert.Equal(t, int64(5), w.voters[2].state(t).Generation)
		assert.Len(t, w.journal.system(dcctlv1.JournalEntry_ALIGN_GENERATION), 1)
	})
}
