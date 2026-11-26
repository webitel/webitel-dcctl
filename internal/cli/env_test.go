package cli

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testUser     = "operator"
	testPassword = "s3cret-pass"
)

func errUnimplemented(method string) error {
	return status.Errorf(codes.Unimplemented, "method %s not implemented", method)
}

// env is a CLI against a fake API, with the credentials in the environment.
type env struct {
	t     *testing.T
	api   *fakeAPI
	addr  string
	stdin string
	opts  Options
}

type result struct {
	code   int
	stdout string
	stderr string
}

func newEnv(t *testing.T) *env {
	t.Helper()

	api, addr := startFake(t)

	t.Setenv("DCCTL_SERVER", addr)
	t.Setenv("DCCTL_USER", testUser)
	t.Setenv(passwordEnv, testPassword)

	return &env{t: t, api: api, addr: addr, opts: Options{
		ConfigPath: filepath.Join(t.TempDir(), "none.yaml"),
		RetryDelay: time.Millisecond,
		ReadPassword: func(string, io.Writer) (string, error) {
			return "", errors.New("the test has no terminal")
		},
	}}
}

func (e *env) app(out, errOut *bytes.Buffer) *cli.App {
	app := &cli.App{
		Name:      "webitel-dcctl",
		Reader:    strings.NewReader(e.stdin),
		Writer:    out,
		ErrWriter: errOut,
		Commands:  Commands(e.opts),
	}
	Setup(app)

	return app
}

func (e *env) run(args ...string) result {
	e.t.Helper()

	var out, errOut bytes.Buffer

	err := e.app(&out, &errOut).Run(append([]string{"webitel-dcctl"}, args...))
	code := Report(&errOut, err)

	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}
