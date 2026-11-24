package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/runtime"
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
				Flags:  []cli.Flag{configFlag(config.DefaultServePath)},
				Action: serve,
			},
			{
				Name:   "arbiter",
				Usage:  "Run the arbiter daemon",
				Flags:  []cli.Flag{configFlag(config.DefaultArbiterPath)},
				Action: arbiter,
			},
			{
				Name:   "version",
				Usage:  "Print the version",
				Action: printVersion,
			},
		},
	}
}

func configFlag(path string) *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "config",
		Aliases: []string{"c"},
		Usage:   "Path to the config file",
		Value:   path,
	}
}

func serve(c *cli.Context) (err error) {
	cfg, err := config.LoadServe(c.String("config"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(c.Context, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// After the first signal the default handling returns, so a second one
	// kills a stuck shutdown.
	context.AfterFunc(ctx, stop)

	shutdownTelemetry, err := setupTelemetry(ctx, c.Command.Name)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, shutdownTelemetry()) }()

	rt, err := runtime.New(runtime.Config{
		HTTP:   cfg.Listen.HTTP,
		Peer:   cfg.Listen.Peer,
		Health: cfg.Listen.Health,
		TLS:    cfg.TLS,
		Consul: &runtime.Consul{Datacenter: cfg.Datacenter, NodeMetaKey: cfg.Consul.NodeMetaKey},
	}, logging.New(logging.WithComponent("dcctl.runtime")))
	if err != nil {
		return err
	}

	return rt.Run(ctx)
}

func arbiter(c *cli.Context) error {
	if _, err := config.LoadArbiter(c.String("config")); err != nil {
		return err
	}

	return fmt.Errorf("%s: %w", c.Command.Name, errNotImplemented)
}

func printVersion(c *cli.Context) error {
	_, err := fmt.Fprintln(c.App.Writer, buildinfo.Version)

	return err
}
