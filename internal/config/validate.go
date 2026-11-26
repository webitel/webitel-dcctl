package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// maxBcryptCost bounds the work an unauthenticated login attempt can cause.
const maxBcryptCost = 14

var bcryptHash = regexp.MustCompile(`^\$2[aby]\$\d{2}\$[./A-Za-z0-9]{53}$`)

// Validate reports every invalid value of c, one per line.
func (c *Serve) Validate() error {
	var v violations

	if c.Profile != ProfileWarmStandby && c.Profile != ProfileStretch {
		v.add("profile", "%q is not one of %s, %s", c.Profile, ProfileWarmStandby, ProfileStretch)
	}

	v.datacenter(c.Datacenter, c.Consul.Servers)

	v.loopback("listen.http", c.Listen.HTTP)
	v.nodeAddr("listen.peer", c.Listen.Peer)
	v.loopback("listen.health", c.Listen.Health)
	v.tls(c.TLS)
	v.users(c.Auth.Users)
	v.consul(c.Consul)
	v.patroni(c.Patroni)

	// Both datacenters of warm_standby run a cluster of the same scope, so one static list
	// of seeds would read the members of one datacenter as those of the other.
	if len(c.Patroni.Endpoints) > 0 && c.Profile == ProfileWarmStandby {
		v.add("patroni.endpoints", "must be empty for profile %s", ProfileWarmStandby)
	}

	v.sessionTTL(c.Coordination.SessionTTL)
	v.lockDelay(c.Coordination.LockDelay)
	v.positive("coordination.lease_period", c.Coordination.LeasePeriod)
	v.positive("coordination.lease_acquire_after", c.Coordination.LeaseAcquireAfter)

	v.positive("apps.stop_timeout", c.Apps.StopTimeout)
	v.positive("apps.stage_start_timeout", c.Apps.StageStartTimeout)
	v.positive("apps.failover_stop_timeout", c.Apps.FailoverStopTimeout)
	v.positive("apps.min_healthy_time", c.Apps.MinHealthyTime)

	v.status(c.Status)
	v.lockTimings(c.Coordination, c.Status.RequestTimeout)
	v.leaseTimings(c.Coordination, c.Status.RequestTimeout)

	if c.Journal.MaxEntries <= 0 {
		v.add("journal.max_entries", "must be positive")
	}

	v.positive("journal.align_interval", c.Journal.AlignInterval)

	if c.Arbiter.Address != "" {
		if c.Profile == ProfileStretch {
			v.add("arbiter.address", "must be empty for profile %s", ProfileStretch)
		}

		v.hostPort("arbiter.address", c.Arbiter.Address)
	}

	if c.UI.Dir != "" && !filepath.IsAbs(c.UI.Dir) {
		v.add("ui.dir", "%q is not an absolute path", c.UI.Dir)
	}

	return errors.Join(v...)
}

type violations []error

func (v *violations) add(key, format string, args ...any) {
	*v = append(*v, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func (v *violations) required(key, value string) bool {
	if value == "" {
		v.add(key, "must not be empty")

		return false
	}

	return true
}

func (v *violations) positive(key string, d time.Duration) {
	if d <= 0 {
		v.add(key, "must be positive")
	}
}

func (v *violations) addrPort(key, value string) (netip.AddrPort, bool) {
	if !v.required(key, value) {
		return netip.AddrPort{}, false
	}

	ap, err := netip.ParseAddrPort(value)
	if err != nil {
		v.add(key, "%q is not an IP:port address", value)

		return netip.AddrPort{}, false
	}

	if ap.Port() == 0 {
		v.add(key, "%q has no port", value)

		return netip.AddrPort{}, false
	}

	return ap, true
}

func (v *violations) loopback(key, value string) {
	if ap, ok := v.addrPort(key, value); ok && !ap.Addr().IsLoopback() {
		v.add(key, "%q is not a loopback address", value)
	}
}

// nodeAddr requires an address other nodes can reach: it is advertised in
// Consul or dialed by other nodes as is.
func (v *violations) nodeAddr(key, value string) {
	ap, ok := v.addrPort(key, value)
	if !ok {
		return
	}

	if a := ap.Addr(); a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		v.add(key, "%q is not a node address", value)
	}
}

func (v *violations) hostPort(key, value string) {
	if !v.required(key, value) {
		return
	}

	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || !validPort(port) {
		v.add(key, "%q is not a host:port address", value)
	}
}

func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)

	return err == nil && n != 0
}

func (v *violations) tls(t TLS) {
	v.required("tls.ca", t.CA)
	v.required("tls.cert", t.Cert)
	v.required("tls.key", t.Key)
}

func (v *violations) datacenter(dc string, servers []ConsulServer) {
	if !v.required("datacenter", dc) {
		return
	}

	known := make([]string, 0, len(servers))
	for _, s := range servers {
		if !slices.Contains(known, s.Datacenter) {
			known = append(known, s.Datacenter)
		}
	}

	if !slices.Contains(known, dc) {
		v.add("datacenter", "%q is not among consul.servers[].datacenter %v", dc, known)
	}
}

