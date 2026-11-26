package cli

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

type fakeCall struct {
	method string
	req    proto.Message
}

// fakeAPI is a dcctl API on a real loopback listener. A handler that is nil
// answers Unimplemented.
type fakeAPI struct {
	dcctlv1.UnimplementedStatusServiceServer
	dcctlv1.UnimplementedDatacenterServiceServer
	dcctlv1.UnimplementedOperationServiceServer
	dcctlv1.UnimplementedJournalServiceServer

	mu    sync.Mutex
	calls []fakeCall
	auth  []string

	status          func(*dcctlv1.GetStatusRequest) (*dcctlv1.Status, error)
	verify          func(*dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error)
	listDatacenters func(*dcctlv1.ListDatacentersRequest) (*dcctlv1.ListDatacentersResponse, error)
	getDatacenter   func(*dcctlv1.GetDatacenterRequest) (*dcctlv1.Datacenter, error)
	listOperations  func(*dcctlv1.ListOperationsRequest) (*dcctlv1.ListOperationsResponse, error)
	getOperation    func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error)
	journal         func(*dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error)
	watch           func(*dcctlv1.WatchOperationRequest, grpc.ServerStreamingServer[dcctlv1.Operation]) error
	// act answers every action, named by its RPC.
	act func(method string, req proto.Message) (*dcctlv1.Operation, error)
}

func (f *fakeAPI) record(ctx context.Context, method string, req proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()

	md, _ := metadata.FromIncomingContext(ctx)
	f.auth = append(f.auth, md.Get("authorization")...)

	if req != nil {
		req = proto.Clone(req)
	}

	f.calls = append(f.calls, fakeCall{method: method, req: req})
}

// requests returns the requests of the RPC in the order they came.
func (f *fakeAPI) requests(method string) []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []proto.Message

	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.req)
		}
	}

	return out
}

func (f *fakeAPI) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.method
	}

	return slices.Clone(out)
}

func (f *fakeAPI) authorizations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.auth)
}

func startFake(t *testing.T) (*fakeAPI, string) {
	t.Helper()

	f := &fakeAPI{}

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			f.record(ctx, info.FullMethod, req.(proto.Message))

			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			f.record(ss.Context(), info.FullMethod, nil)

			return h(srv, ss)
		}),
	)
	dcctlv1.RegisterStatusServiceServer(srv, f)
	dcctlv1.RegisterDatacenterServiceServer(srv, f)
	dcctlv1.RegisterOperationServiceServer(srv, f)
	dcctlv1.RegisterJournalServiceServer(srv, f)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = srv.Serve(ln)
	}()

	t.Cleanup(func() {
		srv.Stop()
		<-done
	})

	return f, ln.Addr().String()
}

func (f *fakeAPI) GetStatus(_ context.Context, r *dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
	if f.status == nil {
		return f.UnimplementedStatusServiceServer.GetStatus(context.Background(), r)
	}

	return f.status(r)
}

func (f *fakeAPI) VerifyStatus(_ context.Context, r *dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error) {
	if f.verify == nil {
		return f.UnimplementedStatusServiceServer.VerifyStatus(context.Background(), r)
	}

	return f.verify(r)
}

func (f *fakeAPI) ListDatacenters(_ context.Context, r *dcctlv1.ListDatacentersRequest) (*dcctlv1.ListDatacentersResponse, error) {
	if f.listDatacenters == nil {
		return f.UnimplementedDatacenterServiceServer.ListDatacenters(context.Background(), r)
	}

	return f.listDatacenters(r)
}

func (f *fakeAPI) GetDatacenter(_ context.Context, r *dcctlv1.GetDatacenterRequest) (*dcctlv1.Datacenter, error) {
	if f.getDatacenter == nil {
		return f.UnimplementedDatacenterServiceServer.GetDatacenter(context.Background(), r)
	}

	return f.getDatacenter(r)
}

func (f *fakeAPI) ListOperations(_ context.Context, r *dcctlv1.ListOperationsRequest) (*dcctlv1.ListOperationsResponse, error) {
	if f.listOperations == nil {
		return f.UnimplementedOperationServiceServer.ListOperations(context.Background(), r)
	}

	return f.listOperations(r)
}

func (f *fakeAPI) GetOperation(_ context.Context, r *dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) {
	if f.getOperation == nil {
		return f.UnimplementedOperationServiceServer.GetOperation(context.Background(), r)
	}

	return f.getOperation(r)
}

func (f *fakeAPI) WatchOperation(r *dcctlv1.WatchOperationRequest, s grpc.ServerStreamingServer[dcctlv1.Operation]) error {
	if f.watch == nil {
		return f.UnimplementedOperationServiceServer.WatchOperation(r, s)
	}

	return f.watch(r, s)
}

func (f *fakeAPI) ListJournalEntries(_ context.Context, r *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error) {
	if f.journal == nil {
		return f.UnimplementedJournalServiceServer.ListJournalEntries(context.Background(), r)
	}

	return f.journal(r)
}

func (f *fakeAPI) action(method string, req proto.Message) (*dcctlv1.Operation, error) {
	if f.act == nil {
		return nil, errUnimplemented(method)
	}

	return f.act(method, req)
}

func (f *fakeAPI) SwitchDatacenter(_ context.Context, r *dcctlv1.SwitchDatacenterRequest) (*dcctlv1.Operation, error) {
	return f.action("SwitchDatacenter", r)
}

func (f *fakeAPI) FailoverDatacenter(_ context.Context, r *dcctlv1.FailoverDatacenterRequest) (*dcctlv1.Operation, error) {
	return f.action("FailoverDatacenter", r)
}

func (f *fakeAPI) DemoteDatacenter(_ context.Context, r *dcctlv1.DemoteDatacenterRequest) (*dcctlv1.Operation, error) {
	return f.action("DemoteDatacenter", r)
}

func (f *fakeAPI) RepointDatacenter(_ context.Context, r *dcctlv1.RepointDatacenterRequest) (*dcctlv1.Operation, error) {
	return f.action("RepointDatacenter", r)
}

func (f *fakeAPI) MarkDatacenterRecovered(_ context.Context, r *dcctlv1.MarkDatacenterRecoveredRequest) (*dcctlv1.Operation, error) {
	return f.action("MarkDatacenterRecovered", r)
}

func (f *fakeAPI) ResolveConflict(_ context.Context, r *dcctlv1.ResolveConflictRequest) (*dcctlv1.Operation, error) {
	return f.action("ResolveConflict", r)
}

func (f *fakeAPI) InitializeDatacenters(_ context.Context, r *dcctlv1.InitializeDatacentersRequest) (*dcctlv1.Operation, error) {
	return f.action("InitializeDatacenters", r)
}

func (f *fakeAPI) InitializeArbiter(_ context.Context, r *dcctlv1.InitializeArbiterRequest) (*dcctlv1.Operation, error) {
	return f.action("InitializeArbiter", r)
}

func (f *fakeAPI) SyncApps(_ context.Context, r *dcctlv1.SyncAppsRequest) (*dcctlv1.Operation, error) {
	return f.action("SyncApps", r)
}

func (f *fakeAPI) ResumeOperation(_ context.Context, r *dcctlv1.ResumeOperationRequest) (*dcctlv1.Operation, error) {
	return f.action("ResumeOperation", r)
}

func (f *fakeAPI) AbandonOperation(_ context.Context, r *dcctlv1.AbandonOperationRequest) (*dcctlv1.Operation, error) {
	return f.action("AbandonOperation", r)
}
