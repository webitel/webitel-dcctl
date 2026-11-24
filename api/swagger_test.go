package api_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	_ "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

const (
	swaggerPath = "dcctl.swagger.json"
	errorSchema = "#/definitions/webitel.dcctl.v1.Error"
)

type schema struct {
	Ref        string            `json:"$ref"`
	Properties map[string]schema `json:"properties"`
}

type response struct {
	Schema   schema                     `json:"schema"`
	Examples map[string]json.RawMessage `json:"examples"`
}

type operation struct {
	Responses map[string]response `json:"responses"`
}

type swagger struct {
	Paths map[string]map[string]operation `json:"paths"`
}

func loadSwagger(t *testing.T) swagger {
	t.Helper()

	raw, err := os.ReadFile(swaggerPath)
	require.NoError(t, err)

	var doc swagger
	require.NoError(t, json.Unmarshal(raw, &doc))

	return doc
}

type example struct {
	name    string
	message protoreflect.MessageDescriptor
	body    json.RawMessage
}

func loadExamples(t *testing.T) []example {
	t.Helper()

	var out []example

	for path, methods := range loadSwagger(t).Paths {
		for method, op := range methods {
			for code, resp := range op.Responses {
				body, ok := resp.Examples["application/json"]
				if !ok {
					continue
				}

				ref := resp.Schema.Ref
				if result, stream := resp.Schema.Properties["result"]; stream {
					ref = result.Ref

					var line struct {
						Result json.RawMessage `json:"result"`
					}
					require.NoError(t, json.Unmarshal(body, &line))
					body = line.Result
				}

				out = append(out, example{
					name:    method + " " + path + " " + code,
					message: findMessage(t, ref),
					body:    body,
				})
			}
		}
	}

	return out
}

// findMessage resolves a definition ref of the "fqn" naming strategy, for
// example "#/definitions/webitel.dcctl.v1.Datacenter".
func findMessage(t *testing.T, ref string) protoreflect.MessageDescriptor {
	t.Helper()

	name := protoreflect.FullName(strings.TrimPrefix(ref, "#/definitions/"))
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	require.NoError(t, err, "schema %q", ref)

	msg, ok := desc.(protoreflect.MessageDescriptor)
	require.True(t, ok, "schema %q is not a message", ref)

	return msg
}

func TestSwaggerExamplesMatchMessages(t *testing.T) {
	t.Parallel()

	for _, ex := range loadExamples(t) {
		t.Run(ex.name, func(t *testing.T) {
			t.Parallel()

			msg := dynamicpb.NewMessage(ex.message)
			assert.NoError(t, protojson.Unmarshal(ex.body, msg), "example of %s", ex.message.FullName())
		})
	}
}

func TestSwaggerReadsHaveExamples(t *testing.T) {
	t.Parallel()

	for path, methods := range loadSwagger(t).Paths {
		op, ok := methods["get"]
		if !ok {
			continue
		}

		_, ok = op.Responses["200"].Examples["application/json"]
		assert.True(t, ok, "GET %s has no 200 example", path)
	}
}

func TestSwaggerErrorsUseErrorSchema(t *testing.T) {
	t.Parallel()

	for path, methods := range loadSwagger(t).Paths {
		for method, op := range methods {
			for code, resp := range op.Responses {
				if code == "200" {
					continue
				}

				assert.Equal(t, errorSchema, resp.Schema.Ref, "%s %s %s", method, path, code)
			}
		}
	}
}

func TestSwaggerExamplesCoverErrorIDs(t *testing.T) {
	t.Parallel()

	ids := map[string]bool{}

	for _, ex := range loadExamples(t) {
		if ex.message.FullName() != "webitel.dcctl.v1.Error" {
			continue
		}

		var body struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(ex.body, &body))

		ids[body.ID] = true
	}

	for _, r := range apierr.Reasons() {
		if r.ID() == "" {
			continue
		}

		assert.True(t, ids[r.ID()], "no swagger example for %s", r.ID())
	}
}
