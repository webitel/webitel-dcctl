package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/urfave/cli/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

// maxWatchFailures is how many streams in a row may fail before a watch
// gives up. A stream that delivered a message starts the count over.
const maxWatchFailures = 5

func watchCommand(o Options) *cli.Command {
	return leaf(&cli.Command{
		Name:      "watch",
		Usage:     "Follow an operation until it is final",
		ArgsUsage: "<op>",
		Flags:     connFlags(),
		Before: func(c *cli.Context) error {
			if c.NArg() != 1 {
				return usageErrorf("watch: expected an operation, got %d argument(s)", c.NArg())
			}

			_, err := operationArg(c.Args().First())

			return err
		},
		Action: o.run(func(ctx context.Context, c *cli.Context, s *session) error {
			id, _ := operationArg(c.Args().First())

			return s.watchOperation(ctx, id)
		}),
	})
}

// watchOperation follows the operation id and ends with the exit code of its
// final state.
func (s *session) watchOperation(ctx context.Context, id string) error {
	op, err := s.watch(ctx, "operations/"+id, nil)
	if err != nil {
		return s.watchFailed(ctx, id, err)
	}

	return s.report(op, false)
}

// follow watches op, which the server just returned, until it is final, and
// ends with the exit code of its final state. abandoning is set for the
// abandon command, whose success is the ABANDONED state.
func (s *session) follow(ctx context.Context, op *dcctlv1.Operation, abandoning bool) error {
	if op.GetState() == dcctlv1.Operation_RUNNING {
		final, err := s.watch(ctx, op.GetName(), op)
		if err != nil {
			return s.watchFailed(ctx, shortName(op.GetName()), err)
		}

		op = final
	} else if err := s.printFinal(op); err != nil {
		return err
	}

	return s.report(op, abandoning)
}

func (s *session) watchFailed(ctx context.Context, id string, err error) error {
	if ctx.Err() != nil {
		_, _ = fmt.Fprintf(s.errOut, "stopped watching: the operation keeps running, follow it with: webitel-dcctl watch %s\n", id)

		return exit(ExitCanceled)
	}

	return withHint(err, "the operation may still be running: webitel-dcctl watch %s", id)
}

// printFinal shows an operation that was final when the server returned it.
func (s *session) printFinal(op *dcctlv1.Operation) error {
	if s.asJSON {
		return s.printJSON(op)
	}

	printOperation(s.out, op)

	return nil
}

// report turns the final state of op into the exit code: 0 for SUCCEEDED, 2
// when it has warnings, 3 for INTERRUPTED, 1 for FAILED and ABANDONED. An
// operation that is still RUNNING was started with --detach and is a
// success.
func (s *session) report(op *dcctlv1.Operation, abandoning bool) error {
	id := shortName(op.GetName())

	switch op.GetState() {
	case dcctlv1.Operation_SUCCEEDED:
		if hasWarnings(op) {
			return exit(ExitWarnings)
		}

		return nil
	case dcctlv1.Operation_INTERRUPTED:
		_, _ = fmt.Fprintf(s.errOut, "operation %s is interrupted: continue it with \"webitel-dcctl resume %s\" "+
			"or give it up with \"webitel-dcctl abandon %s\"\n", id, id, id)

		return exit(ExitInterrupted)
	case dcctlv1.Operation_FAILED:
		return &Error{Code: ExitFailed, Err: failure(op), JSON: s.asJSON}
	case dcctlv1.Operation_ABANDONED:
		if abandoning {
			return nil
		}

		return &Error{Code: ExitFailed, Err: fmt.Errorf("operation %s was abandoned", id), JSON: s.asJSON}
	case dcctlv1.Operation_RUNNING:
		return nil
	case dcctlv1.Operation_STATE_UNSPECIFIED:
	}

	// A state of a newer server is not a success this client can vouch for.
	return &Error{Code: ExitFailed, Err: fmt.Errorf("operation %s is in state %s, which this client does not know", id, op.GetState()), JSON: s.asJSON}
}

func failure(op *dcctlv1.Operation) error {
	id := shortName(op.GetName())

	if e := op.GetError(); e != nil {
		return fmt.Errorf("operation %s failed: %s [%s]", id, e.GetDetail(), dash(e.GetReason()))
	}

	return fmt.Errorf("operation %s failed", id)
}

func hasWarnings(op *dcctlv1.Operation) bool {
	return len(op.GetWarnings()) > 0 ||
		slices.ContainsFunc(op.GetSteps(), func(st *dcctlv1.Operation_Step) bool { return len(st.GetWarnings()) > 0 })
}

