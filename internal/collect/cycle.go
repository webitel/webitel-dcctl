package collect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/buildinfo"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

const (
	// apiQuery is the prepared query nginx resolves the API backends with.
	apiQuery = "webitel-api"
	// allocsParallelism bounds the allocation reads of one Nomad cluster running at once.
	allocsParallelism = 8
)

// cycle reads every source once on top of prev, which may be nil, numbers the snapshot
// and logs the sources that stopped or started answering. It reports false when parent
// was canceled before the cycle finished.
func (c *Collector) cycle(parent context.Context, prev *Snapshot) (*Snapshot, bool) {
	if prev == nil {
		prev = c.empty()
	}

	next, ok := c.read(parent, prev)
	if !ok {
		return nil, false
	}

	next.Seq = prev.Seq + 1
	c.logChanges(parent, prev, next)

	return next, true
}

// read reads every source once, all at the same time and none for longer than
// Status.RequestTimeout, and builds a snapshot in which a source that fails keeps its
// data of prev. It reports false when parent was canceled before the reads finished.
func (c *Collector) read(parent context.Context, prev *Snapshot) (*Snapshot, bool) {
	begin := time.Now()

	ctx, cancel := context.WithTimeout(parent, c.cfg.Status.RequestTimeout)
	defer cancel()

	next := c.empty()

	// Every goroutine writes only its own source of next.
	var wg sync.WaitGroup

	for i, v := range c.src.Voters {
		wg.Go(func() {
			data, err := readKV(ctx, v.KV)
			next.Voters[i].KV = observe(prev.Voters[i].KV, data, err, time.Now())
		})
	}

	if c.src.Arbiter != nil {
		wg.Go(func() {
			data, err := readArbiter(ctx, c.src.Arbiter)
			*next.Arbiter = observe(*prev.Arbiter, data, err, time.Now())
		})
	}

	for i, d := range c.src.Datacenters {
		wg.Go(func() {
			data, err := c.readServices(ctx, d)
			next.Datacenters[i].Services = observe(prev.Datacenters[i].Services, data, err, time.Now())
		})
		wg.Go(func() {
			data, err := readNomad(ctx, d.Nomad)
			next.Datacenters[i].Nomad = observe(prev.Datacenters[i].Nomad, data, err, time.Now())
		})
	}

	for i, cl := range c.src.Clusters {
		wg.Go(func() {
			data, err := readInstances(cl.Catalog.ClusterInstances(ctx, c.cfg.Scope, "", false))
			next.Clusters[i].Scope = observe(prev.Clusters[i].Scope, data, err, time.Now())
		})
		wg.Go(func() {
			data, err := readInstances(cl.Catalog.ExecuteQuery(ctx, apiQuery))
			next.Clusters[i].API = observe(prev.Clusters[i].API, data, err, time.Now())
		})
		wg.Go(func() {
			p := prev.Clusters[i].Patroni
			data, err := readPatroni(ctx, cl.Patroni, p.Data)
			next.Clusters[i].Patroni = observePatroni(p, data, err, time.Now())
		})
	}

	wg.Wait()

	if parent.Err() != nil {
		return nil, false
	}

	for i := range next.Clusters {
		placeMembers(&next.Clusters[i], c.cfg.NodeMetaKey)
	}

	next.ObserveTime = time.Now()
	next.CycleDuration = next.ObserveTime.Sub(begin)

	return next, true
}

// empty returns a snapshot with every source of the collector and nothing read.
func (c *Collector) empty() *Snapshot {
	s := &Snapshot{
		Observer:    c.cfg.Instance,
		Profile:     c.cfg.Profile,
		Voters:      make([]Voter, len(c.src.Voters)),
		Datacenters: make([]Datacenter, len(c.src.Datacenters)),
		Clusters:    make([]Cluster, len(c.src.Clusters)),
	}

	for i, v := range c.src.Voters {
		s.Voters[i].Datacenter = v.Datacenter
	}

	if c.src.Arbiter != nil {
		s.Arbiter = new(Source[Arbiter])
	}

	for i, d := range c.src.Datacenters {
		s.Datacenters[i].Name = d.Name
	}

	for i, cl := range c.src.Clusters {
		s.Clusters[i].Datacenters = cl.Datacenters
	}

	return s
}

func readKV(ctx context.Context, r KVReader) (KV, error) {
	keys := [...]string{kv.StateKey, kv.LeaseKey, kv.OpKey, kv.LockKey}

	var (
		entries [len(keys)]*consul.KVEntry
		metas   [len(keys)]consul.QueryMeta
	)

	g, ctx := errgroup.WithContext(ctx)

	for i, key := range keys {
		g.Go(func() error {
			e, meta, err := r.KVGet(ctx, key)
			if errors.Is(err, consul.ErrKeyNotFound) {
				err = nil
			}

			entries[i], metas[i] = e, meta

			return err
		})
	}

	if err := g.Wait(); err != nil {
		return KV{}, err
	}

	return KV{
		State: entries[0], Lease: entries[1], Op: entries[2], Lock: entries[3],
		Meta: leastCurrent(metas[:]...),
	}, nil
}

