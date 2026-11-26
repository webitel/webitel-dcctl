package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
)

// Primitives the switch is built with; planning calls none of them.
type (
	nopApps        struct{ scenario.Apps }
	nopPatroni     struct{ scenario.Patroni }
	nopCoordinator struct{ scenario.Coordinator }
	nopVerifier    struct{ scenario.Verifier }
	nopNomad       struct{ apps.Nomad }
	nopCluster     struct{ pgctl.Patroni }
)

// switchEnv is node-a of dc_a with the switch of warm_standby, on a state
// whose active datacenter is active.
func switchEnv(t *testing.T, active string) *actionEnv {
	t.Helper()

	sw, err := scenario.NewSwitch(scenario.Deps{
		Apps: nopApps{}, Patroni: nopPatroni{}, Coordinator: nopCoordinator{}, Verifier: nopVerifier{},
		Datacenters: map[string]scenario.Datacenter{
			"dc_a": {Nomad: nopNomad{}, Patroni: nopCluster{}},
			"dc_b": {Nomad: nopNomad{}, Patroni: nopCluster{}},
		},
	})
	require.NoError(t, err)

	env := newActionEnv()
	env.planners = Planners{Switch: sw}

	st := exampleStatus()
	st.View = coord.View{
		Majority: 2,
		Effective: &coord.Record{
			State: kv.State{Generation: 7, ActiveDatacenter: active}, Voters: []string{"dc_a", "dc_b"},
		},
	}
	env.preflight.status = st

	return env
}

func TestSwitchThroughAPI(t *testing.T) {
	env := switchEnv(t, "dc_b")
	dcs, _ := env.servers(t)

	dry, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", ValidateOnly: true})
	require.NoError(t, err)
	assert.Empty(t, dry.GetName())
	assert.Equal(t, dcctlv1.Operation_SWITCH, dry.GetType())
	assert.Equal(t, "datacenters/dc_a", dry.GetTarget())
	assert.Equal(t, int64(8), dry.GetNewGeneration())
	require.NotEmpty(t, dry.GetSteps())

	op, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a", RequestId: requestA})
	require.NoError(t, err)
	assert.Equal(t, "operations/op1", op.GetName())

	plans := env.engine.started()
	require.Len(t, plans, 1)

	p := plans[0]
	assert.Equal(t, dcctlv1.Operation_SWITCH, p.Type)
	assert.Equal(t, "dc_b", p.PreviousActive)
	assert.Equal(t, int64(7), p.PreviousGeneration)
	assert.Equal(t, preflight.Switch, p.Preflight.Action)
	assert.Equal(t, requestA, p.RequestID)
	assert.Equal(t, aliceUser, p.Initiator.GetUser())
	assert.Len(t, p.Steps, len(dry.GetSteps()))
}

// TestSwitchCannotPlan checks a switch the checks allow on a state that gives
// no plan.
func TestSwitchCannotPlan(t *testing.T) {
	env := switchEnv(t, "dc_a")
	dcs, _ := env.servers(t)

	_, err := dcs.SwitchDatacenter(aliceCtx(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a"})
	st := assertCode(t, codes.FailedPrecondition, err)
	assert.Equal(t, apierr.ReasonPreflightFailed, reasonOf(st))
	assert.Contains(t, st.Message(), "active already")
	assert.NotNil(t, preflightOf(st))
	assert.Empty(t, env.engine.started())
}
