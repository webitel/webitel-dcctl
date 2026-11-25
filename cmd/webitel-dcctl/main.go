package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/webitel/webitel-go-kit/infra/health"
	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/api"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/runtime"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// healthSystemDcctl is the health system of the checks of dcctl itself.
const healthSystemDcctl health.System = "dcctl"

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

func runServe(ctx context.Context, cfg *config.Serve) (err error) {
	clients, err := consulClients(cfg)
	if err != nil {
		return err
	}

	httpOpts, peerOpts, err := auth.ServerOptions(ctx, cfg.Auth.Users, clients, logging.New(logging.WithComponent("dcctl.auth")))
	if err != nil {
		return err
	}

	agent := runtime.Consul{Datacenter: cfg.Datacenter, NodeMetaKey: cfg.Consul.NodeMetaKey}

	rt, err := runtime.New(runtime.Config{
		HTTP:        cfg.Listen.HTTP,
		Peer:        cfg.Listen.Peer,
		Health:      cfg.Listen.Health,
		TLS:         cfg.TLS,
		HTTPOptions: httpOpts,
		PeerOptions: peerOpts,
		APIGuard:    auth.CheckAPIRequest,
		Consul:      &agent,
	}, logging.New(logging.WithComponent("dcctl.runtime")))
	if err != nil {
		return err
	}

	var arb *arbiter.Client

	if cfg.Arbiter.Address != "" {
		arb, err = arbiter.NewClient(arbiter.ClientOptions{
			Address: cfg.Arbiter.Address,
			TLS:     cfg.TLS,
			Timeout: cfg.Status.RequestTimeout,
		})
		if err != nil {
			return err
		}

		defer func() { err = errors.Join(err, arb.Close()) }()
	}

	log := logging.New(logging.WithComponent("dcctl.collect"))

	self, err := instance(ctx, cfg.Datacenter, agent, log)
	if err != nil {
		return err
	}

	collector, err := collect.FromConfig(cfg, self, arb, collect.ReadsStale, log)
	if err != nil {
		return err
	}

	rt.Health().Liveness(healthSystemDcctl, collector.Check, health.WithName("collector"))

	apiOpts, err := readAPI(cfg, clients, collector)
	if err != nil {
		return err
	}

	if cfg.UI.Dir != "" {
		root, rootErr := os.OpenRoot(cfg.UI.Dir)
		if rootErr != nil {
			return fmt.Errorf("ui.dir: %w", rootErr)
		}

		defer func() { err = errors.Join(err, root.Close()) }()

		apiOpts.UI = root.FS()
	}

	if err := api.Register(ctx, rt, apiOpts); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	wg.Go(func() { collector.Run(ctx) })

	err = rt.Run(ctx)

	cancel()
	wg.Wait()

	return err
}

// consulClients returns the Consul clients of serve that decide on what they
// read, so without stale reads: the instance check of auth, dcctl/op and the
// journal. The collector reads through clients of its own.
func consulClients(cfg *config.Serve) (*consul.Clients, error) {
	clients, err := consul.New(consul.Options{
		Profile:     cfg.Profile,
		NodeMetaKey: cfg.Consul.NodeMetaKey,
		Servers:     cfg.Consul.Servers,
		SessionTTL:  cfg.Coordination.SessionTTL,
		LockDelay:   cfg.Coordination.LockDelay,
		Timeout:     cfg.Status.RequestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("consul: %w", err)
	}

	return clients, nil
}

// readAPI returns what the API reads from: the status of collector, dcctl/op
// and the journal in the KV of every voter, and listen.peer as the observer
// address. Nothing provides alarms and verify checks yet.
func readAPI(cfg *config.Serve, clients *consul.Clients, collector *collect.Collector) (api.Options, error) {
	named, err := coord.NamedClients(cfg.Profile, clients)
	if err != nil {
		return api.Options{}, err
	}

	kvs, err := coord.NewKVs(cfg.Profile, clients)
	if err != nil {
		return api.Options{}, err
	}

	ops, err := coord.NewOpStore(kvs, cfg.Status.RequestTimeout, logging.New(logging.WithComponent("dcctl.op")))
	if err != nil {
		return api.Options{}, fmt.Errorf("operations: %w", err)
	}

	j, err := journal.New(journal.KVs(named), journal.Options{
		MaxEntries: cfg.Journal.MaxEntries,
		Timeout:    cfg.Status.RequestTimeout,
	}, logging.New(logging.WithComponent("dcctl.journal")))
	if err != nil {
		return api.Options{}, fmt.Errorf("journal: %w", err)
	}

	return api.Options{
		Address:    cfg.Listen.Peer,
		Status:     api.FromCollector(collector, status.ConfigFrom(cfg)),
		Operations: ops,
		Journal:    j,
		Log:        logging.New(logging.WithComponent("dcctl.api")),
	}, nil
}

// instance names this dcctl instance as the node of the local Consul agent, which is
// where it is in the catalog, or as the hostname when the agent does not tell.
func instance(ctx context.Context, datacenter string, agent runtime.Consul, log *logging.Logger) (collect.Instance, error) {
	node, err := agent.NodeName(ctx)
	if err == nil {
		log.InfoCtx(ctx, "instance is named by the Consul node", logging.Str("node", node))

		return collect.Instance{Datacenter: datacenter, Node: node}, nil
	}

	host, herr := os.Hostname()
	if herr != nil {
		return collect.Instance{}, fmt.Errorf("name the instance: %w", errors.Join(err, herr))
	}

	log.WarnCtx(ctx, "instance is named by the hostname, the Consul node is unknown", logging.Str("node", host), logging.Err(err))

	return collect.Instance{Datacenter: datacenter, Node: host}, nil
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
