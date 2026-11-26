package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validServe(t *testing.T) *Serve {
	t.Helper()

	cfg, err := parseServe([]byte(renderedServe))
	require.NoError(t, err)

	return cfg
}

func TestServeValidate_valid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Serve)
	}{
		{name: "rendered", mutate: func(*Serve) {}},
		{name: "stretch", mutate: func(c *Serve) { c.Profile, c.Arbiter.Address = ProfileStretch, "" }},
		{name: "ipv6_loopback", mutate: func(c *Serve) { c.Listen.HTTP = "[::1]:10022" }},
		{name: "loopback_not_127_0_0_1", mutate: func(c *Serve) { c.Listen.Health = "127.0.0.2:10025" }},
		{name: "rabbitmq_service_template", mutate: func(c *Serve) { c.Status.RabbitMQService = "rabbitmq-{datacenter}" }},
		{name: "no_info_services", mutate: func(c *Serve) { c.Status.InfoServices = nil }},
		{name: "no_arbiter", mutate: func(c *Serve) { c.Arbiter.Address = "" }},
		{name: "ui_dir", mutate: func(c *Serve) { c.UI.Dir = "/usr/share/webitel-dcctl/ui" }},
		{name: "consul_server_hostname", mutate: func(c *Serve) { c.Consul.Servers[1].Address = "consul.dc-b.example:8500" }},
		{name: "patroni_endpoints_on_stretch", mutate: func(c *Serve) {
			c.Profile, c.Arbiter.Address = ProfileStretch, ""
			c.Patroni.Endpoints = []string{"10.1.0.11:8008", "pg-b1.example:8008"}
		}},
		{name: "bcrypt_max_cost", mutate: func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2b$14$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}},
		{name: "no_patroni_credentials", mutate: func(c *Serve) { c.Patroni.Username, c.Patroni.Password = "", "" }},
		{name: "zero_planned_max_lag", mutate: func(c *Serve) { c.Patroni.PlannedMaxLagBytes = 0 }},
		{name: "bcrypt_2a", mutate: func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2a$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}},
		{name: "bcrypt_2y", mutate: func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2y$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validServe(t)
			tt.mutate(cfg)

			assert.NoError(t, cfg.Validate())
		})
	}
}

