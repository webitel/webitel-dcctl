// Package runtime runs the network side of a dcctl process: the gRPC and
// gateway listener for the CLI and UI, the mTLS listener for other dcctl
// instances, health probes, systemd notifications and the Consul
// registration. Services register on it before Run.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/webitel/webitel-go-kit/infra/health"
	healthhttp "github.com/webitel/webitel-go-kit/infra/health/http"
	"github.com/webitel/webitel-go-kit/infra/health/sdnotify"
	otelhealth "github.com/webitel/webitel-go-kit/infra/otel/instrumentation/health"
	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
)

const (
	defaultStopTimeout = 10 * time.Second
	readHeaderTimeout  = 10 * time.Second
	idleTimeout        = 2 * time.Minute
	// healthStopMargin bounds the health transports past the drain hold.
	healthStopMargin = 5 * time.Second
	bufconnSize      = 1 << 20
)

// Config holds the listeners and settings of a Runtime.
type Config struct {
	// HTTP serves gRPC over h2c, the gateway under /api/ and the UI; empty
	// disables it.
	HTTP string
	// Peer serves gRPC with mutual TLS.
	Peer string
	// Health serves /livez, /readyz and /healthz.
	Health string
	TLS    config.TLS
	// HTTPOptions and PeerOptions are added to the gRPC servers of HTTP and
	// Peer, for example interceptors.
	HTTPOptions []grpc.ServerOption
	PeerOptions []grpc.ServerOption
	// Consul, when set, makes Run check the datacenter of the local Consul
	// agent and register Peer in it.
	Consul *Consul
	// HealthConfig tunes the health checks; zero fields take the go-kit defaults.
	HealthConfig health.Config
	// StopTimeout bounds the graceful stop of the servers; zero means 10s.
	StopTimeout time.Duration
}

// GatewayHandler registers gateway routes that call conn, like the generated
// RegisterXxxServiceHandler functions.
type GatewayHandler func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error

// Runtime is a dcctl process: register services, then Run.
type Runtime struct {
	cfg Config
	log *logging.Logger

	httpGRPC *grpc.Server
	peerGRPC *grpc.Server
	gateway  *runtime.ServeMux
	gwLn     *bufconn.Listener
	gwConn   *grpc.ClientConn
	ui       http.Handler

	health    *health.Registry
	healthSrv *healthhttp.Server

	// Set by Run before started is closed.
	httpLn  net.Listener
	peerLn  net.Listener
	httpSrv *http.Server
	started chan struct{}
}

// New builds a Runtime. It reads the TLS files and does no network I/O.
func New(cfg Config, log *logging.Logger) (*Runtime, error) {
	tlsCfg, err := serverTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}

	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = defaultStopTimeout
	}

	r := &Runtime{
		cfg:      cfg,
		log:      log,
		peerGRPC: grpc.NewServer(append([]grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsCfg))}, cfg.PeerOptions...)...),
		started:  make(chan struct{}),
	}

	if cfg.HTTP != "" {
		r.httpGRPC = grpc.NewServer(cfg.HTTPOptions...)
		r.gateway = newGatewayMux(r.handleError)
		r.gwLn = bufconn.Listen(bufconnSize)

		r.gwConn, err = grpc.NewClient("passthrough:///gateway",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return r.gwLn.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, fmt.Errorf("gateway client: %w", err)
		}
	}

	sl := log.Slog()
	r.healthSrv = healthhttp.NewServer(cfg.Health, healthhttp.WithLogger(sl))
	r.health = health.New(cfg.HealthConfig, sl,
		health.WithTransport(r.healthSrv),
		health.WithTransport(sdnotify.New(sdnotify.WithLogger(sl))),
		health.WithTransport(otelhealth.NewTransport()),
	)

	return r, nil
}

// RegisterService registers a gRPC service on every listener. Call it before Run.
func (r *Runtime) RegisterService(desc *grpc.ServiceDesc, impl any) {
	r.peerGRPC.RegisterService(desc, impl)

	if r.httpGRPC != nil {
		r.httpGRPC.RegisterService(desc, impl)
	}
}

// RegisterGateway adds gateway routes, served under /api/ of the HTTP
// listener. The gateway calls the gRPC services of that listener, through
// the same interceptors as the CLI. Call it before Run.
func (r *Runtime) RegisterGateway(ctx context.Context, h GatewayHandler) error {
	if r.gateway == nil {
		return errors.New("register gateway: no HTTP listener")
	}

	return h(ctx, r.gateway, r.gwConn)
}

// HandleUI serves h on every path of the HTTP listener outside /api/. Call
// it before Run.
func (r *Runtime) HandleUI(h http.Handler) {
	r.ui = h
}

// Health returns the health registry, for checks of other packages. Register
// them before Run.
func (r *Runtime) Health() *health.Registry {
	return r.health
}

