package api

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/source"
)

const watchedName = "operations/0b9f3c2e-5d1a-4c55-9a7e-2f4d8c1b6a10"

func watched(revision int64, state dcctlv1.Operation_State) *dcctlv1.Operation {
	return &dcctlv1.Operation{Name: watchedName, Revision: revision, State: state}
}

// ledBy returns op led from the datacenter dc, whose copy answers for it.
func ledBy(dc string, op *dcctlv1.Operation) *dcctlv1.Operation {
	op.Leader = &dcctlv1.Instance{Node: "dcctl-" + dc, Datacenter: "datacenters/" + dc}

	return op
}

// liveOperations are the copies of dcctl/op that a test changes while a
// stream reads them.
type liveOperations struct {
	mu    sync.Mutex
	view  coord.OpView
	err   error
	reads int
}

func (l *liveOperations) Read(context.Context) (coord.OpView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.reads++

	return l.view, l.err
}

func (l *liveOperations) readCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.reads
}

func (l *liveOperations) set(copies ...coord.OpCopy) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.view = coord.OpView{Copies: copies}
}

func (l *liveOperations) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.err = err
}

func liveWith(op *dcctlv1.Operation) *liveOperations {
	l := &liveOperations{}
	l.set(present("dc_a", op))

	return l
}

// fakeEvents is an engine that runs the operations in runs.
type fakeEvents struct {
	mu   sync.Mutex
	runs bool
	subs []*fakeSub
}

func (f *fakeEvents) Runs(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.runs
}

func (f *fakeEvents) setRuns(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs = v
}

func (f *fakeEvents) Subscribe(string) OperationSubscription {
	f.mu.Lock()
	defer f.mu.Unlock()

	s := &fakeSub{c: make(chan *dcctlv1.Operation, 64)}
	f.subs = append(f.subs, s)

	return s
}

// sub returns the i-th subscription made.
func (f *fakeEvents) sub(i int) *fakeSub {
	f.mu.Lock()
	defer f.mu.Unlock()

	if i >= len(f.subs) {
		return nil
	}

	return f.subs[i]
}

func (f *fakeEvents) subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.subs)
}

type fakeSub struct {
	mu     sync.Mutex
	c      chan *dcctlv1.Operation
	closed bool
}

func (s *fakeSub) C() <-chan *dcctlv1.Operation { return s.c }

// Close closes the channel once, like the engine does.
func (s *fakeSub) Close() { s.drop() }

// drop closes the channel the way the engine drops a slow subscriber.
func (s *fakeSub) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed {
		s.closed = true
		close(s.c)
	}
}

func (s *fakeSub) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

func (s *fakeSub) push(op *dcctlv1.Operation) { s.c <- op }

// fakeStream collects what WatchOperation sends.
type fakeStream struct {
	grpc.ServerStream

	ctx context.Context //nolint:containedctx // grpc.ServerStream returns its context from a method

	mu   sync.Mutex
	sent []*dcctlv1.Operation
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(op *dcctlv1.Operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sent = append(f.sent, op)

	return nil
}

func (f *fakeStream) revisions() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]int64, 0, len(f.sent))
	for _, op := range f.sent {
		out = append(out, op.GetRevision())
	}

	return out
}

// watching is a stream of WatchOperation running in the bubble of a synctest.
type watching struct {
	stream *fakeStream
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan error
}

func watch(t *testing.T, ops Operations, events OperationEvents) *watching {
	t.Helper()

	stop := make(chan struct{})

	return watchOn(t, NewOperationServer(ops, events, stop, testLogger()), stop)
}

// watchOn watches the operation on s, which stops when stop is closed.
func watchOn(t *testing.T, s *OperationServer, stop chan struct{}) *watching {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	w := &watching{stream: &fakeStream{ctx: ctx}, cancel: cancel, stop: stop, done: make(chan error, 1)}

	go func() {
		w.done <- s.WatchOperation(&dcctlv1.WatchOperationRequest{Name: watchedName}, w.stream)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-w.done:
		case <-time.After(time.Second):
		}
	})

	return w
}

// result returns how the stream ended, and false while it is open.
func (w *watching) result() (error, bool) {
	synctest.Wait()

	select {
	case err := <-w.done:
		return err, true
	default:
		return nil, false
	}
}

