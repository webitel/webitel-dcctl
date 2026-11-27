package scenario_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/scenario"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const (
	dcA = "dc_a"
	dcB = "dc_b"
	dcC = "dc_c"
)

var (
	self      = operation.Instance{Node: "node-b", Datacenter: dcB, Address: "10.0.0.2:10023"}
	initiator = &dcctlv1.Initiator{User: "admin"}
)

// world is three datacenters, dc_a active at generation 5, with three stages of apps
// each; the KVs of dc_a and dc_b take dcctl/lock and dcctl/op, and the voters are those
// two KVs and the arbiter.
type world struct {
	t        *testing.T
	infra    *infra
	consuls  map[string]*fakeConsul
	voters   []*fakeVoter
	journal  *fakeJournal
	verifier *fakeVerifier
	coord    *coord.Coordinator
	sw       *scenario.Switch
	fo       *scenario.Failover
	ssw      *scenario.StretchSwitch
	sfo      *scenario.StretchFailover
	demote   *scenario.Demote
	repoint  *scenario.Repoint
	mark     *scenario.MarkRecovered
	// kvs are the Consul KVs that take dcctl/lock and dcctl/op; opKV is the one op reads.
	kvs  []string
	opKV string
	// stretch makes the world one of stretch: its scenarios, status and preflight.
	stretch bool
}

func newWorld(t *testing.T) *world {
	t.Helper()

	base := kv.State{Generation: 5, ActiveDatacenter: dcA, OpID: "old", PatroniTimeline: 5}

	w := &world{
		t: t,
		infra: &infra{
			dcs: map[string]*dcState{
				dcA: {running: map[int]bool{1: true, 2: true, 3: true}, role: patroni.RolePrimary, timeline: 5, history: ancestry(5)},
				dcB: {running: map[int]bool{1: false, 2: false, 3: false}, role: patroni.RoleStandbyLeader, timeline: 5, upstream: dcA},
				dcC: {running: map[int]bool{1: false, 2: false, 3: false}, role: patroni.RoleStandbyLeader, timeline: 5, upstream: dcA},
			},
			hooks: map[string]func(context.Context) error{},
		},
		consuls: map[string]*fakeConsul{dcA: newFakeConsul(), dcB: newFakeConsul()},
		voters: []*fakeVoter{
			newVoter(t, dcA, coord.KindKV, base),
			newVoter(t, dcB, coord.KindKV, base),
			newVoter(t, coord.ArbiterName, coord.KindArbiter, base),
		},
		journal:  &fakeJournal{},
		verifier: &fakeVerifier{},
		kvs:      []string{dcA, dcB},
		opKV:     dcB,
	}

	voters := make([]coord.Voter, 0, len(w.voters))
	for _, v := range w.voters {
		voters = append(voters, v)
	}

	w.useVoters(voters...)

	return w
}

// useVoters makes voters the voters of w and builds the scenarios on them.
func (w *world) useVoters(voters ...coord.Voter) {
	w.t.Helper()

	c, err := coord.New(voters, 3*time.Second, quietLog())
	require.NoError(w.t, err)

	w.coord = c

	dcs := map[string]scenario.Datacenter{}
	for _, dc := range []string{dcA, dcB, dcC} {
		dcs[dc] = scenario.Datacenter{Nomad: nomadStub{}, Patroni: patroniStub{}}
	}

	d := scenario.Deps{
		Apps:        fakeApps{w.infra},
		Patroni:     fakePatroni{w.infra},
		Coordinator: c,
		Verifier:    w.verifier,
		Datacenters: dcs,
	}

	w.sw, err = scenario.NewSwitch(d)
	require.NoError(w.t, err)

	w.fo, err = scenario.NewFailover(d)
	require.NoError(w.t, err)

	w.ssw, err = scenario.NewStretchSwitch(d)
	require.NoError(w.t, err)

	w.sfo, err = scenario.NewStretchFailover(d)
	require.NoError(w.t, err)

	w.demote, err = scenario.NewDemote(d)
	require.NoError(w.t, err)

	w.repoint, err = scenario.NewRepoint(d)
	require.NoError(w.t, err)

	w.mark, err = scenario.NewMarkRecovered(d)
	require.NoError(w.t, err)
}

