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
	"go.opentelemetry.io/otel"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/api"
	"github.com/webitel/webitel-dcctl/internal/apps"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/runtime"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
	"github.com/webitel/webitel-dcctl/internal/telemetry"
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

	j, err := newJournal(cfg, clients)
	if err != nil {
		return err
	}

	tracker, err := status.NewTracker(status.TrackerConfigFrom(cfg), j, logging.New(logging.WithComponent("dcctl.status")))
	if err != nil {
		return err
	}

	telemetry.RegisterHealth(rt.Health(), collector.Check, tracker, telemetry.HealthConfig{
		Datacenter: cfg.Datacenter,
		Profile:    cfg.Profile,
		Arbiter:    arb != nil,
	})

	metrics, err := telemetry.RegisterMetrics(otel.GetMeterProvider(), tracker, collect.StaleAfter(cfg.Status))
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, metrics.Unregister()) }()

	kvs, err := coord.NewKVs(cfg.Profile, clients)
	if err != nil {
		return err
	}

	ops, err := coord.NewOpStore(kvs, cfg.Status.RequestTimeout, logging.New(logging.WithComponent("dcctl.op")))
	if err != nil {
		return fmt.Errorf("operations: %w", err)
	}

	coordinator, err := newCoordinator(cfg, clients, arb)
	if err != nil {
		return err
	}

	engine, checks, planners, err := newEngine(cfg, operationDeps{
		self: self, kvs: kvs, ops: ops, coordinator: coordinator, journal: j,
		clients: clients, arb: arb, last: collector.Snapshot,
	})
	if err != nil {
		return err
	}

	peers, err := api.NewPeerClient(cfg.TLS,
		api.ForwardTimeout(cfg.Status.RequestTimeout, preflight.ConfigFrom(cfg).Timeout, len(kvs)), cfg.Status.RequestTimeout)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, peers.Close()) }()

	apiOpts := readAPI(cfg, ops, j, tracker)
	apiOpts.Events = api.FromEngine(engine)
	apiOpts.Actions = &api.Actions{
		Self:        operation.Instance{Node: self.Node, Datacenter: self.Datacenter, Address: cfg.Listen.Peer},
		Datacenters: clients.Datacenters(),
		Engine:      engine,
		Preflight:   checks,
		Planners:    planners,
		Catalog:     api.CatalogOf(clients),
		Forwarder:   peers,
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
	wg.Go(func() { tracker.Run(ctx, collector) })
	wg.Go(func() { _ = engine.Run(ctx) })
	wg.Go(func() { _ = j.Run(ctx, cfg.Journal.AlignInterval) })
	wg.Go(func() { keepLease(ctx, cfg, coordinator) })

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

// newJournal returns the journal in the KV of every voter.
func newJournal(cfg *config.Serve, clients *consul.Clients) (*journal.Journal, error) {
	named, err := coord.NamedClients(cfg.Profile, clients)
	if err != nil {
		return nil, err
	}

	j, err := journal.New(journal.KVs(named), journal.Options{
		MaxEntries: cfg.Journal.MaxEntries,
		Timeout:    cfg.Status.RequestTimeout,
	}, logging.New(logging.WithComponent("dcctl.journal")))
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}

	return j, nil
}

// readAPI returns what the API reads from: the status and the alarms of the last cycle
// of tracker, the copies of dcctl/op in ops, the journal j and listen.peer as the
// observer address. Nothing provides verify checks yet.
func readAPI(cfg *config.Serve, ops *coord.OpStore, j *journal.Journal, tracker *status.Tracker) api.Options {
	log := logging.New(logging.WithComponent("dcctl.api"))

	return api.Options{
		Address:    cfg.Listen.Peer,
		Status:     api.FromTracker(tracker),
		Operations: ops,
		Journal:    j,
		Alarms:     api.TrackerAlarms(tracker, log),
		Log:        log,
	}
}

// newCoordinator returns the coordinator of the voters of serve: the KVs read through
// clients, which allow no stale reads, and the arbiter when there is one.
func newCoordinator(cfg *config.Serve, clients *consul.Clients, arb *arbiter.Client) (*coord.Coordinator, error) {
	voters, err := coord.NewVoters(cfg.Profile, clients, arb)
	if err != nil {
		return nil, err
	}

	c, err := coord.New(voters, cfg.Status.RequestTimeout, logging.New(logging.WithComponent("dcctl.coord")))
	if err != nil {
		return nil, fmt.Errorf("coordination: %w", err)
	}

	return c, nil
}

// operationDeps is what the operation engine of serve is built on.
type operationDeps struct {
	self        collect.Instance
	kvs         []coord.KV
	ops         *coord.OpStore
	coordinator *coord.Coordinator
	journal     *journal.Journal
	clients     *consul.Clients
	arb         *arbiter.Client
	last        func() *collect.Snapshot
}

