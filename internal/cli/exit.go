package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/urfave/cli/v2"
)

// Exit codes of the CLI commands.
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitWarnings    = 2
	ExitInterrupted = 3
	ExitUsage       = 64
	ExitCanceled    = 130
)

// Error is a command failure with its exit code. An empty Err means the
// command already printed what there is to say.
type Error struct {
	Code int
	Err  error
	// JSON is set when the command was asked for -o json, so Report prints the
	// error as a JSON object.
	JSON bool
}

func (e *Error) Error() string {
	if e.Err == nil {
		return ""
	}

	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func exit(code int) error { return &Error{Code: code} }

func usageErrorf(format string, args ...any) error {
	return &Error{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

// ExitCode returns the exit code of a command error. An error that urfave/cli
// made itself, such as the help of an unknown topic, is a usage error: its
// own code 3 would read as an interrupted operation.
func ExitCode(err error) int {
	var e *Error

	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &e):
		return e.Code
	}

	var coder cli.ExitCoder
	if errors.As(err, &coder) {
		return ExitUsage
	}

	return ExitFailed
}

// Report writes err to w as the CLI shows it and returns the exit code.
func Report(w io.Writer, err error) int {
	if err == nil {
		return ExitOK
	}

	w = safe(w)
	code := ExitCode(err)

	var e *Error

	asJSON := errors.As(err, &e) && e.JSON

	msg := describe(err)
	if msg == "" {
		return code
	}

	switch {
	case asJSON:
		_, _ = fmt.Fprintln(w, errorJSON(err))
	case code == ExitUsage:
		_, _ = fmt.Fprintf(w, "usage error: %s\nRun the command with --help for its usage.\n", msg)
	default:
		_, _ = fmt.Fprintf(w, "error: %s\n", strings.TrimRight(msg, "\n"))
	}

	return code
}
