package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

var rlo = string(rune(0x202e))

var hostile = "\x1b[31mred\x1b]0;title\x07" + rlo + "evil\x00\x7f\u009b"

func TestSafeWriter(t *testing.T) {
	var out bytes.Buffer

	n, err := safe(&out).Write([]byte("a\tb\n" + hostile + "c"))

	require.NoError(t, err)
	assert.Equal(t, len("a\tb\n"+hostile+"c"), n)
	assert.Equal(t, "a\tb\n[31mred]0;titleevilc", out.String(), "tabs and new lines stay, the controls go")

	w := safe(&out)
	assert.Same(t, w, safe(w), "one layer")
}

func TestServerTextIsCleanedOnTheTerminal(t *testing.T) {
	assertClean := func(t *testing.T, res result) {
		t.Helper()

		for _, bad := range []string{"\x1b", "\x07", "\x00", "\x7f", rlo, "\u009b"} {
			assert.NotContains(t, res.stdout, bad)
			assert.NotContains(t, res.stderr, bad)
		}
	}

	e := newEnv(t)
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
		op := testOperation("op7", dcctlv1.Operation_FAILED)
		op.Error = &dcctlv1.OperationError{Reason: "STEP_FAILED", Detail: hostile}
		op.Checks = []*dcctlv1.Check{testCheck("a.b", dcctlv1.Check_FAIL, dcctlv1.Check_BLOCK, hostile)}

		return op, nil
	}

	res := e.run("ops", "get", "op7")
	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stdout, "red")
	assertClean(t, res)

	e.api.status = func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
		return nil, status.Error(codes.Internal, hostile)
	}

	res = e.run("status")
	assert.Equal(t, ExitFailed, res.code)
	assertClean(t, res)

	e.api.watch = stream(false, func() *dcctlv1.Operation {
		op := opAt(dcctlv1.Operation_FAILED, dcctlv1.Operation_Step_FAILED)
		op.Error = &dcctlv1.OperationError{Reason: "X", Detail: hostile}
		op.Steps[0].Detail = hostile

		return op
	}())

	res = e.run("watch", "op7")
	assert.Equal(t, ExitFailed, res.code)
	assertClean(t, res)
}

// A character that two writes share is written once, whole.
func TestSafeWriterJoinsASplitCharacter(t *testing.T) {
	for _, text := range []string{"é", "日", "\U0001F468"} {
		for cut := 1; cut < len(text); cut++ {
			var out bytes.Buffer

			w := safe(&out)

			_, err := w.Write([]byte("a" + text[:cut]))
			require.NoError(t, err)
			_, err = w.Write([]byte(text[cut:] + "b"))
			require.NoError(t, err)

			assert.Equal(t, "a"+text+"b", out.String(), "%q cut at %d", text, cut)
		}
	}

	var out bytes.Buffer

	_, _ = safe(&out).Write([]byte("a\xffb"))

	assert.Equal(t, "a\ufffdb", out.String(), "a byte that is not UTF-8 is not held back")
}

// -o json is data: the characters of its strings are not filtered, and it
// parses back to what the server sent.
func TestJSONKeepsTheCharactersOfStrings(t *testing.T) {
	e := newEnv(t)

	st := testStatus()
	st.Alarms[0].Detail = "pg\u00adnode \U0001F468\u200d\U0001F4BB \u2066x\u2069 \x1b[1m"
	e.api.status = func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) { return st, nil }

	res := e.run("status", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)

	got := &dcctlv1.Status{}
	require.NoError(t, protoJSONUnmarshal([]byte(res.stdout), got))
	assert.Equal(t, st.GetAlarms()[0].GetDetail(), got.GetAlarms()[0].GetDetail())
}
