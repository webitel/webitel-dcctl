package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/auth"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/operation"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	dcstatus "github.com/webitel/webitel-dcctl/internal/status"
)

const (
	aliceUser     = "alice"
	alicePassword = "s3cret-pa55"
	requestA      = "0b6e4f0e-5d3c-4c6a-9a43-2f6c1f3b8a01"
	// clientIP is the address of a direct gRPC client of the tests.
	clientIP = "192.0.2.10"
)

var selfA = operation.Instance{Node: "node-a", Datacenter: "dc_a", Address: "10.0.0.1:10023"}

// memOperations is dcctl/op of one KV in memory.
type memOperations struct {
	mu  sync.Mutex
	ops []*dcctlv1.Operation
	err error
}

func (m *memOperations) Read(context.Context) (coord.OpView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return coord.OpView{}, m.err
	}

	view := coord.OpView{}
	for i, op := range m.ops {
		view.Copies = append(view.Copies, coord.OpCopy{
			Name: "kv" + string(rune('0'+i)), Status: coord.StatusPresent, Operation: proto.CloneOf(op),
		})
	}

	return view, nil
}

func (m *memOperations) put(op *dcctlv1.Operation) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ops = append(m.ops, proto.CloneOf(op))
}

// fakePreflight decides every action with the checks of its action.
type fakePreflight struct {
	mu     sync.Mutex
	checks map[preflight.Action][]check.Check
	// status is the state the checks are decided on.
	status dcstatus.Status
	err    error
	runs   []preflight.Request
}

func (f *fakePreflight) Evaluate(_ context.Context, req preflight.Request) (preflight.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs = append(f.runs, req)
	if f.err != nil {
		return preflight.Result{}, f.err
	}

	return preflight.Result{Verdict: check.Decide(f.checks[req.Action], req.Confirmations), Status: f.status}, nil
}

func (f *fakePreflight) refuse(a preflight.Action, cs ...check.Check) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.checks == nil {
		f.checks = make(map[preflight.Action][]check.Check)
	}

	f.checks[a] = cs
}

func (f *fakePreflight) requests() []preflight.Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]preflight.Request(nil), f.runs...)
}

// blockedCheck fails with a block effect.
func blockedCheck() check.Check {
	return check.Check{ID: check.CoordQuorum, Result: check.Fail, Effect: check.Block, Detail: "no majority of voters"}
}

// fakeEngine starts operations in ops.
type fakeEngine struct {
	ops *memOperations

	mu       sync.Mutex
	plans    []operation.Plan
	resumes  []operation.ResumeRequest
	abandons []operation.AbandonRequest
	startErr error
	err      error
	// onStart runs on every Start, before it answers.
	onStart func()
}

func (e *fakeEngine) Start(_ context.Context, p operation.Plan) (*dcctlv1.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.plans = append(e.plans, p)
	if e.onStart != nil {
		e.onStart()
	}

	if e.startErr != nil {
		return nil, e.startErr
	}

	op := &dcctlv1.Operation{
		Name: "operations/op" + string(rune('0'+len(e.plans))), Type: p.Type, State: dcctlv1.Operation_RUNNING,
		Target: datacenterName(p.Target), Initiator: p.Initiator, Mode: p.Mode, Steps: p.Pending(),
		RequestId: p.RequestID, CreateTime: timestamppb.Now(), Revision: 1,
	}
	setRequest(op, p.Request)

	if e.ops != nil {
		e.ops.put(op)
	}

	return op, nil
}

func (e *fakeEngine) Resume(_ context.Context, req operation.ResumeRequest) (*dcctlv1.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.resumes = append(e.resumes, req)
	if e.err != nil {
		return nil, e.err
	}

	return &dcctlv1.Operation{Name: "operations/" + req.Operation, State: dcctlv1.Operation_RUNNING}, nil
}

func (e *fakeEngine) Abandon(_ context.Context, req operation.AbandonRequest) (*dcctlv1.Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.abandons = append(e.abandons, req)
	if e.err != nil {
		return nil, e.err
	}

	return &dcctlv1.Operation{Name: "operations/" + req.Operation, State: dcctlv1.Operation_ABANDONED}, nil
}

func (e *fakeEngine) started() []operation.Plan {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]operation.Plan(nil), e.plans...)
}

// fakePlanner plans one GENERATION step on the datacenter of the request.
type fakePlanner[R actionRequest] struct {
	target func(R) string
	err    error
}

