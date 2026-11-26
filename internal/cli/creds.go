package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/urfave/cli/v2"
)

// passwordEnv holds the password when it is not in a file.
const passwordEnv = "DCCTL_PASSWORD"

// maxPasswordFile bounds what is read from the password file.
const maxPasswordFile = 4 << 10

var errNoPassword = errors.New("no password: set " + passwordEnv + ", pass --" + flagPasswordFile +
	", or run the command in a terminal to be asked")

// credentials are the basic auth credentials of the operator. They go to the
// server in the authorization metadata of every call and nowhere else.
type credentials struct {
	user     string
	password string
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c credentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.password))

	return map[string]string{"authorization": "Basic " + token}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials. The
// connection is checked by [dial]: plaintext is only for the loopback
// address, where listen.http listens by default.
func (credentials) RequireTransportSecurity() bool { return false }

// resolveCredentials returns the user and the password of the command: the
// user from --user or DCCTL_USER, the password from --password-file, else
// DCCTL_PASSWORD, else a prompt on the terminal.
func resolveCredentials(c *cli.Context, o Options) (credentials, error) {
	user := c.String(flagUser)
	if user == "" {
		return credentials{}, usageErrorf("no user: pass --%s or set DCCTL_USER", flagUser)
	}

	if strings.Contains(user, ":") {
		return credentials{}, usageErrorf("the user name must not contain a colon")
	}

	pw, err := resolvePassword(c, o, user)
	if err != nil {
		return credentials{}, err
	}

	return credentials{user: user, password: pw}, nil
}

func resolvePassword(c *cli.Context, o Options, user string) (string, error) {
	if path := c.String(flagPasswordFile); path != "" {
		return readPasswordFile(path)
	}

	if pw, ok := o.LookupEnv(passwordEnv); ok && pw != "" {
		return pw, nil
	}

	pw, err := o.ReadPassword(fmt.Sprintf("Password for %s: ", user), c.App.ErrWriter)
	if err != nil {
		return "", err
	}

	if pw == "" {
		return "", errors.New("the password is empty")
	}

	return pw, nil
}

// readPasswordFile reads the password from path: a regular file that has no
// permission for others. A file the group can read is allowed: the service
// group reads its own files. One trailing newline is not part of the password.
func readPasswordFile(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator names their own password file.
	if err != nil {
		return "", fmt.Errorf("password file: %w", err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("password file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("password file %s is not a regular file", path)
	}

	if info.Mode().Perm()&fs.FileMode(0o007) != 0 {
		return "", fmt.Errorf("password file %s is accessible by others: run chmod 600 on it", path)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxPasswordFile+1))
	if err != nil {
		return "", fmt.Errorf("password file: %w", err)
	}

	if len(data) > maxPasswordFile {
		return "", fmt.Errorf("password file %s is longer than %d bytes", path, maxPasswordFile)
	}

	pw := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if pw == "" {
		return "", fmt.Errorf("password file %s is empty", path)
	}

	return pw, nil
}
