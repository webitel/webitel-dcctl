package operation_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/operation"
)

func TestSubscriberGetsEveryChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		sub := e.Subscribe("")
		defer sub.Close()

		var tr trace

		progress := operation.Step{Type: dcctlv1.Operation_Step_PATRONI_CATCHUP, Run: func(_ context.Context, x *operation.Exec) error {
			for _, d := range []string{"lag 300", "lag 100"} {
				x.Progress(d)
				time.Sleep(operation.DefaultProgressInterval + time.Second)
			}

			return nil
		}}

		_, err := e.Start(t.Context(), switchPlan(tr.step(dcctlv1.Operation_Step_APPS_STOP, "stop", nil), progress))
		require.NoError(t, err)

		var got []*dcctlv1.Operation

		for op := range sub.C() {
			got = append(got, op)
			if op.GetState() == dcctlv1.Operation_SUCCEEDED {
				break
			}
		}

		for i, op := range got {
			assert.Equal(t, int64(i+1), op.GetRevision(), "every revision, in order")
		}

		type change struct {
			step   int
			state  dcctlv1.Operation_Step_State
			detail string
		}

		var changes []change

		for i := 1; i < len(got); i++ {
			for j, st := range got[i].GetSteps() {
				prev := got[i-1].GetSteps()[j]
				if st.GetState() != prev.GetState() || st.GetDetail() != prev.GetDetail() {
					changes = append(changes, change{j, st.GetState(), st.GetDetail()})
				}
			}
		}

		assert.Equal(t, []change{
			{0, dcctlv1.Operation_Step_RUNNING, ""},
			{0, succeeded, ""},
			{1, dcctlv1.Operation_Step_RUNNING, ""},
			{1, dcctlv1.Operation_Step_RUNNING, "lag 300"},
			{1, dcctlv1.Operation_Step_RUNNING, "lag 100"},
			{1, succeeded, "lag 100"},
		}, changes)

		for i := 1; i < len(got); i++ {
			assert.True(t, got[i].GetUpdateTime().AsTime().After(got[i-1].GetUpdateTime().AsTime()) ||
				got[i].GetUpdateTime().AsTime().Equal(got[i-1].GetUpdateTime().AsTime()))
		}
	})
}

func TestSlowSubscriberDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.engine(self)
		stop := start(t, e)

		defer stop()

		synctest.Wait()

		sub := e.Subscribe("")
		defer sub.Close()

		var tr trace

		steps := make([]operation.Step, 0, 40)
		for range 40 {
			steps = append(steps, tr.step(dcctlv1.Operation_Step_VERIFY, "verify", nil))
		}

		_, err := e.Start(t.Context(), switchPlan(steps...))
		require.NoError(t, err)

		synctest.Wait()

		assert.Equal(t, dcctlv1.Operation_SUCCEEDED, w.op("dc_a").GetState(), "the engine does not wait for a subscriber")

		n := 0
		for range sub.C() {
			n++
		}

		assert.Equal(t, 64, n, "the channel is closed after what it holds")
	})
}
