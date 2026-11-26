package operation

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/preflight"
)

const (
	// DefaultProgressInterval is how often the progress of a step is written
	// to dcctl/op at most.
	DefaultProgressInterval = 5 * time.Second
	// DefaultJournalProgressInterval is how often the progress of a step is
	// journaled at most.
	DefaultJournalProgressInterval = 30 * time.Second
)

// Preflight runs the checks of an action; [*preflight.Runner] is one.
type Preflight interface {
	Run(ctx context.Context, req preflight.Request) (check.Verdict, error)
}

// Journal journals operations, steps and coordination events; [*journal.Journal]
// is one.
type Journal interface {
	Append(ctx context.Context, e *dcctlv1.JournalEntry) (journal.Appended, error)
}

// Recorder records how long operations and steps took; [*telemetry.OperationRecorder]
// is one.
type Recorder interface {
	RecordOperation(ctx context.Context, op *dcctlv1.Operation) error
	RecordStep(ctx context.Context, opType dcctlv1.Operation_Type, step *dcctlv1.Operation_Step) error
}

// Instance is this dcctl instance as operations name their leader.
type Instance struct {
	// Node is the Consul node name of the instance.
	Node string
	// Datacenter is the name of its datacenter.
	Datacenter string
	// Address is its listen.peer address.
	Address string
}

func (s Instance) proto() *dcctlv1.Instance {
	return &dcctlv1.Instance{Node: s.Node, Datacenter: datacenterName(s.Datacenter), Address: s.Address}
}

// leads reports whether s is the leader of op.
func (s Instance) leads(op *dcctlv1.Operation) bool {
	l := op.GetLeader()

	return l.GetNode() == s.Node && datacenterID(l.GetDatacenter()) == s.Datacenter
}

// Config configures an [Engine].
type Config struct {
	Self        Instance
	Locker      *coord.Locker
	Ops         *coord.OpStore
	Coordinator *coord.Coordinator
	Preflight   Preflight
	Journal     Journal
	Recorder    Recorder
	// Resumers rebuild the steps of an interrupted operation by its type; a
	// type without one cannot be resumed.
	Resumers map[dcctlv1.Operation_Type]Resumer
	// Interval is the pause between the rounds that interrupt the operations
	// no live instance runs and align the copies of dcctl/op and the voters.
	Interval time.Duration
	// ProgressInterval and JournalProgressInterval bound how often the
	// progress of a step is written to dcctl/op and journaled; zero means
	// [DefaultProgressInterval] and [DefaultJournalProgressInterval].
	ProgressInterval        time.Duration
	JournalProgressInterval time.Duration
	Log                     *logging.Logger
}

// Engine runs operations. Create it with [New], then [Engine.Run] it; the
// actions are refused with [ErrNotRunning] outside Run.
type Engine struct {
	cfg Config
	bus *bus

	mu       sync.Mutex
	ctx      context.Context //nolint:containedctx // the operations run in the context of Run
	stopping bool
	// active are the operations this engine runs now, by name.
	active map[string]bool
	wg     sync.WaitGroup
}

// New returns an engine. It does no I/O.
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.Self.Node == "" || cfg.Self.Datacenter == "":
		return nil, errors.New("operation engine needs the node and the datacenter of this instance")
	case cfg.Locker == nil || cfg.Ops == nil || cfg.Coordinator == nil:
		return nil, errors.New("operation engine needs the locker, the operation store and the coordinator")
	case cfg.Preflight == nil || cfg.Journal == nil || cfg.Recorder == nil:
		return nil, errors.New("operation engine needs the preflight, the journal and the recorder")
	case cfg.Interval <= 0:
		return nil, errors.New("operation engine interval must be positive")
	case cfg.ProgressInterval < 0 || cfg.JournalProgressInterval < 0:
		return nil, errors.New("progress intervals cannot be negative")
	case cfg.Log == nil:
		return nil, errors.New("operation engine needs a logger")
	}

	if cfg.ProgressInterval == 0 {
		cfg.ProgressInterval = DefaultProgressInterval
	}

	if cfg.JournalProgressInterval == 0 {
		cfg.JournalProgressInterval = DefaultJournalProgressInterval
	}

	return &Engine{cfg: cfg, bus: newBus(), active: make(map[string]bool)}, nil
}

// Run runs until ctx ends. Right away and then every interval it interrupts
// the RUNNING operations no live instance runs and aligns the copies of
// dcctl/op and the voters. The operations started while it runs, run in its
// context: when ctx ends they stop before their next step and are
// interrupted, and Run returns once they have.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()

	if e.ctx != nil {
		e.mu.Unlock()

		return errors.New("operation engine is already running")
	}

	e.ctx = ctx
	e.mu.Unlock()

	t := time.NewTicker(e.cfg.Interval)

	for {
		e.maintain(ctx)

		select {
		case <-ctx.Done():
		case <-t.C:
			continue
		}

		break
	}

	t.Stop()

	e.mu.Lock()
	e.stopping = true
	e.mu.Unlock()

	e.wg.Wait()

	return nil
}

// Subscribe returns a subscription to every change of the operation name that
// this engine makes, or of every operation for an empty name. Close it when
// done.
func (e *Engine) Subscribe(name string) *Subscription {
	return e.bus.subscribe(name)
}

// enter registers an action, so that Run waits for it, and returns the context
// operations run in. leave must follow unless the action hands its work to a
// goroutine that calls it.
func (e *Engine) enter() (context.Context, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ctx == nil || e.stopping || e.ctx.Err() != nil {
		return nil, ErrNotRunning
	}

	e.wg.Add(1)

	return e.ctx, nil
}

func (e *Engine) leave() { e.wg.Done() }

// claim marks the operation name as run by this engine; false when it already
// is.
func (e *Engine) claim(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.active[name] {
		return false
	}

	e.active[name] = true

	return true
}

func (e *Engine) unclaim(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.active, name)
}

func (e *Engine) runs(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.active[name]
}
