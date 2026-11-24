package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/hashicorp/consul/api"

	"github.com/webitel/webitel-go-kit/infra/discovery"
	"github.com/webitel/webitel-go-kit/infra/discovery/consul"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"
)

const (
	defaultAgentAddress = "127.0.0.1:8500"
	// agentTimeout bounds one call to the local Consul agent.
	agentTimeout = 5 * time.Second
)

// Consul is the local Consul agent: Run checks that it belongs to Datacenter
// and registers the peer listener in it.
type Consul struct {
	// Address of the local agent; empty takes CONSUL_HTTP_ADDR, then
	// 127.0.0.1:8500.
	Address string
	// Datacenter must equal the agent's node_meta under NodeMetaKey.
	Datacenter  string
	NodeMetaKey string
}

func (c Consul) apiConfig() *api.Config {
	cfg := api.DefaultConfig()

	switch {
	case c.Address != "":
		cfg.Address = c.Address
	case os.Getenv(api.HTTPAddrEnvName) == "":
		cfg.Address = defaultAgentAddress
	}

	return cfg
}

// checkDatacenter refuses an agent whose node_meta names another datacenter:
// such an instance would act on the datacenter it is not in.
func (c Consul) checkDatacenter(ctx context.Context) error {
	cfg := c.apiConfig()
	cfg.HttpClient = &http.Client{Timeout: agentTimeout}

	client, err := api.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("consul client: %w", err)
	}

	self, err := await(ctx, client.Agent().Self)
	if err != nil {
		return fmt.Errorf("read node_meta of the local Consul agent at %s: %w", cfg.Address, err)
	}

	got, ok := self["Meta"][c.NodeMetaKey].(string)
	if !ok {
		return fmt.Errorf("no node_meta %s in the local Consul agent; datacenter is %q", c.NodeMetaKey, c.Datacenter)
	}

	if got != c.Datacenter {
		return fmt.Errorf("node_meta %s=%q of the local Consul agent, but datacenter is %q", c.NodeMetaKey, got, c.Datacenter)
	}

	return nil
}

// register registers the peer listener as buildinfo.ServiceName with a TTL
// check that follows readiness. The returned func deregisters within its ctx.
func (c Consul) register(ctx context.Context, peer net.Addr, ready func() (bool, error), log *slog.Logger) (func(context.Context) error, error) {
	rctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()

	reg, err := await(rctx, func() (*consul.Registry, error) {
		return consul.NewConsulRegistry(c.apiConfig().Address, log)
	})
	if err != nil {
		return nil, fmt.Errorf("consul registry: %w", err)
	}

	reg.SetTimeout(agentTimeout)
	reg.SetHealthCheck(false)
	consul.WithReadiness(ready)(reg)

	svc := &discovery.ServiceInstance{
		Id:        discovery.GenerateInstanceID(buildinfo.ServiceName),
		Name:      buildinfo.ServiceName,
		Version:   buildinfo.Version,
		Endpoints: []string{"grpc://" + peer.String()},
	}

	if err := reg.Register(rctx, svc); err != nil {
		return nil, fmt.Errorf("register %s in the local Consul agent: %w", buildinfo.ServiceName, err)
	}

	return func(ctx context.Context) error {
		dctx, cancel := context.WithTimeout(ctx, agentTimeout)
		defer cancel()

		_, err := await(dctx, func() (struct{}, error) { return struct{}{}, reg.Deregister(dctx, svc) })
		if err != nil {
			return fmt.Errorf("deregister %s from the local Consul agent: %w", buildinfo.ServiceName, err)
		}

		return nil
	}, nil
}

// await stops waiting for fn when ctx is done. The Consul calls it wraps do
// not take a ctx (Agent.Self, the registry constructor) or do not honor it
// while waiting for the TTL heartbeat (Registry.Deregister); fn finishes in
// the background.
func await[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}

	done := make(chan result, 1)

	go func() {
		v, err := fn()
		done <- result{v, err}
	}()

	select {
	case res := <-done:
		return res.v, res.err
	case <-ctx.Done():
		var zero T

		return zero, ctx.Err()
	}
}
