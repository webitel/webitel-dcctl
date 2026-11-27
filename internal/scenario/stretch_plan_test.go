package scenario_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
)

const (
	stepStop       = dcctlv1.Operation_Step_APPS_STOP
	stepStart      = dcctlv1.Operation_Step_APPS_START
	stepGeneration = dcctlv1.Operation_Step_GENERATION
	stepVerify     = dcctlv1.Operation_Step_VERIFY
)

func TestStretchSwitchPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)

		p := w.plan()

		assert.Equal(t, dcctlv1.Operation_SWITCH, p.Type)
		assert.Equal(t, dcB, p.Target)
		assert.Equal(t, dcctlv1.Operation_QUORUM, p.Mode)
		assert.Equal(t, int64(5), p.PreviousGeneration)
		assert.Equal(t, int64(6), p.NewGeneration)
		assert.Equal(t, dcA, p.PreviousActive)
		assert.Equal(t, preflight.Request{Action: preflight.Switch, Params: preflight.Params{Datacenter: dcB}}, p.Preflight)

		assert.Equal(t, []planStep{
			{Type: stepStop, Stage: 3, Datacenter: dcA, Rollback: true},
			{Type: stepStop, Stage: 2, Datacenter: dcA, Rollback: true},
			{Type: stepStop, Stage: 1, Datacenter: dcA, Rollback: true},
			{Type: stepGeneration, Datacenter: dcB, Rollback: true},
			{Type: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, Datacenter: dcB, PNR: true},
			{Type: stepStart, Stage: 1, Datacenter: dcB},
			{Type: stepStart, Stage: 2, Datacenter: dcB},
			{Type: stepStart, Stage: 3, Datacenter: dcB},
			{Type: dcctlv1.Operation_Step_PATRONI_FOLLOW, Datacenter: dcA},
			{Type: stepVerify},
		}, planSteps(p.Steps))
	})
}

func TestStretchFailoverPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.infra.pg(loseLeader)

		p := w.failoverPlan(isolation)

		assert.Equal(t, dcctlv1.Operation_FAILOVER, p.Type)
		assert.Equal(t, dcctlv1.Operation_QUORUM, p.Mode)
		assert.Equal(t, int64(6), p.NewGeneration)

		assert.Equal(t, []planStep{
			{Type: stepGeneration, Datacenter: dcB, Rollback: true},
			{Type: stepStop, Stage: 3, Datacenter: dcA},
			{Type: stepStop, Stage: 2, Datacenter: dcA},
			{Type: stepStop, Stage: 1, Datacenter: dcA},
			{Type: dcctlv1.Operation_Step_PATRONI_FAILOVER, Datacenter: dcB, PNR: true},
			{Type: stepStart, Stage: 1, Datacenter: dcB},
			{Type: stepStart, Stage: 2, Datacenter: dcB},
			{Type: stepStart, Stage: 3, Datacenter: dcB},
			{Type: stepVerify},
		}, planSteps(p.Steps))
	})
}

// Each profile plans only its own scenarios.
func TestPlan_profiles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		stretch := w.status()

		warm := stretch
		warm.Profile = config.ProfileWarmStandby

		_, err := w.ssw.Plan(switchTo(dcB), initiator, preflight.Result{Status: warm})
		require.ErrorIs(t, err, scenario.ErrCannotPlan)

		_, err = w.sfo.Plan(failoverTo(dcB, isolation), initiator, preflight.Result{Status: warm})
		require.ErrorIs(t, err, scenario.ErrCannotPlan)

		_, err = w.sw.Plan(switchTo(dcB), initiator, preflight.Result{Status: stretch})
		require.ErrorIs(t, err, scenario.ErrCannotPlan)

		_, err = w.fo.Plan(failoverTo(dcB, isolation), initiator, preflight.Result{Status: stretch})
		require.ErrorIs(t, err, scenario.ErrCannotPlan)
	})
}

// max_discard_bytes the preflight does not ask for, as the candidate does not lag, is
// refused, not ignored.
func TestStretchFailoverPlan_unaskedDiscard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		w.infra.pg(loseLeader)

		_, err := w.sfo.Plan(failoverTo(dcB, &dcctlv1.Confirmation{Isolation: true, MaxDiscardBytes: 500}), initiator,
			preflight.Result{Status: w.status()})
		require.ErrorIs(t, err, scenario.ErrCannotPlan)
	})
}

func TestStretchResume_rebuildsSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)

		p := w.plan()
		steps, err := w.ssw.Resume(t.Context(), recorded(p))
		require.NoError(t, err)
		assert.Equal(t, planSteps(p.Steps), planSteps(steps))

		w.infra.pg(loseLeader)

		p = w.failoverPlan(isolation)
		steps, err = w.sfo.Resume(t.Context(), recorded(p))
		require.NoError(t, err)
		assert.Equal(t, planSteps(p.Steps), planSteps(steps))
	})
}

// A scenario of one profile does not resume an operation of the other, nor one with a
// step of the other type of action.
func TestStretchResume_refuses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newStretchWorld(t)
		stretchSwitch := recorded(w.plan())

		w.infra.pg(loseLeader)
		stretchFailover := recorded(w.failoverPlan(isolation))

		warm := newWorld(t)
		warmSwitch := recorded(warm.plan())
		warmFailover := recorded(warm.failoverPlan(isolation))

		withSwitchover := recorded(w.failoverPlan(isolation))
		withSwitchover.Steps[4].Type = dcctlv1.Operation_Step_PATRONI_SWITCHOVER

		tests := []struct {
			name string
			r    operation.Resumer
			op   *dcctlv1.Operation
		}{
			{name: "stretch_switch_of_warm", r: w.ssw, op: warmSwitch},
			{name: "stretch_failover_of_warm", r: w.sfo, op: warmFailover},
			{name: "warm_switch_of_stretch", r: w.sw, op: stretchSwitch},
			{name: "warm_failover_of_stretch", r: w.fo, op: stretchFailover},
			{name: "failover_with_switchover", r: w.sfo, op: withSwitchover},
			{name: "stretch_switch_of_failover", r: w.ssw, op: stretchFailover},
			{name: "stretch_failover_of_switch", r: w.sfo, op: stretchSwitch},
		}

		for _, tt := range tests {
			_, err := tt.r.Resume(t.Context(), tt.op)
			assert.Error(t, err, tt.name)
		}
	})
}
