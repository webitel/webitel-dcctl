package api

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

// otherInitiator returns the InitiatorKey value of mallory, as if forwarded.
func otherInitiator(t *testing.T) string {
	t.Helper()

	ini := aliceFrom("node-x", "dc_c")
	ini.User = "mallory"

	raw, err := proto.Marshal(ini)
	require.NoError(t, err)

	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestHTTPCallIsNotForwarded checks that x-dcctl-initiator a client sends to
// listen.http does not make its call forwarded: the action runs on behalf of
// the authenticated user and is still forwarded to its leader.
func TestHTTPCallIsNotForwarded(t *testing.T) {
	a, b := pair(t)

	ctx := metadata.AppendToOutgoingContext(aliceCall(t), auth.InitiatorKey, otherInitiator(t))
	_, err := a.datacenters().SwitchDatacenter(ctx, &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"})
	require.NoError(t, err)
	assert.Equal(t, 1, a.forwards.count())

	plans := b.env.engine.started()
	require.Len(t, plans, 1)
	assert.Equal(t, aliceUser, plans[0].Initiator.GetUser())
	assert.Equal(t, "node-a", plans[0].Initiator.GetInstance().GetNode())

	ctx = metadata.AppendToOutgoingContext(callCtx(t), auth.InitiatorKey, otherInitiator(t))
	_, err = a.datacenters().SwitchDatacenter(ctx, &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_a"})
	assert.Equal(t, codes.Unauthenticated, status.Code(err), "error = %v", err)
}

func TestGatewayCallIsNotForwarded(t *testing.T) {
	a, b := pair(t)

	for _, header := range []string{"Grpc-Metadata-X-Dcctl-Initiator", "X-Dcctl-Initiator"} {
		res := a.post(t, "/api/v1/datacenters/dc_b:switch", `{}`, header, otherInitiator(t))
		assert.Equal(t, http.StatusOK, res.code, res.body)
	}

	plans := b.env.engine.started()
	require.Len(t, plans, 2)

	for _, p := range plans {
		assert.Equal(t, aliceUser, p.Initiator.GetUser())
	}

	assert.Equal(t, 2, a.forwards.count())
}

// TestClientAddressNotValid checks that a client address that cannot be
// forwarded fails neither the action here nor the forwarded one.
func TestClientAddressNotValid(t *testing.T) {
	a, b := pair(t)
	long := strings.Repeat("x", 300)

	for _, path := range []string{"/api/v1/datacenters/dc_a:switch", "/api/v1/datacenters/dc_b:switch"} {
		res := a.post(t, path, `{}`, "X-Forwarded-For", long)
		assert.Equal(t, http.StatusOK, res.code, "%s: %s", path, res.body)
	}

	require.Len(t, a.env.engine.started(), 1)
	require.Len(t, b.env.engine.started(), 1)
	assert.NotEqual(t, long, a.env.engine.started()[0].Initiator.GetAddress())
	assert.NotEqual(t, long, b.env.engine.started()[0].Initiator.GetAddress())
}

// TestPeerListenerAdmission calls the peer listener of an instance directly.
func TestPeerListenerAdmission(t *testing.T) {
	ca := testpki.New(t, "test ca")
	env := newActionEnv()
	b := startInstance(t, ca, nodeB, env, &peerSet{})
	req := &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/dc_b"}
	method := dcctlv1.DatacenterService_SwitchDatacenter_FullMethodName

	registered, err := NewPeerClient(ca.Files(t, "127.0.0.1"), 5*time.Second, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = registered.Close() })

	err = registered.Forward(callCtx(t), b.peerAddr, method, req, &dcctlv1.Operation{})
	assert.Equal(t, codes.Unauthenticated, status.Code(err), "no initiator: %v", err)

	ctx := metadata.AppendToOutgoingContext(callCtx(t), "authorization", basicHeader())
	err = registered.Forward(ctx, b.peerAddr, method, req, &dcctlv1.Operation{})
	assert.Equal(t, codes.Unauthenticated, status.Code(err), "basic auth on listen.peer: %v", err)

	unregistered, err := NewPeerClient(ca.Files(t, "10.9.9.9"), 5*time.Second, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unregistered.Close() })

	ctx = metadata.AppendToOutgoingContext(callCtx(t), auth.InitiatorKey, otherInitiator(t))
	err = unregistered.Forward(ctx, b.peerAddr, method, req, &dcctlv1.Operation{})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "certificate of an IP not registered: %v", err)
	assert.Empty(t, env.engine.started())
}
