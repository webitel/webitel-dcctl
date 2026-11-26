package runtime_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/preflight/answer"
	"github.com/webitel/webitel-dcctl/internal/runtime"
)

const (
	switchPath   = "/v1/datacenters/{datacenter}:switch"
	failoverPath = "/v1/datacenters/{datacenter}:failover"
	resumePath   = "/v1/operations/{operation}:resume"
)

// swaggerExample returns the JSON example of the POST response code of path.
func swaggerExample(t *testing.T, path, code string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile("../../api/dcctl.swagger.json")
	require.NoError(t, err)

	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Examples map[string]json.RawMessage `json:"examples"`
			} `json:"responses"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))

	ex, ok := doc.Paths[path]["post"].Responses[code].Examples["application/json"]
	require.True(t, ok, "example %s of POST %s", code, path)

	var out map[string]any
	require.NoError(t, json.Unmarshal(ex, &out))

	return out
}

// refusalBody returns the body the gateway answers the refusal of checks with.
func refusalBody(t *testing.T, checks []check.Check, given check.Confirmations, plan []*dcctlv1.Operation_Step, mode dcctlv1.Operation_Mode) (int, map[string]any) {
	t.Helper()

	err := answer.Refusal(check.Decide(checks, given), plan, mode)
	require.Error(t, err)

	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api"+switchPath, nil)
	runtime.WriteError(w, r, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	return w.Code, body
}

func TestRefusalBody_preflightFailed(t *testing.T) {
	checks := []check.Check{
		{
			ID: check.CoordLockFree, Result: check.Pass, Effect: check.Block, Detail: "the lock is free",
			Facts: map[string]any{"unreachable": []any{}},
		},
		{
			ID: check.PatroniTargetLag, Result: check.Fail, Effect: check.Block, Datacenter: "dc_b",
			Detail: "dc_b lags 52428800 bytes, more than 1048576", Facts: map[string]any{"lag_bytes": 52428800, "limit_bytes": 1048576},
		},
	}
	plan := []*dcctlv1.Operation_Step{{Type: dcctlv1.Operation_Step_APPS_PREFLIGHT}}

	code, got := refusalBody(t, checks, check.Confirmations{}, plan, dcctlv1.Operation_QUORUM)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, swaggerExample(t, switchPath, "400"), got)
}

func TestRefusalBody_confirmationRequired(t *testing.T) {
	checks := []check.Check{{
		ID: check.CoordIsolation, Result: check.Fail, Effect: check.Confirm, Confirmation: check.ConfirmIsolation,
		Detail: "the old primary must be isolated",
		Facts: map[string]any{
			"mode": "forced_only",
			"voters": []any{
				map[string]any{"name": "dc_a", "reachable": false},
				map[string]any{"name": "dc_b", "reachable": true, "generation": 7, "active_datacenter": "dc_a"},
				map[string]any{"name": "arbiter", "reachable": false},
			},
			"datacenters": []any{map[string]any{
				"datacenter": "dc_a", "patroni_reachable": false,
				"last_seen": map[string]any{
					"time": "2026-11-23T10:01:58Z", "member": "pg-a1", "role": "primary", "xlog_location": 83886176,
				},
			}},
		},
	}}
	plan := []*dcctlv1.Operation_Step{
		{Type: dcctlv1.Operation_Step_GENERATION, PointOfNoReturn: true},
		{Type: dcctlv1.Operation_Step_PATRONI_PROMOTE, Datacenter: "datacenters/dc_b"},
	}

	code, got := refusalBody(t, checks, check.Confirmations{}, plan, dcctlv1.Operation_FORCED)

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, swaggerExample(t, failoverPath, "400"), got)
}

func TestRefusalBody_lockUnavailable(t *testing.T) {
	checks := []check.Check{{
		ID: check.CoordLockFree, Result: check.Fail, Effect: check.Block,
		Detail: "no Consul KV answers, the lock may be held",
		Facts:  map[string]any{"reachable": false, "unreachable": []any{"dc_a", "dc_b"}},
	}}

	code, got := refusalBody(t, checks, check.Confirmations{}, nil, dcctlv1.Operation_MODE_UNSPECIFIED)

	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.NotEmpty(t, got["violations"])
	assert.NotEmpty(t, got["preflight"])

	delete(got, "violations")
	delete(got, "preflight")
	assert.Equal(t, swaggerExample(t, failoverPath, "503"), got)
}

func TestRefusalBody_lockBusy(t *testing.T) {
	checks := []check.Check{{
		ID: check.CoordLockFree, Result: check.Fail, Effect: check.Block,
		Detail: `dcctl/lock in dc_a is held by "dcctl-b1" of dc_b`,
		Facts: map[string]any{
			"kv": "dc_a", "holder": "dcctl-b1", "datacenter": "dc_b", "operation": "0b9f3c2e-5d1a-4c55-9a7e-2f4d8c1b6a10",
		},
	}}

	code, got := refusalBody(t, checks, check.Confirmations{}, nil, dcctlv1.Operation_MODE_UNSPECIFIED)

	assert.Equal(t, http.StatusConflict, code)
	assert.NotEmpty(t, got["violations"])
	assert.NotEmpty(t, got["preflight"])

	delete(got, "violations")
	delete(got, "preflight")
	assert.Equal(t, swaggerExample(t, switchPath, "409"), got)
}

// TestRefusalBody_reasons compares the reasons whose examples carry only metadata.
func TestRefusalBody_reasons(t *testing.T) {
	const op = "operations/0b9f3c2e-5d1a-4c55-9a7e-2f4d8c1b6a10"

	tests := []struct {
		name       string
		failed     check.Check
		path, code string
	}{
		{
			name: "operation_running",
			failed: check.Check{
				ID: check.CoordNoOperation, Result: check.Fail, Effect: check.Block,
				Detail: "operation " + op + " is running", Facts: map[string]any{"op_id": op, "status": "running"},
			},
			path: "/v1/datacenters/{datacenter}:demote", code: "400",
		},
		{
			name: "leader_unavailable",
			failed: check.Check{
				ID: check.CoordLeaderReachable, Result: check.Fail, Effect: check.Block,
				Detail: "the leading instance is critical in Consul", Facts: map[string]any{"instance": "dcctl-b1"},
			},
			path: switchPath, code: "503",
		},
		{
			name: "operation_not_interrupted",
			failed: check.Check{
				ID: check.OpInterrupted, Result: check.Fail, Effect: check.Block,
				Detail: op + " is running", Facts: map[string]any{"operation": op, "state": "RUNNING"},
			},
			path: resumePath, code: "400",
		},
		{
			name: "operation_not_found",
			failed: check.Check{
				ID: check.OpInterrupted, Result: check.Fail, Effect: check.Block,
				Detail: op + " is not found", Facts: map[string]any{"operation": op, "found": false},
			},
			path: resumePath, code: "404",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, got := refusalBody(t, []check.Check{tt.failed}, check.Confirmations{}, nil, dcctlv1.Operation_MODE_UNSPECIFIED)

			want := swaggerExample(t, tt.path, tt.code)

			assert.EqualValues(t, want["code"], code)

			for _, key := range []string{"id", "code", "status", "detail", "metadata"} {
				assert.Equal(t, want[key], got[key], key)
			}
		})
	}
}