func (p fakePlanner[R]) Plan(req R, ini *dcctlv1.Initiator, _ preflight.Result) (operation.Plan, error) {
	if p.err != nil {
		return operation.Plan{}, p.err
	}

	target := ""
	if p.target != nil {
		target = p.target(req)
	}

	return operation.Plan{
		Target:             target,
		Initiator:          ini,
		Mode:               dcctlv1.Operation_QUORUM,
		PreviousGeneration: 4,
		NewGeneration:      5,
		Steps: []operation.Step{{
			Type: dcctlv1.Operation_Step_GENERATION, Datacenter: target,
			Run: func(context.Context, *operation.Exec) error { return nil },
		}},
	}, nil
}

func byName[R interface{ GetName() string }](r R) string { return datacenterID(r.GetName()) }

func testPlanners() Planners {
	return Planners{
		Switch:          fakePlanner[*dcctlv1.SwitchDatacenterRequest]{target: byName[*dcctlv1.SwitchDatacenterRequest]},
		Failover:        fakePlanner[*dcctlv1.FailoverDatacenterRequest]{target: byName[*dcctlv1.FailoverDatacenterRequest]},
		Demote:          fakePlanner[*dcctlv1.DemoteDatacenterRequest]{target: byName[*dcctlv1.DemoteDatacenterRequest]},
		Repoint:         fakePlanner[*dcctlv1.RepointDatacenterRequest]{target: byName[*dcctlv1.RepointDatacenterRequest]},
		MarkRecovered:   fakePlanner[*dcctlv1.MarkDatacenterRecoveredRequest]{target: byName[*dcctlv1.MarkDatacenterRecoveredRequest]},
		ResolveConflict: fakePlanner[*dcctlv1.ResolveConflictRequest]{target: func(r *dcctlv1.ResolveConflictRequest) string { return datacenterID(r.GetWinner()) }},
		Initialize: fakePlanner[*dcctlv1.InitializeDatacentersRequest]{target: func(r *dcctlv1.InitializeDatacentersRequest) string {
			return datacenterID(r.GetActiveDatacenter())
		}},
		InitializeArbiter: fakePlanner[*dcctlv1.InitializeArbiterRequest]{},
		SyncApps:          fakePlanner[*dcctlv1.SyncAppsRequest]{target: func(*dcctlv1.SyncAppsRequest) string { return "dc_a" }},
	}
}

// fakeCatalog lists the passing instances of each datacenter.
type fakeCatalog struct {
	mu    sync.Mutex
	nodes map[string][]consul.ServiceNode
}

func (c *fakeCatalog) Instances(_ context.Context, dc string) ([]consul.ServiceNode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.nodes[dc], nil
}

func (c *fakeCatalog) set(dc, node, hostPort string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.nodes == nil {
		c.nodes = make(map[string][]consul.ServiceNode)
	}

	host, port, _ := net.SplitHostPort(hostPort)
	p, _ := strconv.Atoi(port)

	c.nodes[dc] = append(c.nodes[dc], consul.ServiceNode{Node: node, Address: host, Port: p, Status: "passing"})
}

// fakeForwarder answers every forwarded call with resp or err and records
// them.
type fakeForwarder struct {
	mu    sync.Mutex
	calls []forwardedCall
	resp  *dcctlv1.Operation
	err   error
}

type forwardedCall struct {
	address, method string
	req             proto.Message
	md              metadata.MD
}

func (f *fakeForwarder) Forward(ctx context.Context, address, method string, req, resp proto.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	md, _ := metadata.FromOutgoingContext(ctx)
	f.calls = append(f.calls, forwardedCall{address: address, method: method, req: proto.Clone(req), md: md})

	if f.err != nil {
		return f.err
	}

	proto.Merge(resp, f.resp)

	return nil
}

func (f *fakeForwarder) forwarded() []forwardedCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]forwardedCall(nil), f.calls...)
}

// actionEnv is the action servers of instance node-a of dc_a over fakes.
type actionEnv struct {
	ops       *memOperations
	preflight *fakePreflight
	engine    *fakeEngine
	catalog   *fakeCatalog
	forwarder *fakeForwarder
	status    fakeStatusSource
	planners  Planners
}

