package pgctl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/source"
)

// primitive is what one primitive checks, sends and waits for.
type primitive struct {
	action  Action
	dc      string
	timeout time.Duration
	// prepare checks that the cluster is the one to change and in a state to start from,
	// before the first request and before each resend. An error wrapping ErrPrecondition
	// ends the primitive; any other one is retried. Nil when there is nothing to check.
	prepare func(ctx context.Context, first bool) (Observation, error)
	// send makes the request; nil when the primitive only waits.
	send func(ctx context.Context) error
	// resend allows sending the request again while its outcome is unknown.
	resend bool
	// skip, when set and true after the check, leaves out the request: the condition is
	// met already.
	skip func() bool
	// observe reads the condition. An error wrapping ErrPrecondition ends the primitive.
	observe func(ctx context.Context) (obs Observation, done bool, err error)
}

// run is one call of a primitive.
type run struct {
	log    *logging.Logger
	p      primitive
	opts   Options
	result Result
	// readFailing is set while the last poll could not read the state.
	readFailing bool
	// unknownSent is set once an attempt had an unknown outcome: it may have been
	// applied, and a refusal of a later attempt may come from that.
	unknownSent bool
}

func (c *Controller) run(ctx context.Context, p primitive, opts Options) (Result, error) {
	r := &run{log: c.log, p: p, opts: opts, result: Result{Datacenter: p.dc, Action: p.action, Request: RequestNone}}

	if p.send != nil && opts.Guard == nil {
		return r.result, fmt.Errorf("%s %s: no guard", p.action, p.dc)
	}

	// The request runs beside the polls and is waited for after its context is canceled.
	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithTimeoutCause(ctx, p.timeout, ErrTimeout)
	defer cancel()

	r.log.InfoCtx(ctx, "patroni primitive started", r.fields()...)
	r.report(PhaseCheck)

	if err := r.check(ctx); err != nil {
		return r.fail(ctx, err)
	}

	var (
		answer   <-chan error
		lastSend time.Time
	)

	if p.send != nil && (p.skip == nil || !p.skip()) {
		if err := r.guard(ctx); err != nil {
			return r.fail(ctx, err)
		}

		r.result.Request = RequestUnknown
		r.report(PhaseRequest)

		answer, lastSend = r.launch(ctx, &wg), time.Now()
	}

	for {
		obs, done, err := r.poll(ctx)
		if err != nil {
			return r.fail(ctx, err)
		}

		if done {
			r.result.Observation = obs
			r.report(PhaseDone)
			r.log.InfoCtx(ctx, "patroni primitive done", r.fields()...)

			return r.result, nil
		}

		if answer == nil && r.canResend(lastSend) {
			sent, err := r.resend(ctx)
			if err != nil {
				return r.fail(ctx, err)
			}

			if sent {
				answer, lastSend = r.launch(ctx, &wg), time.Now()
			}
		}

		t := time.NewTimer(pollInterval)

		select {
		case <-ctx.Done():
			t.Stop()

			return r.fail(ctx, context.Cause(ctx))
		case err := <-answer:
			t.Stop()

			answer = nil

			if r.answered(ctx, err) {
				return r.refused(ctx, err)
			}
		case <-t.C:
		}
	}
}

// check runs prepare until it passes, fails the precondition or ctx ends.
func (r *run) check(ctx context.Context) error {
	if r.p.prepare == nil {
		return nil
	}

	logged := false

	for {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		obs, err := r.p.prepare(readCtx, true)

		cancel()

		switch {
		case ctx.Err() != nil:
			return context.Cause(ctx)
		case err == nil, errors.Is(err, ErrPrecondition):
			r.result.Observation = obs

			return err
		}

		r.result.Observation = obs
		r.report(PhaseCheck)

		if !logged {
			r.log.WarnCtx(ctx, "patroni state not read, retrying", r.fields(logging.Err(err))...)

			logged = true
		}

		if !sleep(ctx, pollInterval) {
			return fmt.Errorf("%w: %w", context.Cause(ctx), err)
		}
	}
}

// poll reads the condition once under the read timeout. A read cut by the end of ctx
// says nothing and is not reported.
func (r *run) poll(ctx context.Context) (Observation, bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	obs, done, err := r.p.observe(readCtx)

	switch {
	case ctx.Err() != nil:
		return obs, false, nil
	case err != nil:
		r.result.Observation = obs

		return obs, false, err
	case done:
		return obs, true, nil
	}

	switch {
	case obs.Err != nil && !r.readFailing:
		r.log.WarnCtx(ctx, "patroni state not read, retrying", r.fields(logging.Err(obs.Err))...)
	case obs.Err == nil && r.readFailing:
		r.log.InfoCtx(ctx, "patroni state read again", r.fields()...)
	}

	r.readFailing = obs.Err != nil
	r.result.Observation = obs
	r.report(PhaseWait)

	return obs, false, nil
}

