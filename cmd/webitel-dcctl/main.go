package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/runtime"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

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
				Action: runArbiter,
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

func serve(c *cli.Context) error {
	cfg, err := config.LoadServe(c.String("config"))
	if err != nil {
		return err
	}

	return runDaemon(c, func(ctx context.Context) error {
		return runServe(ctx, cfg)
	})
}

func runServe(ctx context.Context, cfg *config.Serve) error {
	httpOpts, peerOpts, err := serverOptions(ctx, cfg)
	if err != nil {
		return err
	}

	rt, err := runtime.New(runtime.Config{
		HTTP:        cfg.Listen.HTTP,
		Peer:        cfg.Listen.Peer,
		Health:      cfg.Listen.Health,
		TLS:         cfg.TLS,
		HTTPOptions: httpOpts,
		PeerOptions: peerOpts,
		APIGuard:    auth.CheckAPIRequest,
		Consul:      &runtime.Consul{Datacenter: cfg.Datacenter, NodeMetaKey: cfg.Consul.NodeMetaKey},
	}, logging.New(logging.WithComponent("dcctl.runtime")))
	if err != nil {
		return err
	}

	return rt.Run(ctx)
}

// serverOptions returns the interceptors of the listeners, with the dcctl
// instances listed by the Consul servers of cfg.
func serverOptions(ctx context.Context, cfg *config.Serve) (httpOpts, peerOpts []grpc.ServerOption, err error) {
	catalog, err := consul.New(consul.Options{
		Profile:     cfg.Profile,
		NodeMetaKey: cfg.Consul.NodeMetaKey,
		Servers:     cfg.Consul.Servers,
		SessionTTL:  cfg.Coordination.SessionTTL,
		LockDelay:   cfg.Coordination.LockDelay,
		Timeout:     cfg.Status.RequestTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("consul: %w", err)
	}

	return auth.ServerOptions(ctx, cfg.Auth.Users, catalog, logging.New(logging.WithComponent("dcctl.auth")))
}

func runArbiter(c *cli.Context) error {
	cfg, err := config.LoadArbiter(c.String("config"))
	if err != nil {
		return err
	}

	return runDaemon(c, func(ctx context.Context) error {
		return arbiter.Run(ctx, cfg, logging.New(logging.WithComponent("dcctl.arbiter")))
	})
}

// runDaemon runs fn until SIGINT or SIGTERM, with OpenTelemetry set up
// around it.
func runDaemon(c *cli.Context, fn func(context.Context) error) (err error) {
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

	return fn(ctx)
}

func printVersion(c *cli.Context) error {
	_, err := fmt.Fprintln(c.App.Writer, buildinfo.Version)

	return err
}