func newActionEnv() *actionEnv {
	ops := &memOperations{}
	st := exampleStatus()
	st.Effective.ActiveDatacenter = "dc_a"

	return &actionEnv{
		ops: ops, preflight: &fakePreflight{}, engine: &fakeEngine{ops: ops},
		catalog: &fakeCatalog{}, forwarder: &fakeForwarder{resp: &dcctlv1.Operation{Name: "operations/remote"}},
		status: fakeStatusSource{st: st, ok: true}, planners: testPlanners(),
	}
}

func (e *actionEnv) actions() *Actions {
	return &Actions{
		Self: selfA, Datacenters: []string{"dc_a", "dc_b"},
		Engine: e.engine, Preflight: e.preflight, Planners: e.planners,
		Catalog: e.catalog, Forwarder: e.forwarder,
	}
}

func (e *actionEnv) servers(t *testing.T) (*DatacenterServer, *OperationServer) {
	t.Helper()

	a, err := newActionServer(e.actions(), e.status, e.ops, testLogger())
	require.NoError(t, err)

	dcs := NewDatacenterServer(e.status, observerAddress, testLogger())
	dcs.actions = a

	ops := NewOperationServer(e.ops, nil, nil, testLogger())
	ops.actions = a

	return dcs, ops
}

// testBasic is basic auth with the user alice.
func testBasic(t *testing.T) *auth.Basic {
	t.Helper()

	h, err := bcrypt.GenerateFromPassword([]byte(alicePassword), bcrypt.MinCost)
	require.NoError(t, err)

	b, err := auth.NewBasic(t.Context(), []config.User{
		{Name: aliceUser, PasswordHash: string(h), Role: config.RoleOperator},
	}, testLogger())
	require.NoError(t, err)

	return b
}

func basicHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(aliceUser+":"+alicePassword))
}

var unaryInfo = &grpc.UnaryServerInfo{FullMethod: dcctlv1.DatacenterService_SwitchDatacenter_FullMethodName}

// aliceCtx returns the context of a call of alice on listen.http, as basic
// auth leaves it.
func aliceCtx(t *testing.T) context.Context {
	t.Helper()

	ctx := peer.NewContext(t.Context(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(clientIP), Port: 50123}})
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", basicHeader()))

	return admitted(t, testBasic(t).UnaryServerInterceptor(), ctx)
}

// forwardedCtx returns the context of a call that another instance forwarded
// with ini, as the peer listener admits it.
func forwardedCtx(t *testing.T, ini *dcctlv1.Initiator) context.Context {
	t.Helper()

	out, err := auth.Forwarding(t.Context(), ini)
	require.NoError(t, err)

	md, _ := metadata.FromOutgoingContext(out)

	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "node"}, IPAddresses: []net.IP{net.ParseIP("10.1.0.1")}}
	ctx := peer.NewContext(t.Context(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.ParseIP("10.1.0.1"), Port: 40000},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}},
	})
	ctx = metadata.NewIncomingContext(ctx, md)

	peers := auth.NewPeers(staticPeers{"10.1.0.1:10023"}, testLogger())

	return admitted(t, peers.UnaryServerInterceptor(), ctx)
}

// admitted returns the context the handler gets after interceptor admits a
// call in ctx.
func admitted(t *testing.T, interceptor grpc.UnaryServerInterceptor, ctx context.Context) context.Context {
	t.Helper()

	got := make(chan context.Context, 1)

	_, err := interceptor(ctx, nil, unaryInfo, func(ctx context.Context, _ any) (any, error) {
		got <- ctx

		return nil, nil //nolint:nilnil // the handler only keeps the context
	})
	require.NoError(t, err)

	return <-got
}

// staticPeers lists the same dcctl addresses in dc_a and dc_b.
type staticPeers []string

func (staticPeers) Datacenters() []string { return []string{"dc_a", "dc_b"} }

func (p staticPeers) RegisteredAddrs(context.Context, string, string) ([]string, error) {
	return p, nil
}

func aliceFrom(node, dc string) *dcctlv1.Initiator {
	return &dcctlv1.Initiator{
		User: aliceUser, Address: "203.0.113.7",
		Instance: &dcctlv1.Instance{Node: node, Datacenter: datacenterName(dc), Address: "10.1.0.1:10023"},
	}
}

func assertProto(t *testing.T, want, got proto.Message) {
	t.Helper()

	assert.True(t, proto.Equal(want, got), "got %v; want %v", got, want)
}
