package telemetry

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/webitel/webitel-go-kit/infra/health"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// The health systems of the checks of serve.
const (
	SystemDcctl   health.System = "dcctl"
	SystemConsul  health.System = "consul"
	SystemNomad   health.System = "nomad"
	SystemPatroni health.System = "patroni"
)

var errNoCycle = errors.New("no collection cycle observed yet")

// HealthConfig is what the health checks of serve take from its config.
type HealthConfig struct {
	// Datacenter is the datacenter of this instance.
	Datacenter string
	Profile    config.Profile
	// Arbiter is set when an arbiter is configured.
	Arbiter bool
}

// RegisterHealth registers the checks of serve on reg: collector, the liveness check
// of the collection loop, and the informational checks of the sources of the own
// datacenter and the arbiter as the last cycle in src saw them. The informational
// checks fail until the first cycle. Call it before reg starts.
func RegisterHealth(reg *health.Registry, collector health.Check, src Observations, cfg HealthConfig) {
	c := &snapshotChecks{src: src, cfg: cfg}

	reg.Liveness(SystemDcctl, collector, health.WithName("collector"))
	reg.Informational(SystemConsul, c.consul, health.WithName("local"))
	reg.Informational(SystemNomad, c.nomad, health.WithName("local"))
	reg.Informational(SystemPatroni, c.patroni, health.WithName("local"))

	if cfg.Arbiter {
		reg.Informational(SystemDcctl, c.arbiter, health.WithName("arbiter"))
	}
}

// snapshotChecks read the status of the last cycle and ask no source.
type snapshotChecks struct {
	src Observations
	cfg HealthConfig
}

// own returns the status of the last cycle and the own datacenter in it.
func (c *snapshotChecks) own() (*status.Status, *status.Datacenter, error) {
	obs := c.src.Current()
	if obs == nil {
		return nil, nil, errNoCycle
	}

	dcs := obs.Status.Datacenters

	i := slices.IndexFunc(dcs, func(d status.Datacenter) bool { return d.Name == c.cfg.Datacenter })
	if i < 0 {
		return nil, nil, fmt.Errorf("datacenter %s is not observed", c.cfg.Datacenter)
	}

	return &obs.Status, &dcs[i], nil
}

func (c *snapshotChecks) source(sys status.System) error {
	_, d, err := c.own()
	if err != nil {
		return err
	}

	if s := d.Source(sys); !s.Reachable {
		return fmt.Errorf("%s of %s does not answer: %s", sys, d.Name, s.Reason)
	}

	return nil
}

// consul checks the catalog of the own datacenter and in warm_standby also its KV
// voter, as webitel.dcctl.source.status does.
func (c *snapshotChecks) consul(context.Context) error {
	st, d, err := c.own()
	if err != nil {
		return err
	}

	if state := consulState(st, d); state != sourceOK {
		return fmt.Errorf("the Consul of %s is %s", d.Name, state)
	}

	return nil
}

func (c *snapshotChecks) nomad(context.Context) error {
	return c.source(status.SystemNomad)
}

// patroni checks the leader of the own datacenter in warm_standby. In stretch the
// leader of the one cluster runs in a single datacenter, so the Patroni of the others
// is checked by their own members.
func (c *snapshotChecks) patroni(context.Context) error {
	if err := c.source(status.SystemPatroni); err != nil || c.cfg.Profile == config.ProfileStretch {
		return err
	}

	_, d, err := c.own()
	if err != nil {
		return err
	}

	p := d.Patroni
	if p.Leader == "" {
		return fmt.Errorf("the Patroni of %s has no leader", d.Name)
	}

	i := slices.IndexFunc(p.Members, func(m status.Member) bool { return m.Name == p.Leader })
	if i < 0 || !p.Members[i].Reachable {
		return fmt.Errorf("the Patroni leader %s of %s does not answer", p.Leader, d.Name)
	}

	return nil
}

func (c *snapshotChecks) arbiter(context.Context) error {
	obs := c.src.Current()
	if obs == nil {
		return errNoCycle
	}

	i := slices.IndexFunc(obs.Status.Voters, func(v status.Voter) bool { return v.Type == status.VoterArbiter })

	switch {
	case i < 0:
		return errors.New("the arbiter is not observed")
	case !obs.Status.Voters[i].Reachable:
		return fmt.Errorf("the arbiter does not answer: %s", obs.Status.Voters[i].Reason)
	}

	return nil
}