// watch follows the operation until it leaves RUNNING and returns its last
// state. The stream stays open on INTERRUPTED, so the watch ends there as
// well: the operator decides what to do. A stream that breaks is replaced by
// a new one after the operation is read again.
//
// from is the operation the server returned when it started or resumed this
// one, nil for a watch of an operation that was not just changed: no copy older
// than it is taken as a state of the operation, the first message included.
func (s *session) watch(ctx context.Context, name string, from *dcctlv1.Operation) (*dcctlv1.Operation, error) {
	p := &progress{s: s, floor: from}

	for failures := 0; ; {
		op, delivered, err := s.watchOnce(ctx, name, p)
		if err == nil {
			return op, nil
		}

		if delivered {
			failures = 0
		}

		if failures++; failures > maxWatchFailures || !retryable(err) || ctx.Err() != nil {
			return nil, err
		}

		if err := sleep(ctx, s.o.RetryDelay); err != nil {
			return nil, err
		}

		// The operation may have ended while the stream was down.
		op, gerr := call(ctx, s, func(ctx context.Context) (*dcctlv1.Operation, error) {
			return s.operations.GetOperation(ctx, &dcctlv1.GetOperationRequest{Name: name})
		})

		switch {
		case gerr == nil:
			accepted, err := p.update(op)
			if err != nil {
				return nil, err
			}

			if accepted && op.GetState() != dcctlv1.Operation_RUNNING {
				return op, nil
			}
		case !retryable(gerr):
			return nil, gerr
		}
	}
}

// watchOnce reads one stream. It returns the final operation, or the error
// that ended the stream and whether the stream delivered a message first.
func (s *session) watchOnce(ctx context.Context, name string, p *progress) (op *dcctlv1.Operation, delivered bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := s.operations.WatchOperation(ctx, &dcctlv1.WatchOperationRequest{Name: name})
	if err != nil {
		return nil, false, err
	}

	for {
		op, err := stream.Recv()
		if err != nil {
			return nil, delivered, err
		}

		delivered = true

		accepted, err := p.update(op)
		if err != nil {
			return nil, delivered, err
		}

		if accepted && op.GetState() != dcctlv1.Operation_RUNNING {
			// Any other state ends the watch: a final one, INTERRUPTED, which keeps
			// the stream open, and one this client does not know, which report
			// refuses to call a success.
			return op, delivered, nil
		}
	}
}

// retryable reports whether a call that failed this way may work again: the
// server or the network was not there, as opposed to an answer.
func retryable(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}

	st, ok := statusOf(err)
	if !ok {
		return false
	}

	return st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// progress prints what changed in an operation since the last message. The
// server repeats the current state now and then to keep the stream alive,
// which prints nothing.
type progress struct {
	s     *session
	floor *dcctlv1.Operation
	last  *dcctlv1.Operation
	steps map[int32]string
}

// update prints what changed and reports whether op is a newer state than the
// last one: a copy read after a reconnect may be older than what was shown.
func (p *progress) update(op *dcctlv1.Operation) (bool, error) {
	ref := p.last
	if ref == nil {
		ref = p.floor
	}

	if ref != nil && coord.Supersedes(ref, op) {
		return false, nil
	}

	if p.last != nil && proto.Equal(p.last, op) {
		return true, nil
	}

	first := p.last == nil
	prev := p.last
	p.last = op

	if p.s.asJSON {
		return true, writeJSON(p.s.out, op, false)
	}

	w := p.s.out

	if first {
		_, _ = fmt.Fprintf(w, "%s %s -> %s: %s\n", op.GetName(), op.GetType(), dash(shortName(op.GetTarget())), op.GetState())
	} else if prev.GetState() != op.GetState() {
		_, _ = fmt.Fprintf(w, "%s: %s\n", op.GetName(), op.GetState())
	}

	if p.steps == nil {
		p.steps = make(map[int32]string)
	}

	for _, st := range op.GetSteps() {
		line := fmt.Sprintf("step %d %s %s: %s", st.GetIndex(), st.GetType(), dash(shortName(st.GetDatacenter())), st.GetState())
		if st.GetDetail() != "" {
			line += " - " + st.GetDetail()
		}

		for _, warn := range st.GetWarnings() {
			line += "\n  warning: " + warn
		}

		if p.steps[st.GetIndex()] == line {
			continue
		}

		p.steps[st.GetIndex()] = line

		if st.GetState() != dcctlv1.Operation_Step_PENDING {
			_, _ = fmt.Fprintln(w, line)
		}
	}

	if op.GetState() != dcctlv1.Operation_RUNNING {
		for _, warn := range op.GetWarnings() {
			_, _ = fmt.Fprintf(w, "warning: %s\n", warn)
		}

		if e := op.GetError(); e != nil {
			_, _ = fmt.Fprintf(w, "error: %s: %s\n", dash(e.GetReason()), e.GetDetail())
		}
	}

	return true, nil
}