// guard runs the caller's guard. A guard that fails because ctx ended is not a refusal:
// the cause of ctx is returned instead.
func (r *run) guard(ctx context.Context) error {
	err := r.opts.Guard(ctx)

	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return context.Cause(ctx)
	}

	return fmt.Errorf("%w: %w", ErrGuard, err)
}

// launch sends the request in a goroutine bounded by ctx; the answer comes on the channel.
func (r *run) launch(ctx context.Context, wg *sync.WaitGroup) <-chan error {
	answer := make(chan error, 1)

	wg.Go(func() { answer <- r.p.send(ctx) })

	return answer
}

func (r *run) canResend(lastSend time.Time) bool {
	return r.p.resend && r.result.Request == RequestUnknown && time.Since(lastSend) >= resendInterval
}

// resend checks the cluster and asks the guard again before a request is sent again. A
// check that cannot read the state skips this round.
func (r *run) resend(ctx context.Context) (bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	_, err := r.p.prepare(readCtx, false)

	cancel()

	switch {
	case ctx.Err() != nil:
		return false, nil
	case errors.Is(err, ErrPrecondition):
		return false, err
	case err != nil:
		return false, nil
	}

	if err := r.guard(ctx); err != nil {
		return false, err
	}

	r.log.InfoCtx(ctx, "patroni request sent again", r.fields()...)

	return true, nil
}

// answered records the answer to the request and reports whether Patroni refused it.
// Once an attempt had an unknown outcome a refusal is not final: it may be the conflict
// with that attempt applied, so the outcome stays unknown.
func (r *run) answered(ctx context.Context, err error) bool {
	switch {
	case err == nil:
		r.result.Request = RequestApplied
	case isRefusal(err) && !r.unknownSent:
		r.result.Request = RequestRefused

		return true
	default:
		r.result.Request, r.unknownSent = RequestUnknown, true
		r.log.WarnCtx(ctx, "patroni request outcome unknown, waiting for the condition", r.fields(logging.Err(err))...)
	}

	return false
}

// refused ends a primitive whose request Patroni refused, unless the condition is met
// already, as for a switchover that a previous attempt did.
func (r *run) refused(ctx context.Context, refusal error) (Result, error) {
	obs, done, err := r.poll(ctx)

	switch {
	case err != nil:
		return r.fail(ctx, err)
	case done:
		r.result.Observation = obs
		r.report(PhaseDone)

		return r.result, nil
	}

	return r.fail(ctx, fmt.Errorf("%w: %w", ErrRefused, refusal))
}

// fail records the end of the primitive with cause. A request with an unknown outcome
// adds ErrOutcomeUnknown.
func (r *run) fail(ctx context.Context, cause error) (Result, error) {
	r.report(PhaseFailed)

	if r.result.Request == RequestUnknown {
		cause = fmt.Errorf("%w: %w", ErrOutcomeUnknown, cause)
	}

	err := fmt.Errorf("%s %s: %w", r.p.action, r.p.dc, cause)
	if d := r.result.Detail; d != "" {
		err = fmt.Errorf("%s %s: %s: %w", r.p.action, r.p.dc, d, cause)
	}

	r.log.WarnCtx(ctx, "patroni primitive failed", r.fields(logging.Err(err))...)

	return r.result, err
}

func (r *run) report(phase Phase) {
	if r.opts.Progress != nil {
		r.opts.Progress(Progress{Datacenter: r.p.dc, Action: r.p.action, Phase: phase, Observation: r.result.Observation})
	}
}

func (r *run) fields(extra ...logging.Field) []logging.Field {
	return append([]logging.Field{
		logging.Str("datacenter", r.p.dc), logging.Str("action", string(r.p.action)),
		logging.Str("request", string(r.result.Request)), logging.Str("leader", r.result.Leader),
	}, extra...)
}

// isRefusal reports whether err is a 4xx answer: Patroni checks a request before it
// changes anything, so nothing was changed.
func isRefusal(err error) bool {
	se, ok := errors.AsType[*source.StatusError](err)

	return ok && se.Code >= 400 && se.Code < 500
}

// sleep waits for d and reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
