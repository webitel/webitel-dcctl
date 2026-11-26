package cli

import (
	"github.com/urfave/cli/v2"
)

// Setup makes app report usage errors with ExitUsage. urfave/cli answers an
// unknown command with exit code 3, which is INTERRUPTED here, so the app
// handles it itself, and every usage error is returned to the caller, which
// turns it into an exit code with [Report].
func Setup(app *cli.App) {
	app.ExitErrHandler = func(*cli.Context, error) {}
	app.Action = noCommand

	setupUsage(app.Commands)
	app.OnUsageError = onUsageError
}

func setupUsage(cmds []*cli.Command) {
	for _, c := range cmds {
		if c.OnUsageError == nil {
			c.OnUsageError = onUsageError
		}

		setupUsage(c.Subcommands)
	}
}

func onUsageError(_ *cli.Context, err error, _ bool) error {
	return usageErrorf("%v", err)
}

// noCommand is the action of the app and of a command group, which run when
// no subcommand matches the arguments.
func noCommand(c *cli.Context) error {
	if c.Args().Present() {
		return usageErrorf("unknown command %q", c.Args().First())
	}

	return cli.ShowSubcommandHelp(c)
}
