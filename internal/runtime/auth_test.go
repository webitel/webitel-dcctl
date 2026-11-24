package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/config"
)

const alicePassword = "s3cret-pa55"

func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// authHeader returns the Authorization header with v, or none when v is empty.
func authHeader(v string) http.Header {
	if v == "" {
		return http.Header{}
	}

	return http.Header{"Authorization": {v}}
}

// seenStatus records the identity and metadata GetStatus is called with.
type seenStatus struct {
	dcctlv1.UnimplementedStatusServiceServer

	mu           sync.Mutex
	user         string
	initiator    string
	hasInitiator bool
	md           metadata.MD
}

func (s *seenStatus) GetStatus(ctx context.Context, _ *dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.user, _ = auth.UserFromContext(ctx)
	s.initiator, s.hasInitiator = auth.InitiatorFromContext(ctx)
	s.md, _ = metadata.FromIncomingContext(ctx)

	return &dcctlv1.Status{Name: "status"}, nil
}

// staticCatalog lists the same webitel-dcctl addresses in dc_a.
type staticCatalog []string

func (staticCatalog) Datacenters() []string { return []string{"dc_a"} }

func (c staticCatalog) RegisteredAddrs(context.Context, string, string) ([]string, error) {
	return c, nil
}

// newAuthRuntime runs a runtime with the interceptors and the API check of
// serve; peers lists the registered dcctl instances.
func newAuthRuntime(t *testing.T, p testPKI, peers staticCatalog) (*Runtime, *seenStatus) {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(alicePassword), bcrypt.MinCost)
	require.NoError(t, err)

	users := []config.User{{Name: "alice", PasswordHash: string(hash), Role: config.RoleOperator}}

	cfg := testConfig(p)
	cfg.HTTPOptions, cfg.PeerOptions, err = auth.ServerOptions(t.Context(), users, peers, testLogger())
	require.NoError(t, err)

	cfg.APIGuard = auth.CheckAPIRequest

	rt, err := New(cfg, testLogger())
	require.NoError(t, err)

	st := &seenStatus{}
	rt.RegisterService(&dcctlv1.StatusService_ServiceDesc, st)
	rt.RegisterService(&dcctlv1.OperationService_ServiceDesc, fakeOperations{})
	require.NoError(t, rt.RegisterGateway(t.Context(), dcctlv1.RegisterStatusServiceHandler))
	require.NoError(t, rt.RegisterGateway(t.Context(), dcctlv1.RegisterOperationServiceHandler))

	start(t, rt)

	return rt, st
}

func httpDo(t *testing.T, method, url string, header http.Header, body string) httpResult {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	require.NoError(t, err)

	maps.Copy(req.Header, header)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return httpResult{code: resp.StatusCode, header: resp.Header, body: data}
}

// withAuth adds the authorization metadata unless header is empty.
func withAuth(t *testing.T, ctx context.Context, header string) context.Context {
	t.Helper()

	if header == "" {
		return ctx
	}

	return metadata.AppendToOutgoingContext(ctx, "authorization", header)
}

var authCases = []struct {
	name     string
	header   string
	grpcCode codes.Code
	httpCode int
}{
	{"no_header", "", codes.Unauthenticated, http.StatusUnauthorized},
	{"wrong_password", basicAuth("alice", "wrong"), codes.Unauthenticated, http.StatusUnauthorized},
	{"unknown_user", basicAuth("mallory", alicePassword), codes.Unauthenticated, http.StatusUnauthorized},
	{"valid_user", basicAuth("alice", alicePassword), codes.OK, http.StatusOK},
}

func TestAuthGRPC(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	conn := grpcClient(t, rt.httpLn.Addr().String(), insecure.NewCredentials())
	statusClient := dcctlv1.NewStatusServiceClient(conn)
	operations := dcctlv1.NewOperationServiceClient(conn)

	for _, tt := range authCases {
		t.Run(tt.name+"/unary", func(t *testing.T) {
			_, err := statusClient.GetStatus(withAuth(t, callCtx(t), tt.header), &dcctlv1.GetStatusRequest{})
			assert.Equal(t, tt.grpcCode, status.Code(err), "GetStatus() error = %v", err)
		})

		t.Run(tt.name+"/stream", func(t *testing.T) {
			stream, err := operations.WatchOperation(withAuth(t, callCtx(t), tt.header), &dcctlv1.WatchOperationRequest{Name: "operations/op1"})
			require.NoError(t, err)

			_, err = stream.Recv()
			assert.Equal(t, tt.grpcCode, status.Code(err), "WatchOperation() error = %v", err)
		})
	}
}