// users never puts a password hash into an error.
func (v *violations) users(users []User) {
	if len(users) == 0 {
		v.add("auth.users", "must not be empty")

		return
	}

	seen := make(map[string]bool, len(users))

	for i, u := range users {
		key := fmt.Sprintf("auth.users[%d]", i)

		if v.required(key+".name", u.Name) {
			switch {
			case strings.Contains(u.Name, ":"):
				v.add(key+".name", "%q contains ':'", u.Name)
			case seen[u.Name]:
				v.add(key+".name", "duplicate %q", u.Name)
			}

			seen[u.Name] = true
		}

		if v.required(key+".password_hash", u.PasswordHash) {
			v.bcrypt(key+".password_hash", u.PasswordHash)
		}

		if u.Role != RoleOperator {
			v.add(key+".role", "%q is not one of %s", u.Role, RoleOperator)
		}
	}
}

// bcrypt never puts the hash into an error.
func (v *violations) bcrypt(key, hash string) {
	if !bcryptHash.MatchString(hash) {
		v.add(key, "is not a bcrypt hash")

		return
	}

	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost > maxBcryptCost {
		v.add(key, "bcrypt cost must be within %d..%d", bcrypt.MinCost, maxBcryptCost)
	}
}

// Consul answers 500 to a session outside these bounds, which would look like an
// unreachable server.
const (
	minSessionTTL = 10 * time.Second
	maxSessionTTL = 24 * time.Hour
	maxLockDelay  = 60 * time.Second
)

func (v *violations) sessionTTL(d time.Duration) {
	if d < minSessionTTL || d > maxSessionTTL {
		v.add("coordination.session_ttl", "must be within %s..%s, the Consul limits", minSessionTTL, maxSessionTTL)
	}
}

func (v *violations) lockDelay(d time.Duration) {
	if d <= 0 || d > maxLockDelay {
		v.add("coordination.lock_delay", "must be greater than 0 and at most %s, the Consul limit", maxLockDelay)
	}
}

// lockTimings checks that a lost lock session is noticed before Consul lets another
// holder take the lock: the holder learns of it at its next renewal, at most one
// renewal interval and one request later, and the lock delay holds the key until then.
func (v *violations) lockTimings(c Coordination, requestTimeout time.Duration) {
	if c.SessionTTL < minSessionTTL || c.LockDelay <= 0 || requestTimeout <= 0 {
		return
	}

	renewal := c.SessionTTL / RenewalsPerSessionTTL

	if requestTimeout >= renewal {
		v.add("status.request_timeout", "must be less than coordination.session_ttl/%d (%s)", RenewalsPerSessionTTL, renewal)

		return
	}

	if c.LockDelay <= renewal+requestTimeout {
		v.add("coordination.lock_delay", "must be greater than coordination.session_ttl/%d + status.request_timeout (%s)",
			RenewalsPerSessionTTL, renewal+requestTimeout)
	}
}

// leaseRoundRequests is how many voter requests, each bounded by status.request_timeout,
// one lease renewal must fit: reading the state, reading the lease and a write that may be
// retried after a re-read.
const leaseRoundRequests = 3

// leaseTimings checks that one lease renewal round fits within the renewal interval.
func (v *violations) leaseTimings(c Coordination, requestTimeout time.Duration) {
	if c.LeasePeriod <= 0 || requestTimeout <= 0 {
		return
	}

	if c.LeasePeriod/LeaseRenewalsPerPeriod < leaseRoundRequests*requestTimeout {
		v.add("coordination.lease_period", "must be at least %d × %d × status.request_timeout (%s)",
			LeaseRenewalsPerPeriod, leaseRoundRequests, LeaseRenewalsPerPeriod*leaseRoundRequests*requestTimeout)
	}
}

func (v *violations) consul(c Consul) {
	v.required("consul.node_meta_key", c.NodeMetaKey)

	if len(c.Servers) == 0 {
		v.add("consul.servers", "must not be empty")
	}

	for i, s := range c.Servers {
		key := fmt.Sprintf("consul.servers[%d]", i)
		v.hostPort(key+".address", s.Address)
		v.required(key+".datacenter", s.Datacenter)
	}
}

// patroni never puts the password into an error.
func (v *violations) patroni(p Patroni) {
	v.required("patroni.scope", p.Scope)

	switch {
	case p.Username == "" && p.Password != "":
		v.add("patroni.username", "must be set together with patroni.password")
	case p.Username != "" && p.Password == "":
		v.add("patroni.password", "must be set together with patroni.username")
	}

	for i, e := range p.Endpoints {
		v.hostPort(fmt.Sprintf("patroni.endpoints[%d]", i), e)
	}

	if p.PlannedMaxLagBytes < 0 {
		v.add("patroni.planned_max_lag_bytes", "must not be negative")
	}

	v.positive("patroni.timeouts.demote", p.Timeouts.Demote)
	v.positive("patroni.timeouts.catchup", p.Timeouts.Catchup)
	v.positive("patroni.timeouts.promote", p.Timeouts.Promote)
	v.positive("patroni.timeouts.repoint", p.Timeouts.Repoint)
	v.positive("patroni.timeouts.rewind", p.Timeouts.Rewind)
}

func (v *violations) status(s Status) {
	v.positive("status.interval", s.Interval)
	v.positive("status.request_timeout", s.RequestTimeout)
	v.positive("status.degraded_after", s.DegradedAfter)

	if v.required("status.rabbitmq_service", s.RabbitMQService) &&
		strings.ContainsAny(strings.ReplaceAll(s.RabbitMQService, datacenterPlaceholder, ""), "{}") {
		v.add("status.rabbitmq_service", "%q supports only the %s placeholder", s.RabbitMQService, datacenterPlaceholder)
	}

	for i, name := range s.InfoServices {
		v.required(fmt.Sprintf("status.info_services[%d]", i), name)
	}
}
