package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// session is what a command runs with: the connection and the terminal.
type session struct {
	o      Options
	conn   *grpc.ClientConn
	out    io.Writer
	errOut io.Writer
	in     *bufio.Reader
	asJSON bool

	status      dcctlv1.StatusServiceClient
	datacenters dcctlv1.DatacenterServiceClient
	operations  dcctlv1.OperationServiceClient
	journal     dcctlv1.JournalServiceClient
}

// run returns the action of a command: it checks the output format, gets the
// credentials, connects, and calls fn. The credentials are asked for after
// the arguments are checked, which the Before of the command does.
func (o Options) run(fn func(ctx context.Context, c *cli.Context, s *session) error) cli.ActionFunc {
	return func(c *cli.Context) (err error) {
		asJSON := c.String(flagOutput) == outputJSON

		defer func() {
			if err != nil && asJSON {
				err = jsonError(err)
			}
		}()

		switch c.String(flagOutput) {
		case outputTable, outputJSON:
		default:
			return usageErrorf("--%s must be %s or %s", flagOutput, outputTable, outputJSON)
		}

		creds, err := resolveCredentials(c, o)
		if err != nil {
			return err
		}

		conn, err := dial(c, o, creds)
		if err != nil {
			return err
		}

		defer func() { _ = conn.Close() }()

		ctx, stop := signal.NotifyContext(c.Context, os.Interrupt, syscall.SIGTERM)
		defer stop()

		in := c.App.Reader
		if in == nil {
			in = os.Stdin
		}

		s := &session{
			o: o, conn: conn, out: c.App.Writer, errOut: c.App.ErrWriter, in: bufio.NewReader(in), asJSON: asJSON,
			status:      dcctlv1.NewStatusServiceClient(conn),
			datacenters: dcctlv1.NewDatacenterServiceClient(conn),
			operations:  dcctlv1.NewOperationServiceClient(conn),
			journal:     dcctlv1.NewJournalServiceClient(conn),
		}

		return fn(ctx, c, s)
	}
}

// jsonError marks err to be printed as JSON, keeping its exit code.
func jsonError(err error) error {
	var e *Error
	if errors.As(err, &e) {
		e.JSON = true

		return e
	}

	return &Error{Code: ExitCode(err), Err: err, JSON: true}
}

// call runs one unary call with the call timeout.
func call[T any](ctx context.Context, s *session, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, s.o.CallTimeout)
	defer cancel()

	return fn(ctx)
}

// warnf writes a warning that is not part of the output: stderr.
func (s *session) warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.errOut, "warning: "+format+"\n", args...)
}
