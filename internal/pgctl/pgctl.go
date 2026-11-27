// Package pgctl changes the Patroni clusters of the datacenters: demote and promote a
// cluster, point a standby cluster at another primary, switch the leader over inside a
// cluster, and wait for a standby to catch up or to follow.
//
// A primitive checks that the cluster is the one to change, asks the [Guard], sends one
// request to the leader and polls until the completion condition is met or its timeout
// from patroni.timeouts runs out. Each one is a step of an operation:
//
//   - [Controller.Demote], PATRONI_DEMOTE, within demote: the leader is standby_leader by
//     GET /cluster and by its own GET /patroni, no member reports primary, standby_cluster
//     points at the target, and the datacenter has no healthy primary instance in Consul;
//   - [Controller.Catchup], PATRONI_CATCHUP, within catchup: the standby leader of the
//     target, on the timeline of the demoted leader, received at least its
//     replayed_location and replayed all it received;
//   - [Controller.NoPrimary], PATRONI_NO_PRIMARY, within catchup: no cluster of the
//     datacenters has a primary, by GET /patroni of their members and in Consul; the
//     clusters it does not need may stay unread;
//   - [Controller.Promote], PATRONI_PROMOTE, within promote: the leader is primary on a
//     newer timeline, GET /primary answers 200, standby_cluster is gone from the dynamic
//     configuration and the leader is the only primary, by GET /patroni and in Consul;
//   - [Controller.Repoint], PATRONI_REPOINT, within repoint, and [Controller.Follow],
//     PATRONI_FOLLOW, within rewind: the standby leader streams on the timeline of the
//     primary, and the primary streams to its address;
//   - [Controller.Recover], PATRONI_FOLLOW of demote, within rewind: the condition of
//     Follow, and every other member and instance of the datacenter a replica that
//     streams on that timeline; [Controller.Recovered] reads it once;
//   - [Controller.Switchover], PATRONI_SWITCHOVER, within promote: the candidate leads on
//     a newer timeline and is the only primary, by GET /patroni and in Consul;
//   - [Controller.Failover], PATRONI_FAILOVER, within promote: the candidate leads;
//   - [Controller.Rejoin], PATRONI_FOLLOW in stretch, within rewind: the members of a
//     datacenter stream as replicas on the timeline of the leader.
//
// [Controller.Leader], [Controller.Divergence], [Controller.DivergenceOf],
// [Controller.Demoted], [Controller.DemoteLoss] and [Controller.Place] read the clusters
// once, for the decisions of a step: what the leader received, whether a standby or an
// old primary left the history of the primary, whether a cluster is a standby of another
// already, what demoting a primary discards, and where the members of the cluster of
// stretch run.
//
// The timeline is taken from GET /patroni of the member itself: GET /cluster comes from
// the DCS, which lags by loop_wait.
//
// A read that fails leaves the condition unmet, never met. A request with an unknown
// outcome, a timeout or a transport error after it was sent, does not end the primitive:
// the condition is polled until the deadline, and a PATCH /config is sent again, after
// the same checks, while the condition is not met. A 4xx answer ends the primitive only
// when no earlier attempt had an unknown outcome.
package pgctl

import (
	"context"
	"errors"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

const (
	pollInterval = 2 * time.Second
	// readTimeout bounds the reads of one poll.
	readTimeout = 10 * time.Second
	// resendInterval is how often a PATCH /config with an unknown outcome is sent again.
	resendInterval = 10 * time.Second
	// postgresPort is the PostgreSQL port of every member, set by the deployment.
	postgresPort = 5432
	tagPrimary   = "primary"
)

var (
	// ErrTimeout means the condition was not met before the deadline.
	ErrTimeout = errors.New("patroni did not reach the condition in time")
	// ErrOutcomeUnknown means the request may or may not have been applied, and the
	// condition was not met before the deadline. The error matches [ErrTimeout] too, so
	// check this one first.
	ErrOutcomeUnknown = errors.New("outcome of the patroni request is unknown")
	// ErrGuard wraps the error of the [Guard] that ended a primitive.
	ErrGuard = errors.New("guard refused")
	// ErrRefused means Patroni answered the request with a 4xx status: nothing was changed.
	ErrRefused = errors.New("patroni refused the request")
	// ErrPrecondition means the cluster is not the one the primitive may change, or is in
	// a state it cannot start from; nothing was sent.
	ErrPrecondition = errors.New("patroni cluster does not meet the precondition")
)

// Patroni is the Patroni cluster seen from one datacenter: Members discovers the members
// of the cluster there and reads GET /patroni of each, the other methods go to one
// member. [*patroni.Client] implements it.
type Patroni interface {
	Members(ctx context.Context) ([]patroni.Member, error)
	Primary(ctx context.Context, endpoint string) (bool, error)
	Config(ctx context.Context, endpoint string) (map[string]any, error)
	History(ctx context.Context, endpoint string) ([]patroni.HistoryEntry, error)
	PatchConfig(ctx context.Context, endpoint string, patch map[string]any) (map[string]any, error)
	Switchover(ctx context.Context, endpoint, leader, candidate string) (string, error)
	Failover(ctx context.Context, endpoint, candidate string) (string, error)
}

// Catalog is the Consul catalog of every datacenter. Its reads must not be stale: an
// instance gone from a server cut off from the leader would read as no primary.
// [*consul.Clients] implements it.
type Catalog interface {
	// HealthyAddrs returns host:port of the passing instances of service with tag in dc.
	HealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error)
	// RegisteredAddrs returns host:port of every instance of service in dc.
	RegisteredAddrs(ctx context.Context, dc, service string) ([]string, error)
	// ClusterHealthyAddrs is HealthyAddrs through the Consul of dc without the
	// datacenter filter; in stretch it covers the whole cluster.
	ClusterHealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error)
}

// Cluster is the Patroni cluster of a datacenter. In stretch, where one cluster spans
// every datacenter, it is that cluster with the datacenter the primitive is about.
type Cluster struct {
	Datacenter string
	Patroni    Patroni
}

// Guard confirms that the caller may still change the cluster, for example that it holds
// the lease of the current generation. It runs before each request; an error ends the
// primitive at once with [ErrGuard]. An error of ctx should be returned as it is: a guard
// cut by the deadline ends the primitive as a timeout, not as a refusal.
type Guard func(ctx context.Context) error

// Options are the caller's part of a primitive.
type Options struct {
	// Guard is required by the primitives that send a request.
	Guard Guard
	// Progress, when set, receives every [Progress] from the goroutine running the
	// primitive, which waits for it to return.
	Progress func(Progress)
}

// Controller runs the primitives with the scope and the timeouts of patroni in the config.
type Controller struct {
	cfg     config.Patroni
	catalog Catalog
	log     *logging.Logger
}

// New returns the primitives for cfg.
func New(cfg config.Patroni, catalog Catalog, log *logging.Logger) (*Controller, error) {
	t := cfg.Timeouts

	switch {
	case cfg.Scope == "":
		return nil, errors.New("no patroni scope")
	case t.Demote <= 0, t.Catchup <= 0, t.Promote <= 0, t.Repoint <= 0, t.Rewind <= 0:
		return nil, errors.New("patroni timeouts must be positive")
	case catalog == nil:
		return nil, errors.New("no consul catalog")
	case log == nil:
		return nil, errors.New("no logger")
	}

	return &Controller{cfg: cfg, catalog: catalog, log: log}, nil
}
