package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

func basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func (e *env) verifyOK() {
	e.api.verify = func(*dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error) {
		return &dcctlv1.VerifyStatusResponse{}, nil
	}
}

func writeSecret(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))

	return path
}

func TestPasswordFromEnv(t *testing.T) {
	e := newEnv(t)
	e.verifyOK()

	res := e.run("verify")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.Equal(t, []string{basic(testUser, testPassword)}, e.api.authorizations())
}

func TestPasswordFileBeatsEnv(t *testing.T) {
	e := newEnv(t)
	e.verifyOK()

	path := writeSecret(t, "from-the-file\n", 0o600)

	res := e.run("verify", "--password-file", path)

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.Equal(t, []string{basic(testUser, "from-the-file")}, e.api.authorizations())
}

func TestPasswordFileIsChecked(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
	}{
		{"accessible by others", "pw", 0o644, "accessible by others"},
		{"empty", "\n", 0o600, "is empty"},
		{"too long", strings.Repeat("a", 5000), 0o600, "longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.verifyOK()

			res := e.run("verify", "--password-file", writeSecret(t, tt.content, tt.mode))

			assert.Equal(t, ExitFailed, res.code)
			assert.Contains(t, res.stderr, tt.want)
			assert.Empty(t, e.api.methods(), "no call without a password")
		})
	}

	for name, path := range map[string]string{
		"missing":     filepath.Join(t.TempDir(), "none"),
		"a directory": t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.verifyOK()

			res := e.run("verify", "--password-file", path)

			assert.Equal(t, ExitFailed, res.code)
			assert.Contains(t, res.stderr, "password file")
			assert.Empty(t, e.api.methods())
		})
	}
}

func TestPasswordPrompt(t *testing.T) {
	e := newEnv(t)
	e.verifyOK()
	t.Setenv(passwordEnv, "")

	var prompt string

	e.opts.ReadPassword = func(p string, _ io.Writer) (string, error) {
		prompt = p

		return "typed", nil
	}

	res := e.run("verify")

	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.Equal(t, "Password for operator: ", prompt)
	assert.Equal(t, []string{basic(testUser, "typed")}, e.api.authorizations())
}

func TestNoPassword(t *testing.T) {
	e := newEnv(t)
	t.Setenv(passwordEnv, "")

	res := e.run("verify")

	assert.Equal(t, ExitFailed, res.code)
	assert.Empty(t, e.api.methods())
}

func TestNoUser(t *testing.T) {
	e := newEnv(t)
	t.Setenv("DCCTL_USER", "")

	res := e.run("verify")

	assert.Equal(t, ExitUsage, res.code)
	assert.Contains(t, res.stderr, "no user")

	res = e.run("verify", "--user", "a:b")

	assert.Equal(t, ExitUsage, res.code)
	assert.Contains(t, res.stderr, "colon")
}

// The password is never an argument: no command has a flag for it, and a
// guess at its name is a usage error that does not reach the server.
func TestNoPasswordFlag(t *testing.T) {
	var walk func(cmds []*cli.Command)

	walk = func(cmds []*cli.Command) {
		for _, cmd := range cmds {
			for _, f := range cmd.Flags {
				for _, name := range f.Names() {
					assert.NotContains(t, []string{"password", "pass", "passwd", "pw", "p"}, name,
						"%s has the flag --%s", cmd.Name, name)
				}
			}

			walk(cmd.Subcommands)
		}
	}

	walk(Commands(Options{}))

	e := newEnv(t)
	e.verifyOK()

	for _, args := range [][]string{
		{"verify", "--password", "x"},
		{"verify", "--password=x"},
		{"verify", "-p", "x"},
		{"switch", "dc_b", "--password", "x"},
	} {
		res := e.run(args...)

		assert.Equal(t, ExitUsage, res.code, args)
		assert.NotContains(t, res.stdout+res.stderr, "x\n")
	}

	assert.Empty(t, e.api.methods())
}

