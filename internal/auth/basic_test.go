package auth_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/config"
)

const (
	alicePassword = "s3cret-pa55"
	// tcpClient is the address of a direct gRPC client.
	tcpClient = "192.0.2.10"
)

func basicHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func hash(t *testing.T, password string) string {
	t.Helper()

	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)

	return string(h)
}

func newBasic(t *testing.T) (*auth.Basic, *logtest.Recorder) {
	t.Helper()

	rec := logtest.NewRecorder()

	b, err := auth.NewBasic(t.Context(), []config.User{
		{Name: "alice", PasswordHash: hash(t, alicePassword), Role: config.RoleOperator},
	}, logging.New(logging.WithProvider(rec)))
	require.NoError(t, err)

	return b, rec
}

// incoming returns a context of a call from addr with the metadata pairs.
func incoming(t *testing.T, addr net.Addr, pairs ...string) context.Context {
	t.Helper()

	ctx := peer.NewContext(t.Context(), &peer.Peer{Addr: addr})

	return metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
}

func tcpAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(tcpClient), Port: 50123}
}

// requireUnauthenticated checks err is AUTHENTICATION_REQUIRED.
func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()

	st := status.Convert(err)
	require.Equal(t, codes.Unauthenticated, st.Code(), "error = %v", err)
	require.NotEmpty(t, st.Details())

	info, ok := st.Details()[0].(*errdetails.ErrorInfo)
	require.True(t, ok, "first detail is %T", st.Details()[0])
	assert.Equal(t, apierr.Domain, info.GetDomain())
	assert.Equal(t, string(apierr.ReasonAuthenticationRequired), info.GetReason())
}

// handled records what the handler saw.
type handled struct {
	called    bool
	user      string
	initiator bool
	md        metadata.MD
}

func (h *handled) unary(ctx context.Context, _ any) (any, error) {
	h.see(ctx)

	return "ok", nil
}

func (h *handled) stream(_ any, ss grpc.ServerStream) error {
	h.see(ss.Context())

	return nil
}

func (h *handled) see(ctx context.Context) {
	h.called = true
	h.user, _ = auth.UserFromContext(ctx)
	_, h.initiator = auth.InitiatorFromContext(ctx)
	h.md, _ = metadata.FromIncomingContext(ctx)
}

var unaryInfo = &grpc.UnaryServerInfo{FullMethod: "/webitel.dcctl.v1.StatusService/GetStatus"}

var streamInfo = &grpc.StreamServerInfo{FullMethod: "/webitel.dcctl.v1.OperationService/WatchOperation", IsServerStream: true}

type ctxStream struct {
	grpc.ServerStream

	ctx context.Context //nolint:containedctx // grpc.ServerStream returns its context from a method
}

func (s ctxStream) Context() context.Context { return s.ctx }

