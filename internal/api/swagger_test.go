package api

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const swaggerPath = "../../api/dcctl.swagger.json"

// swaggerExample returns the application/json example of the response code
// of the method on path, as raw JSON.
func swaggerExample(t *testing.T, path, method, code string) string {
	t.Helper()

	raw, err := os.ReadFile(swaggerPath)
	require.NoError(t, err)

	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Examples map[string]json.RawMessage `json:"examples"`
			} `json:"responses"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))

	ex, ok := doc.Paths[path][method].Responses[code].Examples["application/json"]
	require.True(t, ok, "no example for %s %s %s", method, path, code)

	return string(ex)
}

// unproducible are JSON paths of a swagger example the server cannot give,
// each with the reason; '*' matches every array element.
type unproducible map[string]string

// assertExample compares got with the example after dropping skip from both.
func assertExample(t *testing.T, example, got string, skip unproducible) {
	t.Helper()

	var want, have any
	require.NoError(t, json.Unmarshal([]byte(example), &want))
	require.NoError(t, json.Unmarshal([]byte(got), &have))

	for p := range skip {
		dropPath(want, strings.Split(p, "."))
		dropPath(have, strings.Split(p, "."))
	}

	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)

	haveJSON, err := json.Marshal(have)
	require.NoError(t, err)

	assert.JSONEq(t, string(wantJSON), string(haveJSON))
}

func dropPath(v any, path []string) {
	if len(path) == 0 {
		return
	}

	switch node := v.(type) {
	case map[string]any:
		if len(path) == 1 {
			delete(node, path[0])

			return
		}

		dropPath(node[path[0]], path[1:])
	case []any:
		if path[0] == "*" {
			for _, e := range node {
				dropPath(e, path[1:])
			}

			return
		}

		if i, err := strconv.Atoi(path[0]); err == nil && i < len(node) {
			dropPath(node[i], path[1:])
		}
	}
}

// gatewayJSON marshals m as the gateway does.
func gatewayJSON(t *testing.T, m proto.Message) string {
	t.Helper()

	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	require.NoError(t, err)

	return string(b)
}
