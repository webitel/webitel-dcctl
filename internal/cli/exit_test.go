package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func testApp(action cli.ActionFunc) *cli.App {
	app := &cli.App{
		Name:      "webitel-dcctl",
		Writer:    io.Discard,
		ErrWriter: io.Discard,
		Commands: []*cli.Command{
			{Name: "do", Flags: []cli.Flag{&cli.StringFlag{Name: "name"}}, Action: action},
			{Name: "group", Action: noCommand, Subcommands: []*cli.Command{{Name: "leaf", Action: action}}},
		},
	}
	Setup(app)

	return app
}

func runApp(app *cli.App, args ...string) (int, string) {
	err := app.Run(append([]string{"webitel-dcctl"}, args...))

	var out bytes.Buffer

	code := Report(&out, err)

	return code, out.String()
}

func TestUsageErrorsAreNotInterrupted(t *testing.T) {
	ok := func(*cli.Context) error { return nil }

	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"nope"}},
		{"unknown flag", []string{"do", "--nope"}},
		{"flag without value", []string{"do", "--name"}},
		{"unknown subcommand", []string{"group", "nope"}},
		{"help of an unknown topic", []string{"help", "nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out := runApp(testApp(ok), tt.args...)

			assert.Equal(t, ExitUsage, code)
			assert.NotEqual(t, ExitInterrupted, code)
			assert.NotEmpty(t, out)
		})
	}
}

func TestNoCommandShowsHelp(t *testing.T) {
	var out bytes.Buffer

	app := testApp(nil)
	app.Writer = &out

	code, _ := runApp(app)

	assert.Equal(t, ExitOK, code)
	assert.Contains(t, out.String(), "COMMANDS")
}

func TestExitCodeOfCommandErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, ExitOK},
		{"warnings", exit(ExitWarnings), ExitWarnings},
		{"interrupted", exit(ExitInterrupted), ExitInterrupted},
		{"plain error", errors.New("boom"), ExitFailed},
		{"urfave exit code", cli.Exit("x", 3), ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := runApp(testApp(func(*cli.Context) error { return tt.err }), "do")

			assert.Equal(t, tt.want, code)
		})
	}
}

// A refusal is told apart by its reason only; no reason maps to the code of an
// operation state or to a usage error.
func TestExitCodeOfReasons(t *testing.T) {
	for _, r := range apierr.Reasons() {
		t.Run(string(r), func(t *testing.T) {
			err := apierr.New(r, "a message that says interrupted, usage and unknown command")

			assert.Equal(t, ExitFailed, ExitCode(err))
		})
	}
}

func TestReportMessages(t *testing.T) {
	var out bytes.Buffer

	err := apierr.Status(apierr.ReasonOperationRunning, "operation is running",
		map[string]string{"operation": "operations/op7"}).Err()

	require.Equal(t, ExitFailed, Report(&out, err))
	assert.Contains(t, out.String(), "operation is running [OPERATION_RUNNING]")
	assert.Contains(t, out.String(), "webitel-dcctl watch op7")

	out.Reset()
	assert.Equal(t, ExitFailed, Report(&out, apierr.New(apierr.ReasonAuthenticationRequired, "no")))
	assert.Contains(t, out.String(), "authentication failed")

	out.Reset()
	assert.Equal(t, ExitInterrupted, Report(&out, exit(ExitInterrupted)))
	assert.Empty(t, out.String())
}
