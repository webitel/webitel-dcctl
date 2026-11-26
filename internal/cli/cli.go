package cli

import (
	"github.com/urfave/cli/v2"
)

// Commands returns the CLI commands of webitel-dcctl. They call the API of a
// running dcctl; serve and arbiter are not among them.
func Commands(o Options) []*cli.Command {
	o = o.withDefaults()

	return []*cli.Command{
		verifyCommand(o),
	}
}
