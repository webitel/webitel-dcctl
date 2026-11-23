package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"
)

var errNotImplemented = errors.New("not implemented")

func main() {
	if err := newApp().Run(os.Args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}

func newApp() *cli.App {
	return &cli.App{
		Name:  "webitel-dcctl",
		Usage: "Control which datacenter of a Webitel installation is active",
		Commands: []*cli.Command{
			{
				Name:   "serve",
				Usage:  "Run the dcctl daemon",
				Action: notImplemented,
			},
			{
				Name:   "arbiter",
				Usage:  "Run the arbiter daemon",
				Action: notImplemented,
			},
			{
				Name:   "version",
				Usage:  "Print the version",
				Action: printVersion,
			},
		},
	}
}

func notImplemented(c *cli.Context) error {
	return fmt.Errorf("%s: %w", c.Command.Name, errNotImplemented)
}

func printVersion(c *cli.Context) error {
	_, err := fmt.Fprintln(c.App.Writer, buildinfo.Version)

	return err
}