// engine returns an engine of this instance on w that resumes switches and failovers.
func (w *world) engine() *operation.Engine {
	w.t.Helper()

	kvs := make([]coord.KV, 0, len(w.kvs))
	for _, name := range w.kvs {
		kvs = append(kvs, coord.KV{Name: name, Client: w.consuls[name]})
	}

	resumers := map[dcctlv1.Operation_Type]operation.Resumer{
		dcctlv1.Operation_SWITCH: w.sw, dcctlv1.Operation_FAILOVER: w.fo,
		dcctlv1.Operation_DEMOTE: w.demote, dcctlv1.Operation_REPOINT: w.repoint, dcctlv1.Operation_MARK_RECOVERED: w.mark,
	}
	if w.stretch {
		resumers = map[dcctlv1.Operation_Type]operation.Resumer{
			dcctlv1.Operation_SWITCH: w.ssw, dcctlv1.Operation_FAILOVER: w.sfo,
		}
	}

	locker, err := coord.NewLocker(kvs, coord.LockOptions{TTL: 15 * time.Second, LockDelay: 15 * time.Second, Timeout: 3 * time.Second}, quietLog())
	require.NoError(w.t, err)

	ops, err := coord.NewOpStore(kvs, 3*time.Second, quietLog())
	require.NoError(w.t, err)

	e, err := operation.New(operation.Config{
		Self:        self,
		Locker:      locker,
		Ops:         ops,
		Coordinator: w.coord,
		Preflight:   w.preflight(),
		Journal:     w.journal,
		Recorder:    fakeRecorder{},
		Resumers:    resumers,
		Interval:    15 * time.Second,
		Log:         quietLog(),
	})
	require.NoError(w.t, err)

	return e
}

// preflight decides the confirmations on w, see fakePreflight.
func (w *world) preflight() fakePreflight {
	return fakePreflight{
		w: w.infra, stretch: w.stretch, op: func() *dcctlv1.Operation { return storedOp(w.consuls[w.opKV]) },
		view: func(ctx context.Context) (coord.View, error) { return w.coord.Read(ctx) },
	}
}

// start runs e until the returned stop, which waits for Run to return.
func start(t *testing.T, e *operation.Engine) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- e.Run(ctx) }()

	return func() {
		cancel()
		require.NoError(t, <-done)
	}
}

func threeStages() status.Nomad {
	return status.Nomad{Stages: []status.Stage{{Stage: 1}, {Stage: 2}, {Stage: 3}}}
}

// status is the state a preflight of a switch would decide on now.
func (w *world) status() status.Status {
	w.t.Helper()

	view, err := w.coord.Read(w.t.Context())
	require.NoError(w.t, err)

	profile := config.ProfileWarmStandby
	if w.stretch {
		profile = config.ProfileStretch
	}

	return status.Status{
		Profile: profile,
		View:    view,
		Datacenters: []status.Datacenter{
			{Name: dcA, Nomad: threeStages()},
			{Name: dcB, Nomad: threeStages()},
			{Name: dcC, Nomad: threeStages()},
		},
	}
}

func switchTo(dc string) *dcctlv1.SwitchDatacenterRequest {
	return &dcctlv1.SwitchDatacenterRequest{Name: "datacenters/" + dc, RequestId: "6f1c3a52-7d1e-4b8a-9a43-0d3f5b2c1e77"}
}

func (w *world) plan() operation.Plan {
	w.t.Helper()

	plan := w.sw.Plan
	if w.stretch {
		plan = w.ssw.Plan
	}

	p, err := plan(switchTo(dcB), initiator, preflight.Result{Status: w.status()})
	require.NoError(w.t, err)

	return p
}

// op returns the operation in the KV of dc_b, or the global one of stretch.
func (w *world) op() *dcctlv1.Operation {
	w.t.Helper()

	return w.consuls[w.opKV].op(w.t)
}

func (w *world) lockFree() bool {
	for _, c := range w.consuls {
		if c.lockHolder() != "" {
			return false
		}
	}

	return true
}

// rawOps returns dcctl/op of every KV as stored.
func (w *world) rawOps() map[string][]byte {
	out := map[string][]byte{}
	for dc, c := range w.consuls {
		out[dc] = c.rawOp()
	}

	return out
}

func (w *world) setRawOps(ops map[string][]byte) {
	for dc, b := range ops {
		w.consuls[dc].setRawOp(b)
	}
}

// crashAt makes the primitive of key wait until the operation stops, and keeps dcctl/op
// as it was stored while the primitive ran: the record a crash would leave.
func (w *world) crashAt(key string) (crashed func() map[string][]byte) {
	var saved map[string][]byte

	w.infra.hook(key, func(ctx context.Context) error {
		saved = w.rawOps()

		<-ctx.Done()

		return context.Cause(ctx)
	})

	return func() map[string][]byte { return saved }
}

// stepOf returns the step of op of type typ and stage.
func stepOf(t *testing.T, op *dcctlv1.Operation, typ dcctlv1.Operation_Step_Type, stage int32) *dcctlv1.Operation_Step {
	t.Helper()

	for _, st := range op.GetSteps() {
		if st.GetType() == typ && st.GetStage() == stage {
			return st
		}
	}

	require.Failf(t, "no step", "operation has no step %s", typ)

	return nil
}

func allRunning(d dcState) bool {
	for _, r := range d.running {
		if !r {
			return false
		}
	}

	return true
}

func noneRunning(d dcState) bool {
	for _, r := range d.running {
		if r {
			return false
		}
	}

	return true
}