// newEngine returns the operation engine, the preflight of the actions and their
// planners. The preflight collects with consistent reads and plans the managed jobs on
// the Nomad leaders, as an action acts on what it reads. In warm_standby the switch
// plans the switches and resumes its interrupted operations.
func newEngine(cfg *config.Serve, d operationDeps) (*operation.Engine, *preflight.Runner, api.Planners, error) {
	locker, err := coord.NewLocker(d.kvs, coord.LockOptions{
		TTL:       cfg.Coordination.SessionTTL,
		LockDelay: cfg.Coordination.LockDelay,
		Timeout:   cfg.Status.RequestTimeout,
	}, logging.New(logging.WithComponent("dcctl.lock")))
	if err != nil {
		return nil, nil, api.Planners{}, fmt.Errorf("lock: %w", err)
	}

	consistent, err := collect.FromConfig(cfg, d.self, d.arb, collect.ReadsConsistent,
		logging.New(logging.WithComponent("dcctl.preflight")))
	if err != nil {
		return nil, nil, api.Planners{}, err
	}

	planners := make(map[string]preflight.Planner, len(d.clients.Datacenters()))
	nomads := make(map[string]*nomad.Client, len(d.clients.Datacenters()))

	for _, dc := range d.clients.Datacenters() {
		nc, err := nomad.New(nomad.Options{
			Datacenter:      dc,
			Service:         nomad.ServiceName(cfg.Profile, dc),
			CAFile:          cfg.TLS.CA,
			CertFile:        cfg.TLS.Cert,
			KeyFile:         cfg.TLS.Key,
			Resolver:        d.clients,
			RequestTimeout:  cfg.Status.RequestTimeout,
			ConsistentReads: true,
		})
		if err != nil {
			return nil, nil, api.Planners{}, fmt.Errorf("nomad of %s: %w", dc, err)
		}

		planners[dc], nomads[dc] = nc, nc
	}

	recorder, err := telemetry.NewOperationRecorder(otel.GetMeterProvider())
	if err != nil {
		return nil, nil, api.Planners{}, err
	}

	runner := preflight.New(preflight.ConfigFrom(cfg), consistent, planners, d.last)
	resumers := make(map[dcctlv1.Operation_Type]operation.Resumer)

	var actions api.Planners

	if cfg.Profile == config.ProfileWarmStandby {
		sw, err := newSwitch(cfg, d, runner, nomads)
		if err != nil {
			return nil, nil, api.Planners{}, err
		}

		resumers[dcctlv1.Operation_SWITCH], actions.Switch = sw, sw
	}

	e, err := operation.New(operation.Config{
		Self:        operation.Instance{Node: d.self.Node, Datacenter: d.self.Datacenter, Address: cfg.Listen.Peer},
		Locker:      locker,
		Ops:         d.ops,
		Coordinator: d.coordinator,
		Preflight:   runner,
		Journal:     d.journal,
		Recorder:    recorder,
		Resumers:    resumers,
		Interval:    cfg.Coordination.SessionTTL,
		Log:         logging.New(logging.WithComponent("dcctl.operation")),
	})
	if err != nil {
		return nil, nil, api.Planners{}, fmt.Errorf("operations: %w", err)
	}

	return e, runner, actions, nil
}

// newSwitch returns the switch of warm_standby. It changes the apps through the Nomad
// clients of the preflight, which read consistently, and Patroni through clients that
// find the members in the Consul of their datacenter without stale reads.
func newSwitch(cfg *config.Serve, d operationDeps, verifier *preflight.Runner, nomads map[string]*nomad.Client) (*scenario.Switch, error) {
	a, err := apps.New(cfg.Apps, logging.New(logging.WithComponent("dcctl.apps")))
	if err != nil {
		return nil, fmt.Errorf("apps: %w", err)
	}

	pg, err := pgctl.New(cfg.Patroni, d.clients, logging.New(logging.WithComponent("dcctl.pgctl")))
	if err != nil {
		return nil, fmt.Errorf("patroni: %w", err)
	}

	dcs := make(map[string]scenario.Datacenter, len(nomads))

	for dc, nc := range nomads {
		pc, err := patroni.FromConfig(*cfg, dc, d.clients)
		if err != nil {
			return nil, fmt.Errorf("patroni of %s: %w", dc, err)
		}

		dcs[dc] = scenario.Datacenter{Nomad: nc, Patroni: pc}
	}

	sw, err := scenario.NewSwitch(scenario.Deps{
		Apps:        a,
		Patroni:     pg,
		Coordinator: d.coordinator,
		Verifier:    verifier,
		Datacenters: dcs,
	})
	if err != nil {
		return nil, fmt.Errorf("switch: %w", err)
	}

	return sw, nil
}

// keepLease renews the lease of this datacenter while the effective state names it.
func keepLease(ctx context.Context, cfg *config.Serve, c *coord.Coordinator) {
	if err := c.KeepLease(ctx, cfg.Datacenter, cfg.Coordination.LeasePeriod, false); err != nil {
		logging.New(logging.WithComponent("dcctl.coord")).ErrorCtx(ctx, "lease is not renewed", logging.Err(err))
	}
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