func TestAuthGRPCReason(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	client := dcctlv1.NewStatusServiceClient(grpcClient(t, rt.httpLn.Addr().String(), insecure.NewCredentials()))

	_, err := client.GetStatus(withAuth(t, callCtx(t), basicAuth("alice", "wrong")), &dcctlv1.GetStatusRequest{})

	st := status.Convert(err)
	require.NotEmpty(t, st.Details())
	info, ok := st.Details()[0].(*errdetails.ErrorInfo)
	require.True(t, ok)
	assert.Equal(t, string(apierr.ReasonAuthenticationRequired), info.GetReason())
}

func TestAuthGateway(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	base := "http://" + rt.httpLn.Addr().String() + "/api/v1/"

	for _, tt := range authCases {
		for _, path := range []string{"status", "operations/op1:watch"} {
			t.Run(tt.name+"/"+path, func(t *testing.T) {
				resp := httpDo(t, http.MethodGet, base+path, authHeader(tt.header), "")

				assert.Equal(t, tt.httpCode, resp.code, "%s", resp.body)
				assert.Empty(t, resp.header.Values("Www-Authenticate"))
			})
		}
	}
}

func TestAuthGatewayBody(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	base := "http://" + rt.httpLn.Addr().String() + "/api/v1/"
	body := `{
		"id": "dcctl.auth.required",
		"code": 401,
		"status": "Unauthorized",
		"detail": "authentication required"
	}`

	t.Run("unary", func(t *testing.T) {
		resp := httpDo(t, http.MethodGet, base+"status", authHeader(basicAuth("alice", "wrong")), "")

		assert.Equal(t, http.StatusUnauthorized, resp.code)
		assert.Empty(t, resp.header.Values("Www-Authenticate"))
		assert.JSONEq(t, body, string(resp.body))
	})

	t.Run("stream", func(t *testing.T) {
		resp := httpDo(t, http.MethodGet, base+"operations/op1:watch", authHeader(basicAuth("alice", "wrong")), "")

		assert.Equal(t, http.StatusUnauthorized, resp.code)
		assert.Empty(t, resp.header.Values("Www-Authenticate"))
		assert.JSONEq(t, `{"error":`+body+`}`, string(resp.body))
	})
}

func TestAuthCredentialsNotInHandlerMetadata(t *testing.T) {
	rt, st := newAuthRuntime(t, newTestPKI(t), nil)

	header := authHeader(basicAuth("alice", alicePassword))
	header.Set("Cookie", "session=secret")

	resp := httpDo(t, http.MethodGet, "http://"+rt.httpLn.Addr().String()+"/api/v1/status", header, "")
	require.Equal(t, http.StatusOK, resp.code, "%s", resp.body)

	st.mu.Lock()
	defer st.mu.Unlock()

	assert.Equal(t, "alice", st.user)

	for _, key := range []string{"authorization", "grpcgateway-authorization", "grpcgateway-cookie"} {
		assert.Empty(t, st.md.Get(key), "handler metadata has %s", key)
	}
}

func TestAuthCredentialsInQueryIgnored(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	creds := basicAuth("alice", alicePassword)

	for _, query := range []url.Values{
		{"authorization": {creds}},
		{"access_token": {strings.TrimPrefix(creds, "Basic ")}},
	} {
		resp := httpDo(t, http.MethodGet, "http://"+rt.httpLn.Addr().String()+"/api/v1/status?"+query.Encode(), nil, "")
		assert.Equal(t, http.StatusUnauthorized, resp.code, "query %s: %s", query, resp.body)
	}
}

func TestAuthPostRequiresJSON(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	url := "http://" + rt.httpLn.Addr().String() + "/api/v1/operations/op1:resume"

	header := http.Header{}
	header.Set("Authorization", basicAuth("alice", alicePassword))
	header.Set("Content-Type", "text/plain")

	resp := httpDo(t, http.MethodPost, url, header, `{}`)

	assert.Equal(t, http.StatusUnsupportedMediaType, resp.code)
	assert.JSONEq(t, `{
		"code": 415,
		"status": "Unsupported Media Type",
		"detail": "content type must be application/json"
	}`, string(resp.body))

	header.Set("Content-Type", "application/json")

	resp = httpDo(t, http.MethodPost, url, header, `{}`)
	assert.Equal(t, http.StatusNotImplemented, resp.code, "the JSON request reaches the service: %s", resp.body)
}

func TestAuthNoCORS(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	url := "http://" + rt.httpLn.Addr().String() + "/api/v1/status"

	header := http.Header{}
	header.Set("Origin", "https://evil.example")
	header.Set("Access-Control-Request-Method", "GET")
	header.Set("Authorization", basicAuth("alice", alicePassword))

	for _, method := range []string{http.MethodOptions, http.MethodGet} {
		resp := httpDo(t, method, url, header, "")

		for k := range resp.header {
			assert.False(t, strings.HasPrefix(k, "Access-Control-"), "%s %s sets %s", method, url, k)
		}
	}
}

