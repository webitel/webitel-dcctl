package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

// gatewayError is the error body of the gateway.
type gatewayError struct {
	ID        string            `json:"id"`
	Metadata  map[string]string `json:"metadata"`
	Preflight *struct {
		Steps []json.RawMessage `json:"steps"`
	} `json:"preflight"`
}

func decodeError(t *testing.T, res httpResult) gatewayError {
	t.Helper()

	var body gatewayError

	require.NoError(t, json.Unmarshal([]byte(res.body), &body), res.body)

	return body
}

func TestActionGateway(t *testing.T) {
	env := newActionEnv()
	env.ops.put(interrupted("node-a", "dc_a", "dc_b"))
	a := startInstance(t, testpki.New(t, "test ca"), nodeA, env, &peerSet{})

	tests := []struct {
		name, path, body string
		want             dcctlv1.Operation_Type
	}{
		{"datacenter_verb", "/api/v1/datacenters/dc_a:markRecovered", `{"validate_only": true}`, dcctlv1.Operation_MARK_RECOVERED},
		{
			"collection_verb", "/api/v1/datacenters:resolveConflict",
			`{"winner": "datacenters/dc_a", "validate_only": true}`, dcctlv1.Operation_RESOLVE_CONFLICT,
		},
		{"arbiter_verb", "/api/v1/arbiter:initialize", `{"validate_only": true}`, dcctlv1.Operation_INITIALIZE_ARBITER},
		{"operation_verb", "/api/v1/operations/op7:resume", `{"validate_only": true}`, dcctlv1.Operation_TYPE_UNSPECIFIED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := a.post(t, tt.path, tt.body)
			require.Equal(t, http.StatusOK, res.code, res.body)

			var op struct {
				Name string `json:"name"`
				Type string `json:"type"`
			}

			require.NoError(t, json.Unmarshal([]byte(res.body), &op))
			assert.Empty(t, op.Name)

			if tt.want != dcctlv1.Operation_TYPE_UNSPECIFIED {
				assert.Equal(t, tt.want.String(), op.Type)
			}
		})
	}

	assert.Empty(t, env.engine.started())
}

func TestActionGatewayStartsAndReplays(t *testing.T) {
	env := newActionEnv()
	a := startInstance(t, testpki.New(t, "test ca"), nodeA, env, &peerSet{})

	res := a.post(t, "/api/v1/datacenters:syncApps", `{"request_id": "`+requestA+`"}`)
	require.Equal(t, http.StatusOK, res.code, res.body)

	var op struct {
		Name      string `json:"name"`
		RequestID string `json:"request_id"`
		Initiator struct {
			User string `json:"user"`
		} `json:"initiator"`
	}

	require.NoError(t, json.Unmarshal([]byte(res.body), &op))
	assert.Equal(t, "operations/op1", op.Name)
	assert.Equal(t, requestA, op.RequestID)
	assert.Equal(t, aliceUser, op.Initiator.User)

	res = a.post(t, "/api/v1/datacenters:syncApps", `{"request_id": "`+requestA+`", "confirmation": {"isolation": true}}`)
	assert.Equal(t, http.StatusConflict, res.code, res.body)

	body := decodeError(t, res)
	assert.Equal(t, "dcctl.request_id.mismatch", body.ID)
	assert.Equal(t, map[string]string{"operation": "operations/op1"}, body.Metadata)
}

func TestActionGatewayRefusal(t *testing.T) {
	env := newActionEnv()
	env.preflight.refuse(preflight.Switch, blockedCheck())
	a := startInstance(t, testpki.New(t, "test ca"), nodeA, env, &peerSet{})

	res := a.post(t, "/api/v1/datacenters/dc_a:switch", `{"validate_only": true}`)
	require.Equal(t, http.StatusBadRequest, res.code, res.body)

	body := decodeError(t, res)
	assert.Equal(t, "dcctl.preflight.failed", body.ID)
	require.NotNil(t, body.Preflight)
	assert.Len(t, body.Preflight.Steps, 1)
}

func TestActionGatewayUnimplemented(t *testing.T) {
	env := newActionEnv()
	env.planners.Switch = nil
	a := startInstance(t, testpki.New(t, "test ca"), nodeA, env, &peerSet{})

	res := a.post(t, "/api/v1/datacenters/dc_a:switch", `{}`)
	assert.Equal(t, http.StatusNotImplemented, res.code, res.body)
	assert.Empty(t, decodeError(t, res).ID)
}
