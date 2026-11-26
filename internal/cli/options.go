package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

// Options are the parts of the environment the commands depend on; the zero
// value is the real environment.
type Options struct {
	// LookupEnv reads the password variable. It is os.LookupEnv by default.
	LookupEnv func(key string) (string, bool)
	// ReadPassword asks for the password without echo, writing prompt to w.
	// By default it reads from the terminal of the process and fails without
	// one.
	ReadPassword func(prompt string, w io.Writer) (string, error)
	// ConfigPath is the serve config the default server address is read from.
	ConfigPath string
	// CallTimeout bounds a call that is not a watch. It is 30 s by default.
	CallTimeout time.Duration
	// RetryDelay is the wait before a call that failed on the way is repeated
	// and before a watch reconnects. It is 2 s by default.
	RetryDelay time.Duration
}

func (o Options) withDefaults() Options {
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}

	if o.ReadPassword == nil {
		o.ReadPassword = terminalPassword
	}

	if o.CallTimeout == 0 {
		o.CallTimeout = 30 * time.Second
	}

	if o.RetryDelay == 0 {
		o.RetryDelay = 2 * time.Second
	}

	return o
}

func terminalPassword(prompt string, w io.Writer) (string, error) {
	fd := int(os.Stdin.Fd()) // #nosec G115 -- a file descriptor is a small number.
	if !term.IsTerminal(fd) {
		return "", errNoPassword
	}

	state, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("read the password: %w", err)
	}

	// An interrupt must give the terminal back with its echo on.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(sig)

	_, _ = fmt.Fprint(w, prompt)

	type answer struct {
		pw  []byte
		err error
	}

	read := make(chan answer, 1)

	go func() {
		pw, err := term.ReadPassword(fd)
		read <- answer{pw, err}
	}()

	select {
	case <-sig:
		_ = term.Restore(fd, state)

		_, _ = fmt.Fprintln(w)

		return "", &Error{Code: ExitCanceled, Err: errors.New("canceled")}
	case a := <-read:
		_, _ = fmt.Fprintln(w)

		if a.err != nil {
			return "", fmt.Errorf("read the password: %w", a.err)
		}

		return string(a.pw), nil
	}
}

// Flag names shared by the commands.
const (
	flagServer       = "server"
	flagUser         = "user"
	flagPasswordFile = "password-file"
	flagTLS          = "tls"
	flagTLSCA        = "tls-ca"
	flagOutput       = "output"
)

// connFlags are the flags every command that calls the API takes. There is no
// flag for the password: an argument is visible to every user of the host.
func connFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: flagServer, Usage: "address of listen.http of dcctl (default: from the serve config, else 127.0.0.1:10022)", EnvVars: []string{"DCCTL_SERVER"}},
		&cli.StringFlag{Name: flagUser, Usage: "user name", EnvVars: []string{"DCCTL_USER"}},
		&cli.StringFlag{Name: flagPasswordFile, Usage: "file with the password (the password can also be in DCCTL_PASSWORD or asked on the terminal)"},
		&cli.BoolFlag{Name: flagTLS, Usage: "connect with TLS, trusting the system roots"},
		&cli.StringFlag{Name: flagTLSCA, Usage: "connect with TLS, trusting the CA in this file"},
		&cli.StringFlag{Name: flagOutput, Aliases: []string{"o"}, Usage: "output format: table or json", Value: outputTable},
	}
}

// Output formats.
const (
	outputTable = "table"
	outputJSON  = "json"
)
