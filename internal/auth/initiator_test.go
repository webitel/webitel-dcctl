package auth_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/auth"
)

var selfInstance = &dcctlv1.Instance{Node: "node-b", Datacenter: "datacenters/dc_b", Address: "10.1.0.1:10023"}

func aliceInitiator() *dcctlv1.Initiator {
	return &dcctlv1.Initiator{
		User:     "alice",
		Address:  "203.0.113.7",
		Instance: &dcctlv1.Instance{Node: "node-a", Datacenter: "datacenters/dc_a", Address: "10.0.0.1:10023"},
	}
}

// seenInitiator records what auth.Initiator returned in the handler.
type seenInitiator struct {
	called    bool
	ini       *dcctlv1.Initiator
	forwarded bool
	err       error
}

func (s *seenInitiator) unary(ctx context.Context, _ any) (any, error) {
	s.called = true
	s.ini, s.forwarded, s.err = auth.Initiator(ctx, selfInstance)

	return "ok", nil
}

func reasonOf(t *testing.T, err error) apierr.Reason {
	t.Helper()

	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return apierr.Reason(info.GetReason())
		}
	}

	return ""
}

// forwardedValue returns the InitiatorKey value Forwarding sets for ini.
func forwardedValue(t *testing.T, ini *dcctlv1.Initiator) string {
	t.Helper()

	ctx, err := auth.Forwarding(t.Context(), ini)
	require.NoError(t, err)

	md, _ := metadata.FromOutgoingContext(ctx)
	require.Len(t, md.Get(auth.InitiatorKey), 1)

	return md.Get(auth.InitiatorKey)[0]
}

func encode(t *testing.T, ini *dcctlv1.Initiator) string {
	t.Helper()

	raw, err := proto.Marshal(ini)
	require.NoError(t, err)

	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestInitiatorForwarded(t *testing.T) {
	p := newPeers(newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}}))
	want := aliceInitiator()

	var s seenInitiator

	ctx := fromPeer(t.Context(), []string{"10.0.0.1"}, auth.InitiatorKey, forwardedValue(t, want))
	_, err := p.UnaryServerInterceptor()(ctx, "req", unaryInfo, s.unary)
	require.NoError(t, err)
	require.NoError(t, s.err)
	assert.True(t, s.forwarded)
	assertProto(t, want, s.ini)
}

func TestForwardingSendsOnlyTheInitiator(t *testing.T) {
	ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("authorization", "Basic x", "cookie", "c"))
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Basic y", "x-forwarded-for", "203.0.113.7"))

	out, err := auth.Forwarding(ctx, aliceInitiator())
	require.NoError(t, err)

	md, _ := metadata.FromOutgoingContext(out)
	assert.Equal(t, []string{auth.InitiatorKey}, keys(md))
}

func TestForwardingRefusesInvalidInitiator(t *testing.T) {
	ini := aliceInitiator()
	ini.User = ""

	_, err := auth.Forwarding(t.Context(), ini)
	require.Error(t, err)
}

func TestInitiatorOfUser(t *testing.T) {
	b, _ := newBasic(t)

	var s seenInitiator

	ctx := incoming(t, tcpAddr(), "authorization", basicHeader("alice", alicePassword))
	_, err := b.UnaryServerInterceptor()(ctx, "req", unaryInfo, s.unary)
	require.NoError(t, err)
	require.NoError(t, s.err)
	assert.False(t, s.forwarded)

	want := &dcctlv1.Initiator{User: "alice", Address: tcpClient, Instance: selfInstance}
	assertProto(t, want, s.ini)
}

// pipeAddr is the address of an in-process connection, as the gateway's.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "bufconn" }
func (pipeAddr) String() string  { return "bufconn" }

