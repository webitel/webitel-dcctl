package config

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
)

// DefaultArbiterPath is where Ansible renders the arbiter config.
const DefaultArbiterPath = "/etc/webitel/dcctl/arbiter.yaml"

// Arbiter is the config of "webitel-dcctl arbiter".
type Arbiter struct {
	Listen  ArbiterListen `yaml:"listen"`
	DataDir string        `yaml:"data_dir"`
	TLS     TLS           `yaml:"tls"`
	// Clients are the IP addresses of dcctl instances allowed to connect;
	// a client certificate must carry one of them as SAN IP.
	Clients []string `yaml:"clients"`
}

// ArbiterListen holds the listener addresses of the arbiter.
type ArbiterListen struct {
	// API serves gRPC with mTLS for dcctl instances, which dial it, so it is
	// a node address.
	API string `yaml:"api"`
	// Health serves the unauthenticated probes; loopback only.
	Health string `yaml:"health"`
}

func defaultArbiter() Arbiter {
	return Arbiter{DataDir: "/var/lib/webitel/webitel-dcctl-arbiter"}
}

func parseArbiter(data []byte) (*Arbiter, error) {
	cfg := defaultArbiter()
	if err := decode(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// LoadArbiter reads, applies defaults to and validates the arbiter config at path.
func LoadArbiter(path string) (*Arbiter, error) {
	return load(path, parseArbiter, (*Arbiter).Validate)
}

// Validate reports every invalid value of c, one per line.
func (c *Arbiter) Validate() error {
	var v violations

	v.nodeAddr("listen.api", c.Listen.API)
	v.loopback("listen.health", c.Listen.Health)

	if v.required("data_dir", c.DataDir) && !filepath.IsAbs(c.DataDir) {
		v.add("data_dir", "%q is not an absolute path", c.DataDir)
	}

	v.tls(c.TLS)

	if len(c.Clients) == 0 {
		v.add("clients", "must not be empty")
	}

	for i, client := range c.Clients {
		if _, err := netip.ParseAddr(client); err != nil {
			v.add(fmt.Sprintf("clients[%d]", i), "%q is not an IP address", client)
		}
	}

	return errors.Join(v...)
}