// Run serves until ctx is done, then stops gracefully. It returns early when
// the local Consul agent belongs to another datacenter, a listener cannot
// bind, the registration fails or a server fails.
func (r *Runtime) Run(ctx context.Context) error {
	if err := r.prepare(ctx); err != nil {
		if r.gwConn != nil {
			_ = r.gwConn.Close()
		}

		return err
	}

	r.health.Critical(health.SystemHTTP, health.ListenerCheck(r.peerLn), health.WithName("peer"))

	if r.httpLn != nil {
		r.health.Critical(health.SystemHTTP, health.ListenerCheck(r.httpLn), health.WithName("api"))
	}

	var wg sync.WaitGroup

	served := make(chan error, 3)
	r.serve(&wg, served)

	var deregister func(context.Context) error

	runErr := r.health.Start(context.WithoutCancel(ctx))
	if runErr == nil && r.cfg.Consul != nil {
		deregister, runErr = r.cfg.Consul.register(ctx, r.peerLn.Addr(), r.health.ReadyFunc(), r.log.Slog())
	}

	if runErr == nil {
		r.log.InfoCtx(ctx, "started", r.addrFields()...)
		close(r.started)

		select {
		case <-ctx.Done():
		case runErr = <-served:
		}
	}

	stopCtx := context.WithoutCancel(ctx)
	r.log.InfoCtx(stopCtx, "stopping")

	err := errors.Join(runErr, r.stop(stopCtx, deregister))

	wg.Wait()

	return err
}

// prepare runs the start checks and binds the listeners.
func (r *Runtime) prepare(ctx context.Context) error {
	if c := r.cfg.Consul; c != nil {
		if err := c.checkDatacenter(ctx); err != nil {
			return err
		}
	}

	return r.listen(ctx)
}

// listen binds every listener, or none.
func (r *Runtime) listen(ctx context.Context) error {
	var lc net.ListenConfig

	peer, err := lc.Listen(ctx, "tcp", r.cfg.Peer)
	if err != nil {
		return fmt.Errorf("listen.peer: %w", err)
	}

	r.peerLn = peer

	if r.httpGRPC == nil {
		return nil
	}

	r.httpLn, err = lc.Listen(ctx, "tcp", r.cfg.HTTP)
	if err != nil {
		_ = peer.Close()

		return fmt.Errorf("listen.http: %w", err)
	}

	return nil
}

func (r *Runtime) serve(wg *sync.WaitGroup, served chan<- error) {
	wg.Go(func() {
		if err := r.peerGRPC.Serve(r.peerLn); err != nil {
			served <- fmt.Errorf("serve listen.peer: %w", err)
		}
	})

	if r.httpLn == nil {
		return
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	r.httpSrv = &http.Server{
		Handler:           r.httpHandler(),
		Protocols:         protocols,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(r.log.Slog().Handler(), slog.LevelWarn),
	}

	wg.Go(func() {
		if err := r.httpSrv.Serve(r.httpLn); !errors.Is(err, http.ErrServerClosed) {
			served <- fmt.Errorf("serve listen.http: %w", err)
		}
	})
	wg.Go(func() {
		if err := r.httpGRPC.Serve(r.gwLn); err != nil {
			served <- fmt.Errorf("serve gateway: %w", err)
		}
	})
}

// stop leaves Consul, drains health, then stops the servers within
// StopTimeout.
func (r *Runtime) stop(ctx context.Context, deregister func(context.Context) error) error {
	var err error
	if deregister != nil {
		err = deregister(ctx)
	}

	hctx, cancel := context.WithTimeout(ctx, r.drainHold()+healthStopMargin)
	err = errors.Join(err, r.health.Shutdown(hctx))

	cancel()

	sctx, cancel := context.WithTimeout(ctx, r.cfg.StopTimeout)
	defer cancel()

	var wg sync.WaitGroup

	wg.Go(func() { gracefulStop(sctx, r.peerGRPC) })

	if r.httpSrv != nil {
		wg.Go(func() {
			if serr := r.httpSrv.Shutdown(sctx); serr != nil {
				r.httpGRPC.Stop()
				_ = r.httpSrv.Close()
			}

			_ = r.gwConn.Close()
			gracefulStop(sctx, r.httpGRPC)
		})
	}

	wg.Wait()

	return err
}

func (r *Runtime) drainHold() time.Duration {
	if r.cfg.HealthConfig.DrainHold > 0 {
		return r.cfg.HealthConfig.DrainHold
	}

	return health.DefaultConfig().DrainHold
}

// gracefulStop waits for running calls until ctx is done, then cancels them.
func gracefulStop(ctx context.Context, s *grpc.Server) {
	done := make(chan struct{})

	go func() {
		s.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		s.Stop()
		<-done
	}
}

func (r *Runtime) addrFields() []logging.Field {
	fields := []logging.Field{logging.Str("listen.peer", r.peerLn.Addr().String())}
	if r.httpLn != nil {
		fields = append(fields, logging.Str("listen.http", r.httpLn.Addr().String()))
	}

	return append(fields, logging.Str("listen.health", r.healthSrv.Addr()))
}