func (w *watching) requireOpen(t *testing.T) {
	t.Helper()

	_, ended := w.result()
	require.False(t, ended, "the stream ended")
}

func (w *watching) requireEnded(t *testing.T) error {
	t.Helper()

	err, ended := w.result()
	require.True(t, ended, "the stream is open")

	return err
}

func TestWatchLeaderStreamsEngineChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		events := &fakeEvents{runs: true}
		w := watch(t, ops, events)

		synctest.Wait()
		require.Equal(t, 1, events.subscriptions())
		assert.Equal(t, []int64{1}, w.stream.revisions(), "the first message is the current state")

		events.sub(0).push(watched(2, dcctlv1.Operation_RUNNING))
		w.requireOpen(t)
		events.sub(0).push(watched(3, dcctlv1.Operation_SUCCEEDED))

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []int64{1, 2, 3}, w.stream.revisions())
		assert.True(t, events.sub(0).isClosed())
		assert.Equal(t, 1, ops.readCount(), "the leader does not poll the copies")
	})
}

func TestWatchLeaderDroppedSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		events := &fakeEvents{runs: true}
		w := watch(t, ops, events)

		synctest.Wait()
		events.sub(0).push(watched(2, dcctlv1.Operation_RUNNING))
		synctest.Wait()

		// The engine dropped the subscriber at revision 2; the copies are
		// at 5 by the time it reads them again.
		ops.set(present("dc_a", watched(5, dcctlv1.Operation_RUNNING)))
		events.sub(0).drop()
		synctest.Wait()

		require.Equal(t, 2, events.subscriptions(), "subscribed anew")
		// A late copy of an older change and a repeat must not go out.
		events.sub(1).push(watched(4, dcctlv1.Operation_RUNNING))
		events.sub(1).push(watched(5, dcctlv1.Operation_RUNNING))
		events.sub(1).push(watched(6, dcctlv1.Operation_FAILED))

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []int64{1, 2, 5, 6}, w.stream.revisions())
	})
}

func TestWatchFollowerPollsTheCopies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		events := &fakeEvents{}
		w := watch(t, ops, events)

		synctest.Wait()
		assert.Equal(t, []int64{1}, w.stream.revisions())

		time.Sleep(watchPollInterval)
		synctest.Wait()
		assert.Equal(t, []int64{1}, w.stream.revisions(), "an unchanged revision is not sent again")

		ops.set(present("dc_a", watched(2, dcctlv1.Operation_RUNNING)))
		time.Sleep(watchPollInterval)
		synctest.Wait()
		assert.Equal(t, []int64{1, 2}, w.stream.revisions())

		// A copy that is behind answers with an older revision.
		ops.set(present("dc_a", watched(1, dcctlv1.Operation_RUNNING)))
		time.Sleep(watchPollInterval)
		synctest.Wait()
		assert.Equal(t, []int64{1, 2}, w.stream.revisions())

		ops.set(present("dc_a", watched(4, dcctlv1.Operation_ABANDONED)))
		time.Sleep(watchPollInterval)

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []int64{1, 2, 4}, w.stream.revisions())
		assert.Zero(t, events.subscriptions())
	})
}

func TestWatchLeaderChangesMidStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		events := &fakeEvents{}
		w := watch(t, ops, events)

		synctest.Wait()
		require.Zero(t, events.subscriptions())

		// This instance takes the operation over.
		ops.set(present("dc_a", watched(2, dcctlv1.Operation_INTERRUPTED)))
		events.setRuns(true)
		time.Sleep(watchPollInterval)
		synctest.Wait()

		require.Equal(t, 1, events.subscriptions())
		assert.Equal(t, []int64{1, 2}, w.stream.revisions())

		events.sub(0).push(watched(3, dcctlv1.Operation_RUNNING))
		synctest.Wait()
		assert.Equal(t, []int64{1, 2, 3}, w.stream.revisions())

		// Another instance continues it.
		events.setRuns(false)
		ops.set(present("dc_a", ledBy("dc_b", watched(3, dcctlv1.Operation_RUNNING))),
			present("dc_b", ledBy("dc_b", watched(4, dcctlv1.Operation_RUNNING))))
		time.Sleep(watchPollInterval)
		synctest.Wait()

		assert.True(t, events.sub(0).isClosed(), "the engine subscription is closed")
		assert.Equal(t, []int64{1, 2, 3, 4}, w.stream.revisions())

		ops.set(present("dc_b", ledBy("dc_b", watched(5, dcctlv1.Operation_SUCCEEDED))))
		time.Sleep(watchPollInterval)

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []int64{1, 2, 3, 4, 5}, w.stream.revisions())
	})
}