func TestAuthValidationGateway(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)

	header := http.Header{}
	header.Set("Authorization", basicAuth("alice", alicePassword))

	resp := httpDo(t, http.MethodGet, "http://"+rt.httpLn.Addr().String()+"/api/v1/operations/bad%21name", header, "")
	require.Equal(t, http.StatusBadRequest, resp.code, "%s", resp.body)

	var body struct {
		ID         string `json:"id"`
		Violations []struct {
			Type        string `json:"type"`
			Subject     string `json:"subject"`
			Description string `json:"description"`
		} `json:"violations"`
	}
	require.NoError(t, json.Unmarshal(resp.body, &body))

	assert.Equal(t, "dcctl.request.invalid", body.ID)
	require.Len(t, body.Violations, 1)
	assert.Equal(t, "field", body.Violations[0].Type)
	assert.Equal(t, "name", body.Violations[0].Subject)
	assert.NotEmpty(t, body.Violations[0].Description)
}

func TestAuthBeforeValidation(t *testing.T) {
	rt, _ := newAuthRuntime(t, newTestPKI(t), nil)
	client := dcctlv1.NewOperationServiceClient(grpcClient(t, rt.httpLn.Addr().String(), insecure.NewCredentials()))

	_, err := client.GetOperation(callCtx(t), &dcctlv1.GetOperationRequest{Name: "x"})
	assert.Equal(t, codes.Unauthenticated, status.Code(err), "error = %v", err)

	_, err = client.GetOperation(withAuth(t, callCtx(t), basicAuth("alice", alicePassword)), &dcctlv1.GetOperationRequest{Name: "x"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "error = %v", err)
}

func TestAuthInitiatorIgnoredOnHTTP(t *testing.T) {
	rt, st := newAuthRuntime(t, newTestPKI(t), nil)

	t.Run("gateway", func(t *testing.T) {
		header := http.Header{}
		header.Set("Authorization", basicAuth("alice", alicePassword))
		header.Set("Grpc-Metadata-X-Dcctl-Initiator", "user=mallory")

		resp := httpDo(t, http.MethodGet, "http://"+rt.httpLn.Addr().String()+"/api/v1/status", header, "")
		require.Equal(t, http.StatusOK, resp.code)

		st.mu.Lock()
		defer st.mu.Unlock()

		assert.Equal(t, "alice", st.user)
		assert.False(t, st.hasInitiator)
		assert.Empty(t, st.md.Get(auth.InitiatorKey))
	})

	t.Run("grpc", func(t *testing.T) {
		client := dcctlv1.NewStatusServiceClient(grpcClient(t, rt.httpLn.Addr().String(), insecure.NewCredentials()))
		ctx := metadata.AppendToOutgoingContext(callCtx(t), auth.InitiatorKey, "user=mallory")

		_, err := client.GetStatus(withAuth(t, ctx, basicAuth("alice", alicePassword)), &dcctlv1.GetStatusRequest{})
		require.NoError(t, err)

		st.mu.Lock()
		defer st.mu.Unlock()

		assert.False(t, st.hasInitiator)
		assert.Empty(t, st.md.Get(auth.InitiatorKey))
	})
}

func TestAuthPeer(t *testing.T) {
	p := newTestPKI(t)

	t.Run("registered", func(t *testing.T) {
		rt, st := newAuthRuntime(t, p, staticCatalog{"127.0.0.1:8443"})
		conn := grpcClient(t, rt.peerLn.Addr().String(), credentials.NewTLS(p.clientTLS(t, certSigned)))
		ctx := metadata.AppendToOutgoingContext(callCtx(t), auth.InitiatorKey, "user=alice")

		_, err := dcctlv1.NewStatusServiceClient(conn).GetStatus(ctx, &dcctlv1.GetStatusRequest{})
		require.NoError(t, err)

		st.mu.Lock()
		defer st.mu.Unlock()

		assert.True(t, st.hasInitiator)
		assert.Equal(t, "user=alice", st.initiator)
	})

	t.Run("not_registered", func(t *testing.T) {
		rt, _ := newAuthRuntime(t, p, staticCatalog{"10.0.0.1:8443"})
		conn := grpcClient(t, rt.peerLn.Addr().String(), credentials.NewTLS(p.clientTLS(t, certSigned)))

		_, err := dcctlv1.NewStatusServiceClient(conn).GetStatus(callCtx(t), &dcctlv1.GetStatusRequest{})
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "error = %v", err)
	})

	t.Run("validated", func(t *testing.T) {
		rt, _ := newAuthRuntime(t, p, staticCatalog{"127.0.0.1:8443"})
		conn := grpcClient(t, rt.peerLn.Addr().String(), credentials.NewTLS(p.clientTLS(t, certSigned)))

		_, err := dcctlv1.NewOperationServiceClient(conn).GetOperation(callCtx(t), &dcctlv1.GetOperationRequest{Name: "x"})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "error = %v", err)
	})
}