// TestInitiatorOfUserBadAddress checks that a client address the client
// controls and that is not valid is replaced with the address of the
// connection, the same whether the action runs here or is forwarded.
func TestInitiatorOfUserBadAddress(t *testing.T) {
	tests := []struct {
		name      string
		forwarded string
	}{
		{"long", strings.Repeat("x", 300) + ", 10.0.0.9"},
		{"control_character", "203.0.113.7\u0001, 10.0.0.9"},
	}

	b, _ := newBasic(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s seenInitiator

			ctx := incoming(t, pipeAddr{}, "authorization", basicHeader("alice", alicePassword), "x-forwarded-for", tt.forwarded)
			_, err := b.UnaryServerInterceptor()(ctx, "req", unaryInfo, s.unary)
			require.NoError(t, err)
			require.NoError(t, s.err)
			assert.Equal(t, "bufconn", s.ini.GetAddress())

			_, err = auth.Forwarding(t.Context(), s.ini)
			require.NoError(t, err, "the initiator can be forwarded")
		})
	}
}

func TestInitiatorOfNobody(t *testing.T) {
	p := newPeers(newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}}))

	var s seenInitiator

	_, err := p.UnaryServerInterceptor()(fromPeer(t.Context(), []string{"10.0.0.1"}), "req", unaryInfo, s.unary)
	require.NoError(t, err)
	requireUnauthenticated(t, s.err)
	assert.Nil(t, s.ini)
}

func TestInitiatorMalformed(t *testing.T) {
	system := aliceInitiator()
	system.System = true

	noUser := aliceInitiator()
	noUser.User = ""

	noNode := aliceInitiator()
	noNode.Instance.Node = ""

	badDC := aliceInitiator()
	badDC.Instance.Datacenter = "dc_a"

	control := aliceInitiator()
	control.User = "alice\nadmin"

	long := aliceInitiator()
	long.User = strings.Repeat("a", 300)

	otherAddress := aliceInitiator()
	otherAddress.Instance.Address = "10.0.0.2:10023"

	noAddress := aliceInitiator()
	noAddress.Instance.Address = ""

	tests := []struct {
		name  string
		value string
	}{
		{"not_base64", "user=alice"},
		{"not_proto", base64.RawURLEncoding.EncodeToString([]byte{0xff, 0xff, 0xff})},
		{"system", encode(t, system)},
		{"no_user", encode(t, noUser)},
		{"no_node", encode(t, noNode)},
		{"bad_datacenter", encode(t, badDC)},
		{"control_character", encode(t, control)},
		{"long_field", encode(t, long)},
		{"too_long", strings.Repeat("A", 4096)},
		{"address_not_in_certificate", encode(t, otherAddress)},
		{"no_instance_address", encode(t, noAddress)},
	}

	p := newPeers(newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}}))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s seenInitiator

			ctx := fromPeer(t.Context(), []string{"10.0.0.1"}, auth.InitiatorKey, tt.value)
			_, err := p.UnaryServerInterceptor()(ctx, "req", unaryInfo, s.unary)
			require.NoError(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(s.err), "error = %v", s.err)
			assert.Equal(t, apierr.ReasonRequestInvalid, reasonOf(t, s.err))
			assert.True(t, s.forwarded)
		})
	}
}

func TestPeersRefuseSeveralInitiators(t *testing.T) {
	p := newPeers(newCatalog(map[string][]string{"dc_a": {"10.0.0.1:8443"}}))
	v := encode(t, aliceInitiator())

	var s seenInitiator

	ctx := fromPeer(t.Context(), []string{"10.0.0.1"}, auth.InitiatorKey, v, auth.InitiatorKey, v)
	_, err := p.UnaryServerInterceptor()(ctx, "req", unaryInfo, s.unary)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "error = %v", err)
	assert.Equal(t, apierr.ReasonRequestInvalid, reasonOf(t, err))
	assert.False(t, s.called)
}

func assertProto(t *testing.T, want, got proto.Message) {
	t.Helper()

	assert.True(t, proto.Equal(want, got), "got %v; want %v", got, want)
}

func keys(md metadata.MD) []string {
	out := make([]string, 0, len(md))
	for k := range md {
		out = append(out, k)
	}

	return out
}