func TestServeValidate_invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Serve)
		want   string
	}{
		{"profile_empty", func(c *Serve) { c.Profile = "" }, `profile: "" is not one of warm_standby, stretch`},
		{"profile_unknown", func(c *Serve) { c.Profile = "failover" }, `profile: "failover" is not one of warm_standby, stretch`},

		{"datacenter_empty", func(c *Serve) { c.Datacenter = "" }, "datacenter: must not be empty"},
		{"datacenter_not_in_consul", func(c *Serve) { c.Datacenter = "dc_c" }, `datacenter: "dc_c" is not among consul.servers[].datacenter [dc_a dc_b]`},

		{"listen_http_empty", func(c *Serve) { c.Listen.HTTP = "" }, "listen.http: must not be empty"},
		{"listen_http_node_ip", func(c *Serve) { c.Listen.HTTP = "10.1.0.81:10022" }, `listen.http: "10.1.0.81:10022" is not a loopback address`},
		{"listen_http_all_interfaces", func(c *Serve) { c.Listen.HTTP = "0.0.0.0:10022" }, `listen.http: "0.0.0.0:10022" is not a loopback address`},
		{"listen_http_no_host", func(c *Serve) { c.Listen.HTTP = ":10022" }, `listen.http: ":10022" is not an IP:port address`},
		{"listen_http_hostname", func(c *Serve) { c.Listen.HTTP = "localhost:10022" }, `listen.http: "localhost:10022" is not an IP:port address`},
		{"listen_http_zero_port", func(c *Serve) { c.Listen.HTTP = "127.0.0.1:0" }, `listen.http: "127.0.0.1:0" has no port`},
		{"listen_health_empty", func(c *Serve) { c.Listen.Health = "" }, "listen.health: must not be empty"},
		{"listen_health_node_ip", func(c *Serve) { c.Listen.Health = "10.1.0.81:10025" }, `listen.health: "10.1.0.81:10025" is not a loopback address`},
		{"listen_peer_empty", func(c *Serve) { c.Listen.Peer = "" }, "listen.peer: must not be empty"},
		{"listen_peer_no_port", func(c *Serve) { c.Listen.Peer = "10.1.0.81" }, `listen.peer: "10.1.0.81" is not an IP:port address`},
		{"listen_peer_all_interfaces", func(c *Serve) { c.Listen.Peer = "0.0.0.0:10023" }, `listen.peer: "0.0.0.0:10023" is not a node address`},
		{"listen_peer_ipv6_all_interfaces", func(c *Serve) { c.Listen.Peer = "[::]:10023" }, `listen.peer: "[::]:10023" is not a node address`},
		{"listen_peer_loopback", func(c *Serve) { c.Listen.Peer = "127.0.0.1:10023" }, `listen.peer: "127.0.0.1:10023" is not a node address`},
		{"listen_peer_link_local", func(c *Serve) { c.Listen.Peer = "169.254.10.1:10023" }, `listen.peer: "169.254.10.1:10023" is not a node address`},
		{"listen_peer_ipv6_link_local", func(c *Serve) { c.Listen.Peer = "[fe80::1]:10023" }, `listen.peer: "[fe80::1]:10023" is not a node address`},

		{"tls_ca_empty", func(c *Serve) { c.TLS.CA = "" }, "tls.ca: must not be empty"},
		{"tls_cert_empty", func(c *Serve) { c.TLS.Cert = "" }, "tls.cert: must not be empty"},
		{"tls_key_empty", func(c *Serve) { c.TLS.Key = "" }, "tls.key: must not be empty"},

		{"auth_users_empty", func(c *Serve) { c.Auth.Users = nil }, "auth.users: must not be empty"},
		{"auth_user_name_empty", func(c *Serve) { c.Auth.Users[0].Name = "" }, "auth.users[0].name: must not be empty"},
		{"auth_user_name_colon", func(c *Serve) { c.Auth.Users[0].Name = "a:b" }, `auth.users[0].name: "a:b" contains ':'`},
		{"auth_user_duplicate", func(c *Serve) { c.Auth.Users = append(c.Auth.Users, c.Auth.Users[0]) }, `auth.users[1].name: duplicate "support"`},
		{"auth_hash_empty", func(c *Serve) { c.Auth.Users[0].PasswordHash = "" }, "auth.users[0].password_hash: must not be empty"},
		{"auth_hash_plaintext", func(c *Serve) { c.Auth.Users[0].PasswordHash = "secret" }, "auth.users[0].password_hash: is not a bcrypt hash"},
		{"auth_hash_sha512_crypt", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$6$rounds=5000$saltsalt$Zmh9ZQ0oX0V1n2T0e8M1JX0dU8T2W5yG8wGJ8VJ6wQnq1mJ8Z0r1qJcY2L9pX3vB7mN4kH6sD5fG1aZ2xC3vB."
		}, "auth.users[0].password_hash: is not a bcrypt hash"},
		{"auth_hash_truncated", func(c *Serve) { c.Auth.Users[0].PasswordHash = "$2b$04$AUJV.yvHyWu9LFhtKITeVe" }, "auth.users[0].password_hash: is not a bcrypt hash"},
		{"auth_hash_unknown_prefix", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2z$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}, "auth.users[0].password_hash: is not a bcrypt hash"},
		{"auth_hash_foreign_alphabet", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2b$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFU!"
		}, "auth.users[0].password_hash: is not a bcrypt hash"},
		{"auth_hash_trailing_garbage", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2b$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUWx"
		}, "auth.users[0].password_hash: is not a bcrypt hash"},
		{"auth_hash_cost_below_min", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2b$03$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}, "auth.users[0].password_hash: bcrypt cost must be within 4..14"},
		{"auth_hash_cost_above_max", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2b$15$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}, "auth.users[0].password_hash: bcrypt cost must be within 4..14"},
		{"auth_hash_cost_31", func(c *Serve) {
			c.Auth.Users[0].PasswordHash = "$2b$31$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
		}, "auth.users[0].password_hash: bcrypt cost must be within 4..14"},
		{"auth_role_empty", func(c *Serve) { c.Auth.Users[0].Role = "" }, `auth.users[0].role: "" is not one of operator`},
		{"auth_role_unknown", func(c *Serve) { c.Auth.Users[0].Role = "admin" }, `auth.users[0].role: "admin" is not one of operator`},

		{"consul_node_meta_key_empty", func(c *Serve) { c.Consul.NodeMetaKey = "" }, "consul.node_meta_key: must not be empty"},
		{"consul_servers_empty", func(c *Serve) { c.Consul.Servers = nil }, "consul.servers: must not be empty"},
		{"consul_server_address_empty", func(c *Serve) { c.Consul.Servers[0].Address = "" }, "consul.servers[0].address: must not be empty"},
		{"consul_server_address_no_port", func(c *Serve) { c.Consul.Servers[0].Address = "10.1.0.89" }, `consul.servers[0].address: "10.1.0.89" is not a host:port address`},
		{"consul_server_address_bad_port", func(c *Serve) { c.Consul.Servers[0].Address = "10.1.0.89:http" }, `consul.servers[0].address: "10.1.0.89:http" is not a host:port address`},
		{"consul_server_datacenter_empty", func(c *Serve) { c.Consul.Servers[1].Datacenter = "" }, "consul.servers[1].datacenter: must not be empty"},

		{"patroni_scope_empty", func(c *Serve) { c.Patroni.Scope = "" }, "patroni.scope: must not be empty"},
		{"patroni_password_without_username", func(c *Serve) { c.Patroni.Username = "" }, "patroni.username: must be set together with patroni.password"},
		{"patroni_username_without_password", func(c *Serve) { c.Patroni.Password = "" }, "patroni.password: must be set together with patroni.username"},
		{"patroni_endpoints_on_warm_standby", func(c *Serve) { c.Patroni.Endpoints = []string{"10.1.0.11:8008"} }, "patroni.endpoints: must be empty for profile warm_standby"},
		{"patroni_endpoint_empty", func(c *Serve) { c.Patroni.Endpoints = []string{""} }, "patroni.endpoints[0]: must not be empty"},
		{"patroni_endpoint_no_port", func(c *Serve) { c.Patroni.Endpoints = []string{"10.1.0.11:8008", "10.1.0.12"} }, `patroni.endpoints[1]: "10.1.0.12" is not a host:port address`},
		{"patroni_endpoint_url", func(c *Serve) { c.Patroni.Endpoints = []string{"https://10.1.0.11:8008"} }, `patroni.endpoints[0]: "https://10.1.0.11:8008" is not a host:port address`},
		{"patroni_planned_max_lag_negative", func(c *Serve) { c.Patroni.PlannedMaxLagBytes = -1 }, "patroni.planned_max_lag_bytes: must not be negative"},
		{"patroni_timeouts_demote", func(c *Serve) { c.Patroni.Timeouts.Demote = 0 }, "patroni.timeouts.demote: must be positive"},
		{"patroni_timeouts_catchup", func(c *Serve) { c.Patroni.Timeouts.Catchup = 0 }, "patroni.timeouts.catchup: must be positive"},
		{"patroni_timeouts_promote", func(c *Serve) { c.Patroni.Timeouts.Promote = 0 }, "patroni.timeouts.promote: must be positive"},
		{"patroni_timeouts_repoint", func(c *Serve) { c.Patroni.Timeouts.Repoint = 0 }, "patroni.timeouts.repoint: must be positive"},
		{"patroni_timeouts_rewind", func(c *Serve) { c.Patroni.Timeouts.Rewind = -1 }, "patroni.timeouts.rewind: must be positive"},

		{"coordination_session_ttl_zero", func(c *Serve) { c.Coordination.SessionTTL = 0 }, "coordination.session_ttl: must be within 10s..24h0m0s"},
		{"coordination_session_ttl_below_consul_min", func(c *Serve) { c.Coordination.SessionTTL = 9 * time.Second }, "coordination.session_ttl: must be within 10s..24h0m0s"},
		{"coordination_session_ttl_above_consul_max", func(c *Serve) { c.Coordination.SessionTTL = 25 * time.Hour }, "coordination.session_ttl: must be within 10s..24h0m0s"},
		{"coordination_lock_delay_zero", func(c *Serve) { c.Coordination.LockDelay = 0 }, "coordination.lock_delay: must be greater than 0 and at most 1m0s"},
		{"coordination_lock_delay_above_consul_max", func(c *Serve) { c.Coordination.LockDelay = 61 * time.Second }, "coordination.lock_delay: must be greater than 0 and at most 1m0s"},
		{"coordination_lock_delay_within_renewal", func(c *Serve) { c.Coordination.LockDelay = 8 * time.Second }, "coordination.lock_delay: must be greater than coordination.session_ttl/3 + status.request_timeout (8s)"},
		{"status_request_timeout_above_renewal", func(c *Serve) {
			c.Status.RequestTimeout = 5 * time.Second
			c.Coordination.LockDelay = 30 * time.Second
		}, "status.request_timeout: must be less than coordination.session_ttl/3 (5s)"},
		{"coordination_lease_period", func(c *Serve) { c.Coordination.LeasePeriod = 0 }, "coordination.lease_period: must be positive"},
		{"coordination_lease_acquire_after", func(c *Serve) { c.Coordination.LeaseAcquireAfter = 0 }, "coordination.lease_acquire_after: must be positive"},
		{"coordination_lease_period_below_renewal_round", func(c *Serve) { c.Coordination.LeasePeriod = 26 * time.Second }, "coordination.lease_period: must be at least 3 × 3 × status.request_timeout (27s)"},

		{"apps_stop_timeout", func(c *Serve) { c.Apps.StopTimeout = 0 }, "apps.stop_timeout: must be positive"},
		{"apps_stage_start_timeout", func(c *Serve) { c.Apps.StageStartTimeout = 0 }, "apps.stage_start_timeout: must be positive"},
		{"apps_failover_stop_timeout", func(c *Serve) { c.Apps.FailoverStopTimeout = 0 }, "apps.failover_stop_timeout: must be positive"},
		{"apps_min_healthy_time", func(c *Serve) { c.Apps.MinHealthyTime = 0 }, "apps.min_healthy_time: must be positive"},

		{"status_interval", func(c *Serve) { c.Status.Interval = 0 }, "status.interval: must be positive"},
		{"status_request_timeout", func(c *Serve) { c.Status.RequestTimeout = 0 }, "status.request_timeout: must be positive"},
		{"status_degraded_after", func(c *Serve) { c.Status.DegradedAfter = 0 }, "status.degraded_after: must be positive"},
		{"status_rabbitmq_service_empty", func(c *Serve) { c.Status.RabbitMQService = "" }, "status.rabbitmq_service: must not be empty"},
		{"status_rabbitmq_service_unknown_placeholder", func(c *Serve) { c.Status.RabbitMQService = "rabbitmq-{dc}" }, `status.rabbitmq_service: "rabbitmq-{dc}" supports only the {datacenter} placeholder`},
		{"status_rabbitmq_service_unclosed_placeholder", func(c *Serve) { c.Status.RabbitMQService = "rabbitmq-{datacenter" }, `status.rabbitmq_service: "rabbitmq-{datacenter" supports only the {datacenter} placeholder`},
		{"status_info_services_blank", func(c *Serve) { c.Status.InfoServices = []string{"engine", ""} }, "status.info_services[1]: must not be empty"},

		{"journal_max_entries_zero", func(c *Serve) { c.Journal.MaxEntries = 0 }, "journal.max_entries: must be positive"},
		{"journal_align_interval_zero", func(c *Serve) { c.Journal.AlignInterval = 0 }, "journal.align_interval: must be positive"},
		{"ui_dir_relative", func(c *Serve) { c.UI.Dir = "ui/dist" }, `ui.dir: "ui/dist" is not an absolute path`},

		{"arbiter_address_on_stretch", func(c *Serve) { c.Profile = ProfileStretch }, "arbiter.address: must be empty for profile stretch"},
		{"arbiter_address_no_port", func(c *Serve) { c.Arbiter.Address = "10.9.0.10" }, `arbiter.address: "10.9.0.10" is not a host:port address`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validServe(t)
			tt.mutate(cfg)

			assert.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}

