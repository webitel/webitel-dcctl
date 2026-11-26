package apps_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/apps"
)

func TestStopFrom_leavesLowerStages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake()
		f.addJob("opensips", "service", 3, true)
		f.addJob("engine", "service", 2, true)
		f.addJob("freeswitch", "system", 1, true)
		f.addInvalidJob("legacy", true)

		res, err := newApps(t).StopFrom(t.Context(), f.dc(), 2, apps.Options{Guard: allow})
		require.NoError(t, err)

		assert.Equal(t, []int{3, 2}, stageNumbers(res))
		assert.True(t, f.isStopped("opensips"))
		assert.True(t, f.isStopped("engine"))
		assert.True(t, f.isStopped("legacy"), "a job with an invalid stage stops with the first stage")
		assert.False(t, f.isStopped("freeswitch"))
		assert.Empty(t, f.callsOf("stop", "freeswitch"))
	})
}

func TestStartUpTo_leavesHigherStages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()

		res, err := newApps(t).StartUpTo(t.Context(), f.dc(), 2, apps.Options{Guard: allow})
		require.NoError(t, err)

		assert.Equal(t, []int{1, 2}, stageNumbers(res))
		assert.False(t, f.isStopped("freeswitch"))
		assert.False(t, f.isStopped("engine"))
		assert.True(t, f.isStopped("opensips"))
		assert.Empty(t, f.callsOf("start", "opensips"))
	})
}

func TestStartUpTo_refusesInvalidStageOfAnyStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := stoppedDC()
		f.addInvalidJob("legacy", false)

		_, err := newApps(t).StartUpTo(t.Context(), f.dc(), 1, apps.Options{Guard: allow})
		require.Error(t, err)
		assert.Empty(t, f.callsOf("start", "freeswitch"))
	})
}