func TestWatchKeepalive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := watch(t, liveWith(watched(7, dcctlv1.Operation_RUNNING)), &fakeEvents{})

		synctest.Wait()
		time.Sleep(watchKeepalive - time.Second)
		synctest.Wait()
		assert.Equal(t, []int64{7}, w.stream.revisions(), "nothing before the keepalive")

		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, []int64{7, 7}, w.stream.revisions(), "the state is repeated with its revision")

		time.Sleep(watchKeepalive)
		synctest.Wait()
		assert.Equal(t, []int64{7, 7, 7}, w.stream.revisions())
		w.requireOpen(t)
	})
}

func TestWatchChangeDefersKeepalive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := &fakeEvents{runs: true}
		w := watch(t, liveWith(watched(1, dcctlv1.Operation_RUNNING)), events)

		synctest.Wait()
		time.Sleep(watchKeepalive - time.Second)
		events.sub(0).push(watched(2, dcctlv1.Operation_RUNNING))
		time.Sleep(watchKeepalive - time.Second)
		synctest.Wait()

		assert.Equal(t, []int64{1, 2}, w.stream.revisions())
	})
}

func TestWatchInterruptedStaysOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		w := watch(t, ops, &fakeEvents{})

		synctest.Wait()

		ops.set(present("dc_a", watched(2, dcctlv1.Operation_INTERRUPTED)))
		time.Sleep(watchPollInterval)
		w.requireOpen(t)

		ops.set(present("dc_a", watched(3, dcctlv1.Operation_RUNNING)))
		time.Sleep(watchPollInterval)
		w.requireOpen(t)

		ops.set(present("dc_a", watched(4, dcctlv1.Operation_SUCCEEDED)))
		time.Sleep(watchPollInterval)

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []int64{1, 2, 3, 4}, w.stream.revisions())
	})
}

func TestWatchFinalStates(t *testing.T) {
	for _, state := range []dcctlv1.Operation_State{
		dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_FAILED, dcctlv1.Operation_ABANDONED,
	} {
		t.Run(state.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := watch(t, liveWith(watched(9, state)), nil)

				require.NoError(t, w.requireEnded(t), "the current state is sent, then the stream ends")
				assert.Equal(t, []int64{9}, w.stream.revisions())
			})
		})
	}
}

func TestWatchNotFoundOrUnavailable(t *testing.T) {
	other := present("dc_a", pagedOperation(1))

	tests := []struct {
		name   string
		copies []coord.OpCopy
		code   codes.Code
		reason apierr.Reason
	}{
		{
			"every_copy_answered",
			[]coord.OpCopy{other, {Name: "dc_b", Status: coord.StatusAbsent}},
			codes.NotFound, apierr.ReasonOperationNotFound,
		},
		{
			"copy_unreachable",
			[]coord.OpCopy{other, {Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout}},
			codes.Unavailable, apierr.ReasonKVUnavailable,
		},
		{
			"copy_invalid",
			[]coord.OpCopy{other, {Name: "dc_b", Status: coord.StatusInvalid, Reason: source.ReasonInvalid}},
			codes.Unavailable, apierr.ReasonKVUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ops := &liveOperations{}
				ops.set(tt.copies...)
				w := watch(t, ops, &fakeEvents{runs: true})

				st := assertCode(t, tt.code, w.requireEnded(t))
				assert.Equal(t, tt.reason, reasonOf(st))
				assert.Empty(t, w.stream.revisions())
			})
		})
	}
}

func TestWatchReadFailsBeforeTheFirstMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := &liveOperations{}
		ops.fail(errors.New("consul at 10.0.0.1 refused"))
		w := watch(t, ops, nil)

		st := assertCode(t, codes.Internal, w.requireEnded(t))
		assert.NotContains(t, st.Message(), "10.0.0.1")
	})
}

func TestWatchUnreadableCopyMidStreamIsWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		w := watch(t, ops, nil)

		synctest.Wait()
		ops.set(coord.OpCopy{Name: "dc_a", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout})
		time.Sleep(watchPollInterval)
		w.requireOpen(t)

		ops.set(present("dc_a", watched(2, dcctlv1.Operation_SUCCEEDED)))
		time.Sleep(watchPollInterval)

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []int64{1, 2}, w.stream.revisions())
	})
}

func TestWatchOperationGoneMidStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
		w := watch(t, ops, nil)

		synctest.Wait()
		ops.set(present("dc_a", pagedOperation(1)))
		time.Sleep(watchPollInterval)

		st := assertCode(t, codes.NotFound, w.requireEnded(t))
		assert.Equal(t, apierr.ReasonOperationNotFound, reasonOf(st))
		assert.Equal(t, []int64{1}, w.stream.revisions())
	})
}

func TestWatchClientCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := &fakeEvents{runs: true}
		w := watch(t, liveWith(watched(1, dcctlv1.Operation_RUNNING)), events)

		synctest.Wait()
		w.cancel()

		assertCode(t, codes.Canceled, w.requireEnded(t))
		assert.True(t, events.sub(0).isClosed(), "the subscription is closed")
	})
}

func TestWatchServiceStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := &fakeEvents{runs: true}
		w := watch(t, liveWith(watched(1, dcctlv1.Operation_RUNNING)), events)

		synctest.Wait()
		close(w.stop)

		assertCode(t, codes.Unavailable, w.requireEnded(t))
		assert.True(t, events.sub(0).isClosed())
	})
}

func TestWatchGatewayJSONLines(t *testing.T) {
	ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
	events := &fakeEvents{runs: true}
	opts := testOptions()
	opts.Operations = ops
	opts.Events = events
	h := startAPI(t, opts)

	req, err := http.NewRequestWithContext(callCtx(t), http.MethodGet, h.base+"/api/v1/"+watchedName+":watch", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "json")

	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		require.True(t, lines.Scan(), "stream ended: %v", lines.Err())

		return lines.Text()
	}

	line := next()
	assert.JSONEq(t, `{"result":`+marshalWatched(t, watched(1, dcctlv1.Operation_RUNNING))+`}`, line)

	require.Eventually(t, func() bool { return events.sub(0) != nil }, 5*time.Second, 5*time.Millisecond)
	events.sub(0).push(watched(2, dcctlv1.Operation_RUNNING))
	assert.JSONEq(t, `{"result":`+marshalWatched(t, watched(2, dcctlv1.Operation_RUNNING))+`}`, next())

	// The engine drops the subscription and the copies cannot be read: the
	// stream ends with an error line in the gateway error format.
	ops.fail(errors.New("consul at 10.0.0.1 refused"))
	events.sub(0).drop()

	line = next()
	assert.JSONEq(t, `{"error":{"code":500,"status":"Internal Server Error","detail":"operation cannot be watched"}}`, line)
	assert.False(t, lines.Scan(), "the stream ends after the error")
	assert.NotContains(t, line, "10.0.0.1")
}

func marshalWatched(t *testing.T, op *dcctlv1.Operation) string {
	t.Helper()

	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(op)
	require.NoError(t, err)

	return string(b)
}

func TestWatchGatewayFinalClosesTheStream(t *testing.T) {
	opts := testOptions()
	opts.Operations = liveWith(watched(3, dcctlv1.Operation_SUCCEEDED))
	h := startAPI(t, opts)

	resp := h.get(t, "/api/v1/"+watchedName+":watch")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.JSONEq(t, `{"result":`+marshalWatched(t, watched(3, dcctlv1.Operation_SUCCEEDED))+`}`, resp.body)
}