func TestServeValidate_reportsAllViolations(t *testing.T) {
	cfg := validServe(t)
	cfg.Profile = ""
	cfg.Auth.Users = nil

	err := cfg.Validate()
	assert.ErrorContains(t, err, "profile:")
	assert.ErrorContains(t, err, "auth.users:")
}

func TestServeValidate_hidesSecrets(t *testing.T) {
	cfg := validServe(t)
	cfg.Auth.Users[0].PasswordHash = "plaintext-password"
	cfg.Patroni.Username = ""

	err := cfg.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "plaintext-password")
	assert.NotContains(t, err.Error(), cfg.Patroni.Password)
}

func TestStatusRabbitMQServiceFor(t *testing.T) {
	tests := []struct {
		service    string
		datacenter string
		want       string
	}{
		{"rabbitmq", "dc_b", "rabbitmq"},
		{"rabbitmq-{datacenter}", "dc_b", "rabbitmq-dc_b"},
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			s := Status{RabbitMQService: tt.service}
			assert.Equal(t, tt.want, s.RabbitMQServiceFor(tt.datacenter))
		})
	}
}

func TestLoadServe(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(valid, []byte(renderedServe), 0o600))

	cfg, err := LoadServe(valid)
	require.NoError(t, err)
	assert.Equal(t, "dc_a", cfg.Datacenter)

	invalid := filepath.Join(dir, "invalid.yaml")
	require.NoError(t, os.WriteFile(invalid, []byte(renderedServe+"journal:\n  max_entries: 0\n"), 0o600))

	_, err = LoadServe(invalid)
	assert.ErrorContains(t, err, invalid)
	assert.ErrorContains(t, err, "journal.max_entries")

	_, err = LoadServe(filepath.Join(dir, "missing.yaml"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}