func readArbiter(ctx context.Context, r ArbiterReader) (Arbiter, error) {
	names := [...]string{arbiter.RecordState, arbiter.RecordLease}

	var (
		records       [len(names)]*arbiter.Record
		uninitialized [len(names)]bool
	)

	g, ctx := errgroup.WithContext(ctx)

	for i, name := range names {
		g.Go(func() error {
			rec, err := r.Get(ctx, name)

			switch {
			case errors.Is(err, arbiter.ErrUninitialized):
				uninitialized[i] = true
			case errors.Is(err, arbiter.ErrNotFound):
			case err != nil:
				return err
			default:
				records[i] = &rec
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return Arbiter{}, err
	}

	if uninitialized[0] || uninitialized[1] {
		return Arbiter{}, nil
	}

	return Arbiter{Initialized: true, State: records[0], Lease: records[1]}, nil
}

func (c *Collector) readServices(ctx context.Context, d DatacenterSource) (Services, error) {
	info := c.cfg.Status.InfoServices
	names := append([]string{
		nomad.ServiceName(c.cfg.Profile, d.Name),
		buildinfo.ServiceName,
		c.cfg.Status.RabbitMQServiceFor(d.Name),
	}, info...)

	lists := make([][]consul.ServiceNode, len(names))
	metas := make([]consul.QueryMeta, len(names))

	g, ctx := errgroup.WithContext(ctx)

	for i, name := range names {
		g.Go(func() error {
			nodes, meta, err := d.Services.Instances(ctx, name, "", false)
			lists[i], metas[i] = nodes, meta

			return err
		})
	}

	if err := g.Wait(); err != nil {
		return Services{}, err
	}

	s := Services{
		Nomad: lists[0], Dcctl: lists[1], RabbitMQ: lists[2],
		Info: make(map[string][]consul.ServiceNode, len(info)),
		Meta: leastCurrent(metas...),
	}

	for i, name := range info {
		s.Info[name] = lists[3+i]
	}

	return s, nil
}

func readInstances(nodes []consul.ServiceNode, meta consul.QueryMeta, err error) (Instances, error) {
	if err != nil {
		return Instances{}, err
	}

	return Instances{Nodes: nodes, Meta: meta}, nil
}

func readNomad(ctx context.Context, r NomadReader) (Nomad, error) {
	var n Nomad

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		version, err := r.Version(ctx)
		n.Version = version

		return err
	})
	g.Go(func() error {
		leader, endpoint, err := r.Leader(ctx)
		n.Leader, n.Endpoint = leader, endpoint

		return err
	})
	g.Go(func() error {
		nodes, err := r.Nodes(ctx)
		n.Nodes = nodes

		return err
	})
	g.Go(func() error {
		managed, unmanaged, err := r.Jobs(ctx)
		if err != nil {
			return err
		}

		n.Managed, n.Unmanaged = managed, unmanaged

		allocs, err := readAllocs(ctx, r, managed)
		n.Allocs = allocs

		return err
	})

	if err := g.Wait(); err != nil {
		return Nomad{}, err
	}

	return n, nil
}

func readAllocs(ctx context.Context, r NomadReader, jobs []nomad.Job) (map[string][]nomad.Alloc, error) {
	lists := make([][]nomad.Alloc, len(jobs))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(allocsParallelism)

	for i, job := range jobs {
		g.Go(func() error {
			allocs, err := r.Allocs(ctx, job.ID)
			lists[i] = allocs

			return err
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := make(map[string][]nomad.Alloc, len(jobs))
	for i, job := range jobs {
		out[job.ID] = lists[i]
	}

	return out, nil
}

// reachability is what the log needs of a [Source].
type reachability struct {
	reachable bool
	since     time.Time
	reason    source.Reason
	err       error
}

func reachabilityOf[T any](s Source[T]) reachability {
	return reachability{reachable: s.Reachable, since: s.UnreachableSince, reason: s.Reason, err: s.Err}
}

// logChanges logs the sources that stopped or started answering in this cycle.
func (c *Collector) logChanges(ctx context.Context, prev, next *Snapshot) {
	for i, v := range next.Voters {
		c.logChange(ctx, "consul_kv", v.Datacenter, reachabilityOf(prev.Voters[i].KV), reachabilityOf(v.KV))
	}

	if next.Arbiter != nil {
		c.logChange(ctx, "arbiter", "", reachabilityOf(*prev.Arbiter), reachabilityOf(*next.Arbiter))
	}

	for i, d := range next.Datacenters {
		c.logChange(ctx, "consul_catalog", d.Name, reachabilityOf(prev.Datacenters[i].Services), reachabilityOf(d.Services))
		c.logChange(ctx, "nomad", d.Name, reachabilityOf(prev.Datacenters[i].Nomad), reachabilityOf(d.Nomad))
	}

	for i, cl := range next.Clusters {
		dcs := strings.Join(cl.Datacenters, ",")
		p := prev.Clusters[i]

		c.logChange(ctx, "consul_scope", dcs, reachabilityOf(p.Scope), reachabilityOf(cl.Scope))
		c.logChange(ctx, "consul_api_query", dcs, reachabilityOf(p.API), reachabilityOf(cl.API))
		c.logChange(ctx, "patroni", dcs, reachabilityOf(p.Patroni), reachabilityOf(cl.Patroni))
	}
}

func (c *Collector) logChange(ctx context.Context, name, dc string, prev, next reachability) {
	fields := []logging.Field{logging.Str("source", name)}
	if dc != "" {
		fields = append(fields, logging.Str("datacenter", dc))
	}

	switch {
	case !next.reachable && (prev.reachable || prev.since.IsZero()):
		c.log.WarnCtx(ctx, "source is unreachable",
			append(fields, logging.Str("reason", string(next.reason)), logging.Err(next.err))...)
	case next.reachable && !prev.reachable && !prev.since.IsZero():
		c.log.InfoCtx(ctx, "source is reachable again",
			append(fields, logging.Dur("unreachable_for", time.Since(prev.since)))...)
	}
}