func TestBasicRejects(t *testing.T) {
	tests := []struct {
		name  string
		pairs []string
	}{
		{"no_header", nil},
		{"empty", []string{"authorization", ""}},
		{"bearer", []string{"authorization", "Bearer abc"}},
		{"bad_base64", []string{"authorization", "Basic %%%"}},
		{"no_colon", []string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice"))}},
		{"wrong_password", []string{"authorization", basicHeader("alice", "wrong")}},
		{"unknown_user", []string{"authorization", basicHeader("mallory", alicePassword)}},
		{"two_values", []string{"authorization", basicHeader("alice", alicePassword), "authorization", basicHeader("alice", "wrong")}},
	}

	b, _ := newBasic(t)

	for _, tt := range tests {
		t.Run(tt.name+"/unary", func(t *testing.T) {
			var h handled

			_, err := b.UnaryServerInterceptor()(incoming(t, tcpAddr(), tt.pairs...), "req", unaryInfo, h.unary)

			requireUnauthenticated(t, err)
			assert.False(t, h.called, "handler must not run")
		})

		t.Run(tt.name+"/stream", func(t *testing.T) {
			var h handled

			ss := ctxStream{ctx: incoming(t, tcpAddr(), tt.pairs...)}
			err := b.StreamServerInterceptor()(nil, ss, streamInfo, h.stream)

			requireUnauthenticated(t, err)
			assert.False(t, h.called, "handler must not run")
		})
	}
}

func TestBasicAccepts(t *testing.T) {
	b, _ := newBasic(t)
	pairs := []string{
		"authorization", basicHeader("alice", alicePassword),
		"grpcgateway-authorization", basicHeader("alice", alicePassword),
		auth.InitiatorKey, "user=mallory",
		"x-other", "kept",
	}

	check := func(t *testing.T, h handled) {
		t.Helper()

		assert.True(t, h.called)
		assert.Equal(t, "alice", h.user)
		assert.False(t, h.initiator, "the initiator is accepted only from peers")
		assert.Empty(t, h.md.Get(auth.InitiatorKey))
		assert.Empty(t, h.md.Get("authorization"), "credentials do not reach handlers")
		assert.Empty(t, h.md.Get("grpcgateway-authorization"), "credentials do not reach handlers")
		assert.Equal(t, []string{"kept"}, h.md.Get("x-other"))
	}

	t.Run("unary", func(t *testing.T) {
		var h handled

		got, err := b.UnaryServerInterceptor()(incoming(t, tcpAddr(), pairs...), "req", unaryInfo, h.unary)
		require.NoError(t, err)
		assert.Equal(t, "ok", got)
		check(t, h)
	})

	t.Run("stream", func(t *testing.T) {
		var h handled

		err := b.StreamServerInterceptor()(nil, ctxStream{ctx: incoming(t, tcpAddr(), pairs...)}, streamInfo, h.stream)
		require.NoError(t, err)
		check(t, h)
	})

	t.Run("scheme_case", func(t *testing.T) {
		var h handled

		header := "basic " + strings.TrimPrefix(basicHeader("alice", alicePassword), "Basic ")
		_, err := b.UnaryServerInterceptor()(incoming(t, tcpAddr(), "authorization", header), "req", unaryInfo, h.unary)
		require.NoError(t, err)
	})
}

func TestNewBasicNoUsers(t *testing.T) {
	_, err := auth.NewBasic(t.Context(), nil, logging.New(logging.WithProvider(logtest.NewRecorder())))
	require.Error(t, err)
}

func TestNewBasicWarnsOnDifferentCosts(t *testing.T) {
	hashAt := func(t *testing.T, cost int) string {
		t.Helper()

		h, err := bcrypt.GenerateFromPassword([]byte(alicePassword), cost)
		require.NoError(t, err)

		return string(h)
	}

	newWith := func(t *testing.T, costs ...int) []logtest.Record {
		t.Helper()

		users := make([]config.User, 0, len(costs))

		for i, cost := range costs {
			users = append(users, config.User{Name: fmt.Sprintf("user%d", i), PasswordHash: hashAt(t, cost), Role: config.RoleOperator})
		}

		rec := logtest.NewRecorder()
		_, err := auth.NewBasic(t.Context(), users, logging.New(logging.WithProvider(rec)))
		require.NoError(t, err)

		return rec.Records()
	}

	t.Run("same_cost", func(t *testing.T) {
		assert.Empty(t, newWith(t, bcrypt.MinCost, bcrypt.MinCost))
	})

	t.Run("different_costs", func(t *testing.T) {
		records := newWith(t, bcrypt.MinCost, bcrypt.MinCost+1)
		require.Len(t, records, 1)

		text := records[0].Body().String()
		for _, kv := range records[0].Attributes() {
			text += " " + string(kv.Key) + "=" + kv.Value.String()
		}

		assert.Contains(t, text, "bcrypt.cost.max=5")
		assert.NotContains(t, text, "user0")
		assert.NotContains(t, text, "$2a$")
	})
}

// bufAddr is the address of a call through the in-memory gateway connection.
type bufAddr struct{}

func (bufAddr) Network() string { return "bufconn" }
func (bufAddr) String() string  { return "bufconn" }

func TestBasicLogsFailedLogin(t *testing.T) {
	tests := []struct {
		name  string
		addr  net.Addr
		pairs []string
		want  string
	}{
		{"direct_grpc", tcpAddr(), []string{"x-forwarded-for", "203.0.113.66"}, tcpClient},
		{"gateway_direct", bufAddr{}, []string{"x-forwarded-for", "127.0.0.1"}, "127.0.0.1"},
		{"gateway_nginx", bufAddr{}, []string{"x-forwarded-for", "203.0.113.66, 198.51.100.7, 127.0.0.1"}, "198.51.100.7"},
		{"gateway_no_xff", bufAddr{}, nil, "bufconn"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, rec := newBasic(t)
			header := basicHeader("alice", "wrong-password")

			_, err := b.UnaryServerInterceptor()(incoming(t, tt.addr, append(tt.pairs, "authorization", header)...), "req", unaryInfo, (&handled{}).unary)
			requireUnauthenticated(t, err)

			records := rec.Records()
			require.Len(t, records, 1)

			attrs := map[string]string{}
			for _, kv := range records[0].Attributes() {
				attrs[string(kv.Key)] = kv.Value.String()
			}

			assert.Equal(t, tt.want, attrs["client.address"])
			assert.Equal(t, "alice", attrs["user.name"])

			text := records[0].Body().String()
			for _, v := range attrs {
				text += " " + v
			}

			assert.NotContains(t, text, "wrong-password")
			assert.NotContains(t, text, strings.TrimPrefix(header, "Basic "))
		})
	}
}

func TestBasicLogsUnknownUserWithoutName(t *testing.T) {
	b, rec := newBasic(t)

	_, err := b.UnaryServerInterceptor()(incoming(t, tcpAddr(), "authorization", basicHeader("alice-typo-password", "x")), "req", unaryInfo, (&handled{}).unary)
	requireUnauthenticated(t, err)

	records := rec.Records()
	require.Len(t, records, 1)

	for _, kv := range records[0].Attributes() {
		assert.NotContains(t, kv.Value.String(), "alice-typo-password", "a name that is not a user may be a mistyped password")
	}
}

func TestBasicMissingHeaderNotLogged(t *testing.T) {
	b, rec := newBasic(t)

	_, err := b.UnaryServerInterceptor()(incoming(t, tcpAddr()), "req", unaryInfo, (&handled{}).unary)
	requireUnauthenticated(t, err)

	assert.Empty(t, rec.Records(), "a call without credentials is not a login attempt")
}
