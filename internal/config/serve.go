package config

import (
	"strings"
	"time"
)

// DefaultServePath is where Ansible renders the serve config.
const DefaultServePath = "/etc/webitel/dcctl/config.yaml"

// Profile is the topology profile of the installation.
type Profile string

const (
	ProfileWarmStandby Profile = "warm_standby"
	ProfileStretch     Profile = "stretch"
)

// Serve is the config of "webitel-dcctl serve".
type Serve struct {
	Profile      Profile       `yaml:"profile"`
	Datacenter   string        `yaml:"datacenter"`
	Listen       ServeListen   `yaml:"listen"`
	TLS          TLS           `yaml:"tls"`
	Auth         Auth          `yaml:"auth"`
	Consul       Consul        `yaml:"consul"`
	Patroni      Patroni       `yaml:"patroni"`
	Coordination Coordination  `yaml:"coordination"`
	Apps         Apps          `yaml:"apps"`
	Status       Status        `yaml:"status"`
	Journal      Journal       `yaml:"journal"`
	Arbiter      ArbiterClient `yaml:"arbiter"`
}

// ServeListen holds the listener addresses of serve.
type ServeListen struct {
	// HTTP serves gRPC, grpc-gateway and UI with basic auth; loopback only.
	HTTP string `yaml:"http"`
	// Peer serves gRPC with mTLS for other dcctl instances.
	Peer string `yaml:"peer"`
	// Health serves the unauthenticated probes; loopback only.
	Health string `yaml:"health"`
}

// TLS holds the PEM file paths of a node certificate and its CA.
type TLS struct {
	CA   string `yaml:"ca"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// Auth holds the basic auth users.
type Auth struct {
	Users []User `yaml:"users"`
}

// User is a basic auth user.
type User struct {
	Name         string `yaml:"name"`
	PasswordHash string `yaml:"password_hash"`
	Role         Role   `yaml:"role"`
}

// Role is what a user is allowed to do.
type Role string

// RoleOperator is the only role in v1; it allows every action.
const RoleOperator Role = "operator"

// Consul lists the Consul servers of every datacenter.
type Consul struct {
	// NodeMetaKey is the node_meta key that holds the datacenter of a node.
	NodeMetaKey string         `yaml:"node_meta_key"`
	Servers     []ConsulServer `yaml:"servers"`
}

// ConsulServer is a Consul HTTP API address and the datacenter it belongs to.
type ConsulServer struct {
	Address    string `yaml:"address"`
	Datacenter string `yaml:"datacenter"`
}

// Patroni holds the Patroni REST API settings.
type Patroni struct {
	Scope    string `yaml:"scope"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Endpoints is a static initial list of members as host:port of their
	// REST API (always https), used instead of the Consul catalog when set.
	Endpoints          []string        `yaml:"endpoints"`
	PlannedMaxLagBytes int64           `yaml:"planned_max_lag_bytes"`
	Timeouts           PatroniTimeouts `yaml:"timeouts"`
}

// PatroniTimeouts bounds each Patroni primitive.
type PatroniTimeouts struct {
	Demote  time.Duration `yaml:"demote"`
	Catchup time.Duration `yaml:"catchup"`
	Promote time.Duration `yaml:"promote"`
	Repoint time.Duration `yaml:"repoint"`
	Rewind  time.Duration `yaml:"rewind"`
}

// Coordination holds the Consul lock and lease timings.
type Coordination struct {
	SessionTTL        time.Duration `yaml:"session_ttl"`
	LockDelay         time.Duration `yaml:"lock_delay"`
	LeasePeriod       time.Duration `yaml:"lease_period"`
	LeaseAcquireAfter time.Duration `yaml:"lease_acquire_after"`
}

// Apps holds the Nomad job timings.
type Apps struct {
	StopTimeout         time.Duration `yaml:"stop_timeout"`
	StageStartTimeout   time.Duration `yaml:"stage_start_timeout"`
	FailoverStopTimeout time.Duration `yaml:"failover_stop_timeout"`
	MinHealthyTime      time.Duration `yaml:"min_healthy_time"`
}

// Status holds the state collection settings.
type Status struct {
	Interval       time.Duration `yaml:"interval"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	DegradedAfter  time.Duration `yaml:"degraded_after"`
	// RabbitMQService is the RabbitMQ service name in Consul; see RabbitMQServiceFor.
	RabbitMQService string   `yaml:"rabbitmq_service"`
	InfoServices    []string `yaml:"info_services"`
}

// datacenterPlaceholder in Status.RabbitMQService is replaced with the
// datacenter being checked.
const datacenterPlaceholder = "{datacenter}"

// RabbitMQServiceFor returns the RabbitMQ service name of datacenter.
func (s Status) RabbitMQServiceFor(datacenter string) string {
	return strings.ReplaceAll(s.RabbitMQService, datacenterPlaceholder, datacenter)
}

// Journal holds the journal settings.
type Journal struct {
	// MaxEntries caps the journal entries kept in the KV of each datacenter.
	MaxEntries int `yaml:"max_entries"`
}

// ArbiterClient is the arbiter serve connects to; empty Address means no arbiter.
type ArbiterClient struct {
	Address string `yaml:"address"`
}

func defaultServe() Serve {
	return Serve{
		Patroni: Patroni{
			PlannedMaxLagBytes: 1 << 20,
			Timeouts: PatroniTimeouts{
				Demote:  120 * time.Second,
				Catchup: 60 * time.Second,
				Promote: 120 * time.Second,
				Repoint: 300 * time.Second,
				Rewind:  600 * time.Second,
			},
		},
		Coordination: Coordination{
			SessionTTL:        15 * time.Second,
			LockDelay:         15 * time.Second,
			LeasePeriod:       30 * time.Second,
			LeaseAcquireAfter: 30 * time.Second,
		},
		Apps: Apps{
			StopTimeout:         120 * time.Second,
			StageStartTimeout:   180 * time.Second,
			FailoverStopTimeout: 30 * time.Second,
			MinHealthyTime:      10 * time.Second,
		},
		Status: Status{
			Interval:        5 * time.Second,
			RequestTimeout:  3 * time.Second,
			DegradedAfter:   60 * time.Second,
			RabbitMQService: "rabbitmq",
			InfoServices:    []string{"opensips", "engine", "storage", "grafana"},
		},
		Journal: Journal{MaxEntries: 1000},
	}
}

func parseServe(data []byte) (*Serve, error) {
	cfg := defaultServe()
	if err := decode(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