func TestPlaintextOnlyOnLoopback(t *testing.T) {
	e := newEnv(t)
	e.verifyOK()
	t.Setenv("DCCTL_SERVER", "192.0.2.10:10022")

	res := e.run("verify")

	assert.Equal(t, ExitUsage, res.code)
	assert.Contains(t, res.stderr, "not sent without TLS")

	res = e.run("verify", "--server", "not-an-address")

	assert.Equal(t, ExitUsage, res.code)

	res = e.run("verify", "--server", "somewhere.invalid:10022")

	assert.Equal(t, ExitUsage, res.code, "a name is not trusted to be this host")
}

func TestServerFromConfig(t *testing.T) {
	e := newEnv(t)
	e.verifyOK()
	t.Setenv("DCCTL_SERVER", "")

	_, port, err := net.SplitHostPort(e.addr)
	require.NoError(t, err)

	e.opts.ConfigPath = filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(e.opts.ConfigPath,
		fmt.Appendf(nil, "datacenter: dc_a\nlisten:\n  http: 0.0.0.0:%s\n  peer: 10.1.0.81:10023\n", port), 0o600))

	res := e.run("verify")

	assert.Equal(t, ExitOK, res.code, res.stderr)

	require.NoError(t, os.WriteFile(e.opts.ConfigPath, []byte("listen: [\n"), 0o600))

	res = e.run("verify")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "parse")
}

func TestTLS(t *testing.T) {
	ca := testpki.New(t, "test ca")
	files := ca.Files(t, "127.0.0.1")

	api := &fakeAPI{}
	api.verify = func(*dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error) {
		return &dcctlv1.VerifyStatusResponse{}, nil
	}

	cert := ca.Leaf(t, "127.0.0.1")

	srv := grpc.NewServer(grpc.Creds(grpccreds.NewServerTLSFromCert(&cert)))
	dcctlv1.RegisterStatusServiceServer(srv, api)

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = srv.Serve(ln)
	}()

	t.Cleanup(func() {
		srv.Stop()
		<-done
	})

	e := newEnv(t)
	t.Setenv("DCCTL_SERVER", ln.Addr().String())

	res := e.run("verify", "--tls-ca", files.CA)
	assert.Equal(t, ExitOK, res.code, res.stderr)

	res = e.run("verify", "--tls-ca", filepath.Join(t.TempDir(), "none.pem"))
	assert.Equal(t, ExitFailed, res.code)

	bad := writeSecret(t, "not a certificate", 0o600)
	res = e.run("verify", "--tls-ca", bad)
	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "no certificate")

	// The server certificate is not signed by the system roots.
	res = e.run("verify", "--tls")
	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "not reachable")
}

func TestLoopbackAddrs(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:1", "[::1]:1", "127.0.0.2:1", "[::ffff:127.0.0.1]:1", "localhost:1"} {
		got, ok := loopbackAddrs(t.Context(), addr)

		require.True(t, ok, addr)

		for _, a := range got {
			assert.NotContains(t, a, "localhost", "the addresses that were checked are the ones dialed")
		}
	}

	for _, addr := range []string{"0.0.0.0:1", "[::]:1", "10.0.0.1:1", "[fe80::1%lo0]:1", "host.invalid:1", "127.0.0.1", ""} {
		_, ok := loopbackAddrs(t.Context(), addr)

		assert.False(t, ok, addr)
	}
}

// localhost may resolve to ::1 first, and the server listens on 127.0.0.1.
func TestLocalhostToAnIPv4Server(t *testing.T) {
	e := newEnv(t)
	e.verifyOK()

	_, port, err := net.SplitHostPort(e.addr)
	require.NoError(t, err)

	t.Setenv("DCCTL_SERVER", "localhost:"+port)

	res := e.run("verify")

	assert.Equal(t, ExitOK, res.code, res.stderr)
}

func TestPasswordFileModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")

	require.NoError(t, os.WriteFile(path, []byte("x\n"), 0o600))

	for mode, ok := range map[os.FileMode]bool{0o600: true, 0o640: true, 0o601: false, 0o604: false, 0o644: false} {
		require.NoError(t, os.Chmod(path, mode))

		_, err := readPasswordFile(path)

		assert.Equal(t, ok, err == nil, "%o", mode)
	}

	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("y"), 0o600))

	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	pw, err := readPasswordFile(link)
	require.NoError(t, err)
	assert.Equal(t, "y", pw, "the target of a link is checked and read")
}