// TestWatchGatewayFailsBeforeTheFirstMessage checks that a watch that fails
// before it sends anything is answered as every other failed call, with the
// example of the swagger.
func TestWatchGatewayFailsBeforeTheFirstMessage(t *testing.T) {
	unauthenticated := func(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error {
		return apierr.New(apierr.ReasonAuthenticationRequired, "authentication required")
	}

	tests := []struct {
		name   string
		copies []coord.OpCopy
		path   string
		extra  []grpc.StreamServerInterceptor
		code   int
	}{
		{"not_found", []coord.OpCopy{present("dc_a", pagedOperation(1))}, watchedName, nil, http.StatusNotFound},
		{"unavailable", []coord.OpCopy{
			present("dc_a", pagedOperation(1)),
			{Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
		}, watchedName, nil, http.StatusServiceUnavailable},
		{"invalid", nil, "operations/a%20b", nil, http.StatusBadRequest},
		{"unauthenticated", nil, watchedName, []grpc.StreamServerInterceptor{unauthenticated}, http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startAPI(t, withOperations(coord.OpView{Copies: tt.copies}), tt.extra...)

			resp := h.get(t, "/api/v1/"+tt.path+":watch")
			assert.Equal(t, tt.code, resp.code, resp.body)
			assert.JSONEq(t, swaggerExample(t, "/v1/operations/{operation}:watch", "get", strconv.Itoa(tt.code)), resp.body)
		})
	}
}

// A final state that an older change of the same revision hid from the stream
// must still end it: the copies are aligned at that revision and nothing
// newer follows.
func TestWatchFinalAtTheRevisionAlreadySent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := &liveOperations{}
		ops.set(
			present("dc_a", ledBy("dc_a", watched(5, dcctlv1.Operation_INTERRUPTED))),
			coord.OpCopy{Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
		)
		w := watch(t, ops, &fakeEvents{})

		synctest.Wait()
		require.Equal(t, []int64{5}, w.stream.revisions())

		ops.set(
			present("dc_a", ledBy("dc_a", watched(5, dcctlv1.Operation_INTERRUPTED))),
			present("dc_b", ledBy("dc_a", watched(5, dcctlv1.Operation_SUCCEEDED))),
		)
		time.Sleep(watchPollInterval)

		require.NoError(t, w.requireEnded(t))
		assert.Equal(t, []string{"INTERRUPTED", "SUCCEEDED"}, stateNames(w.stream))
	})
}

func stateNames(f *fakeStream) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.sent))
	for _, op := range f.sent {
		out = append(out, op.GetState().String())
	}

	return out
}

// gatedStream blocks the Send of one revision until release is closed.
type gatedStream struct {
	fakeStream

	gate    int64
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (g *gatedStream) Send(op *dcctlv1.Operation) error {
	if op.GetRevision() == g.gate {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}

	return g.fakeStream.Send(op)
}

// When this instance stops running the operation, what the engine published
// and the stream has not read yet is sent before the copies are read, which
// may still be behind.
func TestWatchHandOffSendsBufferedChanges(t *testing.T) {
	for range 20 {
		synctest.Test(t, func(t *testing.T) {
			ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))
			events := &fakeEvents{runs: true}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			stream := &gatedStream{
				fakeStream: fakeStream{ctx: ctx},
				gate:       2, release: make(chan struct{}), entered: make(chan struct{}),
			}
			s := NewOperationServer(ops, events, make(chan struct{}), testLogger())
			done := make(chan error, 1)

			go func() { done <- s.WatchOperation(&dcctlv1.WatchOperationRequest{Name: watchedName}, stream) }()

			synctest.Wait()
			events.sub(0).push(watched(2, dcctlv1.Operation_RUNNING))
			<-stream.entered

			// The engine publishes the final change and stops running the
			// operation while the stream sends; the copy that holds it is
			// not readable.
			events.sub(0).push(watched(3, dcctlv1.Operation_SUCCEEDED))
			events.setRuns(false)
			ops.set(
				present("dc_a", watched(2, dcctlv1.Operation_RUNNING)),
				coord.OpCopy{Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
			)
			time.Sleep(watchPollInterval)
			close(stream.release)
			synctest.Wait()

			select {
			case err := <-done:
				require.NoError(t, err)
				assert.Equal(t, []int64{1, 2, 3}, stream.revisions())
			default:
				require.FailNow(t, "the stream is open after the final change")
			}
		})
	}
}

// Streams that poll share one read of the copies.
func TestWatchFollowersShareTheRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ops := liveWith(watched(1, dcctlv1.Operation_RUNNING))

		const followers = 5

		s := NewOperationServer(ops, nil, nil, testLogger())

		for range followers {
			watchOn(t, s, nil)
		}

		synctest.Wait()
		assert.Equal(t, followers, ops.readCount(), "every stream reads for its first message")

		time.Sleep(3 * watchPollInterval)
		synctest.Wait()
		assert.Equal(t, followers+3, ops.readCount(), "one read per poll for all streams")
	})
}
