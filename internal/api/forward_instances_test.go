package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

var (
	nodeA = operation.Instance{Node: "node-a", Datacenter: "dc_a", Address: "127.0.0.1:10023"}
	nodeB = operation.Instance{Node: "node-b", Datacenter: "dc_b", Address: "127.0.0.1:10023"}
)

// pair runs node-a of dc_a and node-b of dc_b over the same copies of
// dcctl/op; each finds the other in its catalog.
func pair(t *testing.T) (a, b *instance) {
	t.Helper()

	ca := testpki.New(t, "test ca")
	peers := &peerSet{}

	envA, envB := newActionEnv(), newActionEnv()
	envB.ops = envA.ops
	envB.engine.ops = envA.ops

	a = startInstance(t, ca, nodeA, envA, peers)
	b = startInstance(t, ca, nodeB, envB, peers)

	envA.catalog.set("dc_b", "node-b", b.peerAddr)
	envB.catalog.set("dc_a", "node-a", a.peerAddr)

	return a, b
}

func TestForwardBetweenInstances(t *testing.T) {
	a, b := pair(t)

	req := &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b", RequestId: requestA}

	op, err := a.datacenters().SwitchDatacenter(aliceCall(t), req)
	require.NoError(t, err)
	assert.Equal(t, "operations/op1", op.GetName())

	assert.Empty(t, a.env.engine.started(), "node-a does not lead a switch to dc_b")

	plans := b.env.engine.started()
	require.Len(t, plans, 1)

	ini := plans[0].Initiator
	assert.Equal(t, aliceUser, ini.GetUser())
	assert.Equal(t, "127.0.0.1", ini.GetAddress())
	assert.Equal(t, "node-a", ini.GetInstance().GetNode())
	assert.Equal(t, "datacenters/dc_a", ini.GetInstance().GetDatacenter())
	assert.Equal(t, requestA, plans[0].RequestID)

	again, err := a.datacenters().SwitchDatacenter(aliceCall(t), req)
	require.NoError(t, err)
	assert.Equal(t, op.GetName(), again.GetName())
	assert.Equal(t, 1, a.forwards.count(), "a replay is answered from dcctl/op")
	assert.Len(t, b.env.engine.started(), 1)
}

func TestForwardValidateOnly(t *testing.T) {
	a, b := pair(t)

	op, err := a.datacenters().DemoteDatacenter(aliceCall(t), &dcctlv1.DemoteDatacenterRequest{
		Name: "datacenters/dc_b", ValidateOnly: true,
	})
	require.NoError(t, err)
	assert.Empty(t, op.GetName())
	assert.Equal(t, dcctlv1.Operation_DEMOTE, op.GetType())
	assert.Len(t, b.env.preflight.requests(), 1)
	assert.Empty(t, a.env.preflight.requests())
}

func TestForwardLeaderRefusal(t *testing.T) {
	a, b := pair(t)
	b.env.preflight.refuse(preflight.Switch, blockedCheck())

	_, err := a.datacenters().SwitchDatacenter(aliceCall(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	st := assertCode(t, codes.FailedPrecondition, err)
	assert.Equal(t, apierr.ReasonPreflightFailed, reasonOf(st))
	require.NotNil(t, preflightOf(st), "the Preflight detail of the leading instance")
	assert.Len(t, preflightOf(st).GetSteps(), 1)
}

// TestForwardLoopPrevented makes node-a believe node-b leads dc_c: node-b
// does not forward the call again.
func TestForwardLoopPrevented(t *testing.T) {
	a, b := pair(t)
	a.env.catalog.set("dc_c", "node-c", b.peerAddr)
	b.env.catalog.set("dc_c", "node-c", a.peerAddr)

	_, err := a.datacenters().SwitchDatacenter(aliceCall(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_c"})
	require.NoError(t, err)

	assert.Equal(t, 1, a.forwards.count())
	assert.Equal(t, 0, b.forwards.count(), "a forwarded call is not forwarded again")
	assert.Len(t, b.env.preflight.requests(), 1)
}

func TestForwardLeaderUnreachable(t *testing.T) {
	ca := testpki.New(t, "test ca")
	env := newActionEnv()
	a := startInstance(t, ca, nodeA, env, &peerSet{})

	dead := closedAddr(t)
	env.catalog.set("dc_b", "node-b", dead)

	_, err := a.datacenters().SwitchDatacenter(aliceCall(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	st := assertCode(t, codes.Unavailable, err)
	assert.Equal(t, apierr.ReasonLeaderUnavailable, reasonOf(st))
	assert.Equal(t, map[string]string{"datacenters": "dc_b"}, metadataOf(st))
	assert.NotContains(t, st.Message(), "127.0.0.1")

	res := a.post(t, "/api/v1/datacenters/dc_b:switch", `{}`)
	assert.Equal(t, http.StatusServiceUnavailable, res.code, res.body)
	assert.NotContains(t, res.body, "127.0.0.1")

	var body struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	}

	require.NoError(t, json.Unmarshal([]byte(res.body), &body))
	assert.Equal(t, "dcctl.leader.unavailable", body.ID)
	assert.Equal(t, map[string]string{"datacenters": "dc_b"}, body.Metadata)
}

// TestForwardPeerUntrusted checks a leading instance whose certificate is not
// of the installation CA.
func TestForwardPeerUntrusted(t *testing.T) {
	ca := testpki.New(t, "test ca")
	envA, envB := newActionEnv(), newActionEnv()

	a := startInstance(t, ca, nodeA, envA, &peerSet{})
	b := startInstance(t, testpki.New(t, "other ca"), nodeB, envB, &peerSet{})
	envA.catalog.set("dc_b", "node-b", b.peerAddr)

	_, err := a.datacenters().SwitchDatacenter(aliceCall(t), &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	st := assertCode(t, codes.Unavailable, err)
	assert.Equal(t, apierr.ReasonLeaderUnavailable, reasonOf(st))
	assert.Empty(t, envB.engine.started())
}
