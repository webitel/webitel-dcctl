package scenario_test

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// planStep is what a plan tells of a step.
type planStep struct {
	Type       dcctlv1.Operation_Step_Type
	Stage      int32
	Datacenter string
	PNR        bool
	Rollback   bool
}

func planSteps(steps []operation.Step) []planStep {
	out := make([]planStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, planStep{
			Type: s.Type, Stage: s.Stage, Datacenter: s.Datacenter, PNR: s.PointOfNoReturn, Rollback: s.Rollback != nil,
		})
	}

	return out
}

func TestSwitchPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		p := w.plan()

		assert.Equal(t, dcctlv1.Operation_SWITCH, p.Type)
		assert.Equal(t, dcB, p.Target)
		assert.Equal(t, "6f1c3a52-7d1e-4b8a-9a43-0d3f5b2c1e77", p.RequestID)
		assert.Equal(t, initiator, p.Initiator)
		assert.Equal(t, dcctlv1.Operation_QUORUM, p.Mode)
		assert.Equal(t, int64(5), p.PreviousGeneration)
		assert.Equal(t, int64(6), p.NewGeneration)
		assert.Equal(t, dcA, p.PreviousActive)
		assert.Equal(t, preflight.Request{Action: preflight.Switch, Params: preflight.Params{Datacenter: dcB}}, p.Preflight)

		const (
			stop    = dcctlv1.Operation_Step_APPS_STOP
			start   = dcctlv1.Operation_Step_APPS_START
			demote  = dcctlv1.Operation_Step_PATRONI_DEMOTE
			catchup = dcctlv1.Operation_Step_PATRONI_CATCHUP
			none    = dcctlv1.Operation_Step_PATRONI_NO_PRIMARY
			gen     = dcctlv1.Operation_Step_GENERATION
			promote = dcctlv1.Operation_Step_PATRONI_PROMOTE
			repoint = dcctlv1.Operation_Step_PATRONI_REPOINT
			follow  = dcctlv1.Operation_Step_PATRONI_FOLLOW
			verify  = dcctlv1.Operation_Step_VERIFY
		)

		assert.Equal(t, []planStep{
			{Type: stop, Stage: 3, Datacenter: dcA, Rollback: true},
			{Type: stop, Stage: 2, Datacenter: dcA, Rollback: true},
			{Type: stop, Stage: 1, Datacenter: dcA, Rollback: true},
			{Type: demote, Datacenter: dcA, Rollback: true},
			{Type: catchup, Datacenter: dcB},
			{Type: none},
			{Type: gen, Datacenter: dcB, Rollback: true},
			{Type: promote, Datacenter: dcB, PNR: true},
			{Type: start, Stage: 1, Datacenter: dcB},
			{Type: start, Stage: 2, Datacenter: dcB},
			{Type: start, Stage: 3, Datacenter: dcB},
			{Type: repoint, Datacenter: dcC},
			{Type: follow, Datacenter: dcA},
			{Type: verify},
		}, planSteps(p.Steps))

		for _, s := range p.Steps {
			assert.Nil(t, s.Recheck, "a switch takes no confirmation: %s", s.Type)
		}
	})
}

func TestSwitchPlan_stagesAndDatacenters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)

		st := w.status()
		st.Datacenters = []status.Datacenter{
			{Name: dcA, Nomad: status.Nomad{Stages: []status.Stage{{Stage: 0}, {Stage: 2}, {Stage: 5}}}},
			{Name: dcB},
		}

		p, err := w.sw.Plan(switchTo(dcB), initiator, preflight.Result{Status: st})
		require.NoError(t, err)

		var got []planStep

		for _, s := range planSteps(p.Steps) {
			if s.Type == dcctlv1.Operation_Step_APPS_STOP || s.Type == dcctlv1.Operation_Step_APPS_START ||
				s.Type == dcctlv1.Operation_Step_PATRONI_REPOINT {
				got = append(got, planStep{Type: s.Type, Stage: s.Stage, Datacenter: s.Datacenter})
			}
		}

		assert.Equal(t, []planStep{
			{Type: dcctlv1.Operation_Step_APPS_STOP, Stage: 5, Datacenter: dcA},
			{Type: dcctlv1.Operation_Step_APPS_STOP, Stage: 2, Datacenter: dcA},
			{Type: dcctlv1.Operation_Step_APPS_START, Stage: 1, Datacenter: dcB},
		}, got, "the invalid stage is left out and a datacenter without stages has one; two datacenters repoint none")
	})
}

func TestSwitchPlan_cannotPlan(t *testing.T) {
	tests := []struct {
		name   string
		target string
		change func(st *status.Status)
	}{
		{name: "stretch", target: dcB, change: func(st *status.Status) { st.Profile = config.ProfileStretch }},
		{name: "active_unknown", target: dcB, change: func(st *status.Status) { st.View = coord.View{} }},
		{name: "target_active", target: dcA, change: func(*status.Status) {}},
		{name: "target_unknown", target: "dc_x", change: func(*status.Status) {}},
		{name: "datacenter_without_deps", target: dcB, change: func(st *status.Status) {
			st.Datacenters = append(st.Datacenters, status.Datacenter{Name: "dc_d"})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				st := w.status()
				tt.change(&st)

				_, err := w.sw.Plan(switchTo(tt.target), initiator, preflight.Result{Status: st})
				require.ErrorIs(t, err, scenario.ErrCannotPlan)
			})
		})
	}
}

// recorded returns op with the steps of p as the engine records them.
func recorded(p operation.Plan) *dcctlv1.Operation {
	op := &dcctlv1.Operation{Name: "operations/op1", Type: p.Type, Target: "datacenters/" + p.Target}

	for i, s := range p.Steps {
		dc := ""
		if s.Datacenter != "" {
			dc = "datacenters/" + s.Datacenter
		}

		op.Steps = append(op.Steps, &dcctlv1.Operation_Step{
			Index: int32(i), Type: s.Type, Stage: s.Stage, Datacenter: dc, PointOfNoReturn: s.PointOfNoReturn, //nolint:gosec // a plan has a handful of steps
		})
	}

	return op
}

func TestSwitchResume_rebuildsSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		p := w.plan()

		steps, err := w.sw.Resume(t.Context(), recorded(p))
		require.NoError(t, err)
		assert.Equal(t, planSteps(p.Steps), planSteps(steps))
	})
}

func TestSwitchResume_refuses(t *testing.T) {
	tests := []struct {
		name   string
		change func(op *dcctlv1.Operation)
	}{
		{name: "not_a_switch", change: func(op *dcctlv1.Operation) { op.Type = dcctlv1.Operation_FAILOVER }},
		{name: "foreign_step", change: func(op *dcctlv1.Operation) {
			op.Steps[4].Type = dcctlv1.Operation_Step_PATRONI_SWITCHOVER
		}},
		{name: "two_sources", change: func(op *dcctlv1.Operation) { op.Steps[3].Datacenter = "datacenters/" + dcC }},
		{name: "no_target", change: func(op *dcctlv1.Operation) { op.Target = "" }},
		{name: "unknown_datacenter", change: func(op *dcctlv1.Operation) { op.Steps[11].Datacenter = "datacenters/dc_x" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				op := recorded(w.plan())
				tt.change(op)

				_, err := w.sw.Resume(t.Context(), op)
				require.Error(t, err)
			})
		})
	}
}
