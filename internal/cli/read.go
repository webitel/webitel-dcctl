package cli

import (
	"context"

	"github.com/urfave/cli/v2"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

func verifyCommand(o Options) *cli.Command {
	return &cli.Command{
		Name:   "verify",
		Usage:  "Check the conditions of a finished switch of the active datacenter",
		Flags:  connFlags(),
		Before: noArgs,
		Action: o.run(func(ctx context.Context, _ *cli.Context, s *session) error {
			resp, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.VerifyStatusResponse, error) {
				return s.status.VerifyStatus(ctx, &dcctlv1.VerifyStatusRequest{})
			})
			if err != nil {
				return err
			}

			if s.asJSON {
				err = s.printJSON(resp)
			} else {
				printChecks(s.out, resp.GetChecks())
			}

			if err != nil {
				return err
			}

			for _, c := range resp.GetChecks() {
				if c.GetResult() == dcctlv1.Check_FAIL {
					return exit(ExitFailed)
				}
			}

			return nil
		}),
	}
}

// noArgs rejects arguments for a command that takes none.
func noArgs(c *cli.Context) error {
	if c.Args().Present() {
		return usageErrorf("%s takes no arguments, got %q", c.Command.Name, c.Args().First())
	}

	return nil
}
