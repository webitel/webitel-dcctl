package arbiter

import (
	"context"
	"errors"

	"github.com/webitel/webitel-go-kit/infra/health"
	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/arbiter/store"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/runtime"
)

// systemDcctl is the health system of checks of dcctl's own components.
const systemDcctl health.System = "dcctl"

// Run serves the arbiter of cfg until ctx is done: ArbiterService over the
// store in cfg.DataDir, on listen.api with mutual TLS for the clients of
// cfg, and the probes on listen.health. It closes the store on return.
func Run(ctx context.Context, cfg *config.Arbiter, log *logging.Logger) error {
	return serve(ctx, cfg, health.Config{}, log)
}

func serve(ctx context.Context, cfg *config.Arbiter, hc health.Config, log *logging.Logger) (err error) {
	opts, err := auth.ArbiterServerOptions(cfg.Clients, log)
	if err != nil {
		return err
	}

	rt, err := runtime.New(runtime.Config{
		Peer:         cfg.Listen.API,
		PeerKey:      "listen.api",
		PeerCheck:    "arbiter",
		Health:       cfg.Listen.Health,
		TLS:          cfg.TLS,
		PeerOptions:  opts,
		HealthConfig: hc,
	}, log)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, st.Close()) }()

	rt.Health().Critical(systemDcctl, st.Check, health.WithName("store"))
	rt.RegisterService(&dcctlv1.ArbiterService_ServiceDesc, NewServer(st, log))

	return rt.Run(ctx)
}
