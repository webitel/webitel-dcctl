package api

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

const (
	// watchPollInterval is how often a stream reads the copies of dcctl/op
	// and checks whether this instance runs the operation.
	watchPollInterval = 2 * time.Second
	// watchKeepalive is how long a stream stays silent before it repeats the
	// current state.
	watchKeepalive = 30 * time.Second
)

// WatchOperation streams the operation after every change, first its current
// state. On the instance that runs the operation the changes come from the
// engine; on any other they are read from the copies of dcctl/op. The stream
// follows the operation when it moves between instances. A change is sent
// only when it supersedes the last one sent, see [coord.Supersedes], and the
// current state is repeated when nothing was sent for [watchKeepalive]. The
// stream ends with OK after a final state; an INTERRUPTED operation may be
// resumed and keeps it open.
func (s *OperationServer) WatchOperation(req *dcctlv1.WatchOperationRequest, stream grpc.ServerStreamingServer[dcctlv1.Operation]) error {
	w := &watcher{s: s, name: req.GetName(), send: stream.Send}
	defer w.close()

	return w.run(stream.Context())
}

type watcher struct {
	s    *OperationServer
	name string
	send func(*dcctlv1.Operation) error

	// sub is set while this instance runs the operation.
	sub  OperationSubscription
	last *dcctlv1.Operation
	// keepalive fires when nothing was sent for the keepalive.
	keepalive *time.Timer
}

func (w *watcher) close() {
	if w.sub != nil {
		w.sub.Close()
	}
}

func (w *watcher) run(ctx context.Context) error {
	poll := time.NewTicker(w.s.pollInterval)
	defer poll.Stop()

	w.keepalive = time.NewTimer(w.s.keepalive)
	defer w.keepalive.Stop()

	done, err := w.follow(ctx)

	for err == nil && !done {
		var changes <-chan *dcctlv1.Operation
		if w.sub != nil {
			changes = w.sub.C()
		}

		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-w.s.stop:
			return status.Error(codes.Unavailable, "service is shutting down")
		case op, ok := <-changes:
			if !ok {
				// The engine dropped a subscriber that did not keep up: read
				// the operation again and subscribe anew.
				w.sub = nil
				done, err = w.follow(ctx)

				continue
			}

			done, err = w.emit(op)
		case <-poll.C:
			done, err = w.follow(ctx)
		case <-w.keepalive.C:
			err = w.send(w.last)
			w.keepalive.Reset(w.s.keepalive)
		}
	}

	if err != nil {
		return rpcError(ctx, w.s.log, "operation cannot be watched", err)
	}

	return nil
}

// follow brings the stream in line with who runs the operation and sends what
// the copies hold. This instance subscribes before it reads, so a change made
// in between is not lost; [coord.Supersedes] drops what is sent twice.
func (w *watcher) follow(ctx context.Context) (done bool, err error) {
	runs := w.s.events != nil && w.s.events.Runs(w.name)

	switch {
	case runs && w.sub != nil:
		return false, nil
	case runs:
		w.sub = w.s.events.Subscribe(w.name)
	case w.sub != nil:
		sub := w.sub
		w.sub = nil
		sub.Close()

		// The engine may have published a last change that the copies do not
		// show yet.
		for op := range sub.C() {
			if done, err := w.emit(op); done || err != nil {
				return done, err
			}
		}

		return w.read(ctx, false)
	}

	return w.read(ctx, !runs)
}

// read sends the operation as the copies hold it. Before the first message a
// missing operation is an error, see [lookup]. After it, copies that do not
// answer are waited for, but an operation that every copy answered for is
// gone.
func (w *watcher) read(ctx context.Context, shared bool) (done bool, err error) {
	read := w.s.ops.Read
	if shared && w.last != nil {
		read = w.s.polled.read
	}

	view, err := read(ctx)
	if err != nil {
		return false, err
	}

	op, err := lookup(view, w.name)
	if err != nil {
		if w.last != nil && status.Code(err) == codes.Unavailable {
			return false, nil
		}

		return false, err
	}

	return w.emit(op)
}

// emit sends op unless it is not newer than the last one sent.
func (w *watcher) emit(op *dcctlv1.Operation) (done bool, err error) {
	if w.last != nil && !coord.Supersedes(op, w.last) {
		return false, nil
	}

	if err := w.send(op); err != nil {
		return false, err
	}

	w.last = op
	w.keepalive.Reset(w.s.keepalive)

	return coord.Finished(op.GetState()), nil
}

// sharedView lets the polling streams of an instance share one read of the
// copies: a read younger than ttl answers the others.
type sharedView struct {
	ops Operations
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	view coord.OpView
}

func (v *sharedView) read(ctx context.Context) (coord.OpView, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !v.at.IsZero() && time.Since(v.at) < v.ttl {
		return v.view, nil
	}

	view, err := v.ops.Read(ctx)
	if err != nil {
		return coord.OpView{}, err
	}

	v.at, v.view = time.Now(), view

	return view, nil
}
