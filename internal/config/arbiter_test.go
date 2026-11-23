package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const renderedArbiter = `
listen:
  api: 10.9.0.10:10024
  health: 127.0.0.1:10025
tls:
  ca: /etc/ssl/app/ca.pem
  cert: /etc/ssl/app/arbiter.pem
  key: /etc/ssl/app/arbiter-key.pem
clients:
  - 10.1.0.81
  - 10.2.0.81
`

func validArbiter(t *testing.T) *Arbiter {
	t.Helper()

	cfg, err := parseArbiter([]byte(renderedArbiter))
	require.NoError(t, err)

	return cfg
}

func TestParseArbiter(t *testing.T) {
	got := validArbiter(t)

	assert.Equal(t, Arbiter{
		Listen:  ArbiterListen{API: "10.9.0.10:10024", Health: "127.0.0.1:10025"},
		DataDir: "/var/lib/webitel/webitel-dcctl-arbiter",
		TLS: TLS{
			CA:   "/etc/ssl/app/ca.pem",
			Cert: "/etc/ssl/app/arbiter.pem",
			Key:  "/etc/ssl/app/arbiter-key.pem",
		},
		Clients: []string{"10.1.0.81", "10.2.0.81"},
	}, *got)
	assert.NoError(t, got.Validate())
}

func TestParseArbiter_errors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown_key", renderedArbiter + "address: 10.9.0.10:10024\n", "field address not found"},
		{"unknown_listen_key", "listen:\n  http: 127.0.0.1:10022\n", "field http not found"},
		{"empty_file", "", "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArbiter([]byte(tt.yaml))
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestArbiterValidate_invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Arbiter)
		want   string
	}{
		{"listen_api_empty", func(c *Arbiter) { c.Listen.API = "" }, "listen.api: must not be empty"},
		{"listen_api_no_port", func(c *Arbiter) { c.Listen.API = "10.9.0.10" }, `listen.api: "10.9.0.10" is not an IP:port address`},
		{"listen_health_empty", func(c *Arbiter) { c.Listen.Health = "" }, "listen.health: must not be empty"},
		{"listen_health_node_ip", func(c *Arbiter) { c.Listen.Health = "10.9.0.10:10025" }, `listen.health: "10.9.0.10:10025" is not a loopback address`},
		{"data_dir_empty", func(c *Arbiter) { c.DataDir = "" }, "data_dir: must not be empty"},
		{"data_dir_relative", func(c *Arbiter) { c.DataDir = "arbiter" }, `data_dir: "arbiter" is not an absolute path`},
		{"tls_ca_empty", func(c *Arbiter) { c.TLS.CA = "" }, "tls.ca: must not be empty"},
		{"tls_cert_empty", func(c *Arbiter) { c.TLS.Cert = "" }, "tls.cert: must not be empty"},
		{"tls_key_empty", func(c *Arbiter) { c.TLS.Key = "" }, "tls.key: must not be empty"},
		{"clients_empty", func(c *Arbiter) { c.Clients = nil }, "clients: must not be empty"},
		{"client_with_port", func(c *Arbiter) { c.Clients[1] = "10.2.0.81:10023" }, `clients[1]: "10.2.0.81:10023" is not an IP address`},
		{"client_hostname", func(c *Arbiter) { c.Clients[0] = "dc-a-dcctl" }, `clients[0]: "dc-a-dcctl" is not an IP address`},
		{"client_cidr", func(c *Arbiter) { c.Clients[0] = "10.1.0.0/24" }, `clients[0]: "10.1.0.0/24" is not an IP address`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validArbiter(t)
			tt.mutate(cfg)

			assert.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}
