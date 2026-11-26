package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderedServe holds only the keys the Ansible role renders, plus auth.
const renderedServe = `
profile: warm_standby
datacenter: dc_a
listen:
  http: 127.0.0.1:10022
  peer: 10.1.0.81:10023
  health: 127.0.0.1:10025
tls:
  ca: /etc/ssl/app/ca.pem
  cert: /etc/ssl/app/dc_a_nginx.pem
  key: /etc/ssl/app/dc_a_nginx-key.pem
auth:
  users:
    - name: support
      password_hash: "$2b$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW"
      role: operator
consul:
  node_meta_key: webitel_datacenter
  servers:
    - address: 10.1.0.89:8500
      datacenter: dc_a
    - address: 10.2.0.89:8500
      datacenter: dc_b
patroni:
  scope: webitel-postgres
  username: patroni
  password: secret
status:
  rabbitmq_service: rabbitmq
arbiter:
  address: 10.9.0.10:10024
`

func TestParseServe_defaults(t *testing.T) {
	got, err := parseServe([]byte(renderedServe))
	require.NoError(t, err)

	assert.Equal(t, Apps{
		StopTimeout:         120 * time.Second,
		StageStartTimeout:   180 * time.Second,
		FailoverStopTimeout: 30 * time.Second,
		MinHealthyTime:      10 * time.Second,
	}, got.Apps)
	assert.Equal(t, int64(1048576), got.Patroni.PlannedMaxLagBytes)
	assert.Equal(t, PatroniTimeouts{
		Demote:  120 * time.Second,
		Catchup: 60 * time.Second,
		Promote: 120 * time.Second,
		Repoint: 300 * time.Second,
		Rewind:  600 * time.Second,
	}, got.Patroni.Timeouts)
	assert.Equal(t, Coordination{
		SessionTTL:        15 * time.Second,
		LockDelay:         15 * time.Second,
		LeasePeriod:       30 * time.Second,
		LeaseAcquireAfter: 30 * time.Second,
	}, got.Coordination)
	assert.Equal(t, Status{
		Interval:        5 * time.Second,
		RequestTimeout:  3 * time.Second,
		DegradedAfter:   60 * time.Second,
		RabbitMQService: "rabbitmq",
		InfoServices:    []string{"opensips", "engine", "storage", "grafana"},
	}, got.Status)
	assert.Equal(t, Journal{MaxEntries: 1000, AlignInterval: time.Minute}, got.Journal)
}

func TestParseServe_rendered(t *testing.T) {
	got, err := parseServe([]byte(renderedServe))
	require.NoError(t, err)

	assert.Equal(t, ProfileWarmStandby, got.Profile)
	assert.Equal(t, "dc_a", got.Datacenter)
	assert.Equal(t, ServeListen{
		HTTP:   "127.0.0.1:10022",
		Peer:   "10.1.0.81:10023",
		Health: "127.0.0.1:10025",
	}, got.Listen)
	assert.Equal(t, []User{{
		Name:         "support",
		PasswordHash: "$2b$04$AUJV.yvHyWu9LFhtKITeVe9BxDVcAbATExWTJydZmwFgjLQTijFUW",
		Role:         "operator",
	}}, got.Auth.Users)
	assert.Equal(t, []ConsulServer{
		{Address: "10.1.0.89:8500", Datacenter: "dc_a"},
		{Address: "10.2.0.89:8500", Datacenter: "dc_b"},
	}, got.Consul.Servers)
	assert.Equal(t, "webitel-postgres", got.Patroni.Scope)
	assert.Equal(t, "10.9.0.10:10024", got.Arbiter.Address)
}

func TestParseServe_overridesDefault(t *testing.T) {
	got, err := parseServe([]byte(renderedServe + "apps:\n  stop_timeout: 2m\n"))
	require.NoError(t, err)

	assert.Equal(t, 2*time.Minute, got.Apps.StopTimeout)
	assert.Equal(t, 180*time.Second, got.Apps.StageStartTimeout)
}

func TestParseServe_errors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown_top_level_key",
			yaml: renderedServe + "metrics: true\n",
			want: "field metrics not found",
		},
		{
			name: "unknown_nested_key",
			yaml: renderedServe + "apps:\n  stop_timout: 2m\n",
			want: "field stop_timout not found",
		},
		{
			name: "unknown_key_in_list_item",
			yaml: "consul:\n  servers:\n    - address: 10.1.0.89:8500\n      dc: dc_a\n",
			want: "field dc not found",
		},
		{
			name: "duration_without_unit",
			yaml: renderedServe + "apps:\n  stop_timeout: 120\n",
			want: "cannot unmarshal",
		},
		{
			name: "empty_file",
			yaml: "",
			want: "empty",
		},
		{
			name: "multiple_documents",
			yaml: renderedServe + "---\njournal:\n  max_entries: 10\n",
			want: "config must contain a single YAML document",
		},
		{
			name: "only_comments",
			yaml: "# nothing here\n",
			want: "empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseServe([]byte(tt.yaml))
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
