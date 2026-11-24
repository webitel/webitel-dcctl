// Package collect is the collection loop of dcctl: on every cycle it reads the Consul KV,
// catalog, Nomad and Patroni of every datacenter, and the arbiter, and publishes what it
// read as an immutable [Snapshot]. Status, metrics, health and fencing read the snapshot
// and never the sources.
package collect

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// staleCycles is how many cycles, each with its pause and its longest wait for the
// sources, may pass without a finished one before the collector is not live.
const staleCycles = 3

// Config configures a [Collector].
type Config struct {
	Profile config.Profile
	// Instance is this dcctl instance, the observer of every snapshot.
	Instance Instance
	// Status gives the pause between cycles, the longest wait for a source, the RabbitMQ
	// service and the informational services.
	Status config.Status
	// Scope is the Patroni scope, which is also its service in Consul.
	Scope string
	// NodeMetaKey is the node_meta key that holds the datacenter of a node.
	NodeMetaKey string
}

// Collector runs the collection loop and holds the last snapshot.
type Collector struct {
	cfg     Config
	src     Sources
	log     *logging.Logger
	created time.Time

	current atomic.Pointer[published]
}

// published is a snapshot with the channel closed when the next one replaces it.
type published struct {
	snap *Snapshot
	next chan struct{}
}

// New returns a collector that reads src. It does no I/O; [Collector.Run] starts the loop.
func New(cfg Config, src Sources, log *logging.Logger) *Collector {
	c := &Collector{cfg: cfg, src: src, log: log, created: time.Now()}
	c.current.Store(&published{next: make(chan struct{})})

	return c
}

// Run collects until ctx is done. A cycle starts Status.Interval after the previous one
// finished. Call it once.
func (c *Collector) Run(ctx context.Context) {
	for {
		if snap, ok := c.cycle(ctx, c.Snapshot()); ok {
			c.publish(snap)
		}

		t := time.NewTimer(c.cfg.Status.Interval)

		select {
		case <-ctx.Done():
			t.Stop()

			return
		case <-t.C:
		}
	}
}

// Snapshot returns the last snapshot, or nil before the first cycle finished.
func (c *Collector) Snapshot() *Snapshot {
	return c.current.Load().snap
}

// Watch returns the last snapshot, nil before the first one, and a channel closed when a
// newer snapshot is published. Once [Collector.Run] returns, no snapshot follows and the
// last channel is never closed, so a caller also waits on its own context.
func (c *Collector) Watch() (*Snapshot, <-chan struct{}) {
	p := c.current.Load()

	return p.snap, p.next
}

// Check is the liveness check of the loop: it fails when no cycle finished for longer
// than three times the pause plus the request timeout.
func (c *Collector) Check(context.Context) error {
	last := c.created
	if s := c.Snapshot(); s != nil {
		last = s.ObserveTime
	}

	limit := staleCycles * (c.cfg.Status.Interval + c.cfg.Status.RequestTimeout)

	if age := time.Since(last); age > limit {
		return fmt.Errorf("no collection cycle finished for %s, the limit is %s", age.Truncate(time.Second), limit)
	}

	return nil
}

// publish is called by the loop only, so a plain store does not lose a snapshot.
func (c *Collector) publish(s *Snapshot) {
	prev := c.current.Load()
	c.current.Store(&published{snap: s, next: make(chan struct{})})
	close(prev.next)
}
