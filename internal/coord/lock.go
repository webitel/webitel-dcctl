package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// ErrLockLost means a session of the lock expired or was invalidated, or
// could not be renewed for its TTL: the lock in that datacenter may be free.
var ErrLockLost = errors.New("lock lost")

// LockBusyError is returned by [Locker.Acquire] when another holder has the
// lock. Its gRPC status is Aborted LOCK_BUSY with metadata holder,
// datacenter (of the holder) and operation.
type LockBusyError struct {
	// Name is the KV where the lock is held.
	Name string
	// Holder is the value of the lock; zero when it cannot be read or the
	// lock is in its lock delay.
	Holder kv.Lock
	// LockDelay is set when no session holds the key but Consul still
	// keeps it after the session of its last holder ended.
	LockDelay bool
}

func (e *LockBusyError) Error() string {
	switch {
	case e.LockDelay:
		return fmt.Sprintf("%s in %s is in its lock delay after the session of its holder ended", kv.LockKey, e.Name)
	case e.Holder.Instance == "":
		return fmt.Sprintf("%s in %s is held by an unknown holder", kv.LockKey, e.Name)
	}

	return fmt.Sprintf("%s in %s is held by instance %s of %s for operation %s",
		kv.LockKey, e.Name, e.Holder.Instance, e.Holder.Datacenter, e.Holder.Operation)
}

// GRPCStatus returns the status the API answers with.
func (e *LockBusyError) GRPCStatus() *status.Status {
	md := make(map[string]string, 3)

	if e.Holder.Instance != "" {
		md["holder"] = e.Holder.Instance
	}

	if e.Holder.Datacenter != "" {
		md["datacenter"] = e.Holder.Datacenter
	}

	if e.Holder.Operation != "" {
		md["operation"] = "operations/" + e.Holder.Operation
	}

	return apierr.Status(apierr.ReasonLockBusy, e.Error(), md)
}

// LockUnavailableError is returned by [Locker.Acquire] when no KV answered,
// so the lock was taken nowhere. Its gRPC status is Unavailable
// LOCK_UNAVAILABLE with metadata datacenters, the unreachable datacenters
// comma-separated; stretch has none, its one KV is the cluster-wide Consul.
type LockUnavailableError struct {
	Unreachable []Unreachable
}

func (e *LockUnavailableError) Error() string {
	names := make([]string, 0, len(e.Unreachable))
	for _, u := range e.Unreachable {
		names = append(names, u.Name)
	}

	return fmt.Sprintf("%s not taken: no kv answered (%s)", kv.LockKey, strings.Join(names, ", "))
}

func (e *LockUnavailableError) Unwrap() []error {
	errs := make([]error, 0, len(e.Unreachable))
	for _, u := range e.Unreachable {
		errs = append(errs, u.Err)
	}

	return errs
}

// GRPCStatus returns the status the API answers with.
func (e *LockUnavailableError) GRPCStatus() *status.Status {
	dcs := make([]string, 0, len(e.Unreachable))
	for _, u := range e.Unreachable {
		if u.Name != StretchName {
			dcs = append(dcs, u.Name)
		}
	}

	var md map[string]string
	if len(dcs) > 0 {
		md = map[string]string{"datacenters": strings.Join(dcs, ",")}
	}

	return apierr.Status(apierr.ReasonLockUnavailable, e.Error(), md)
}

// Unreachable is a KV where the lock was not taken because it did not answer.
type Unreachable struct {
	Name   string
	Reason source.Reason
	Err    error
}

// ReleaseOutcome is what releasing the lock in one KV did.
type ReleaseOutcome string

const (
	ReleaseDone ReleaseOutcome = "released"
	// ReleaseNotHeld means the session no longer held the lock: it was lost
	// before.
	ReleaseNotHeld ReleaseOutcome = "not_held"
	// ReleaseUnknown means the release failed or may not have been applied;
	// the lock is free once its session is destroyed or expires. Err
	// matches [ErrOutcomeUnknown].
	ReleaseUnknown ReleaseOutcome = "unknown"
)

// Released is the result of releasing the lock in one KV.
type Released struct {
	Name    string
	Outcome ReleaseOutcome
	// SessionDestroyed is set when the session was destroyed after the
	// release, which frees a key it still holds after the lock delay.
	SessionDestroyed bool
	Err              error
}

// LockOptions are the timings of a [Locker].
type LockOptions struct {
	// TTL and LockDelay are those of the sessions, consul.Options.SessionTTL
	// and LockDelay.
	TTL       time.Duration
	LockDelay time.Duration
	// Timeout bounds each call to a KV.
	Timeout time.Duration
}

// Locker takes dcctl/lock in the KVs.
type Locker struct {
	kvs     []KV
	ttl     time.Duration
	timeout time.Duration
	log     *logging.Logger
}

// NewLocker returns the locker of kvs, which are locked in name order. A
// lost session is noticed at the next renewal, so the lock delay must
// outlast a renewal interval and a request, and a request must end within
// the renewal interval.
func NewLocker(kvs []KV, opts LockOptions, log *logging.Logger) (*Locker, error) {
	sorted, err := sortedKVs(kvs)
	if err != nil {
		return nil, err
	}

	renewal := opts.TTL / config.RenewalsPerSessionTTL

	switch {
	case opts.TTL <= 0:
		return nil, errors.New("session ttl must be positive")
	case opts.Timeout <= 0:
		return nil, errors.New("kv timeout must be positive")
	case opts.Timeout >= renewal:
		return nil, fmt.Errorf("kv timeout %s must be less than the renewal interval %s", opts.Timeout, renewal)
	case opts.LockDelay <= renewal+opts.Timeout:
		return nil, fmt.Errorf("lock delay %s must be greater than the renewal interval and the kv timeout, %s",
			opts.LockDelay, renewal+opts.Timeout)
	}

	return &Locker{kvs: sorted, ttl: opts.TTL, timeout: opts.Timeout, log: log}, nil
}

// Acquire takes the lock for holder in every KV that answers, in name
// order, without waiting. A lock held by another holder in any KV releases
// the ones taken and returns [*LockBusyError]; a session lost on the way
// returns [ErrLockLost]; no KV answering returns [*LockUnavailableError].
// A KV that does not answer is skipped and reported by [Lock.Unreachable];
// whether the KVs held are enough is for the caller to decide.
//
// Each session is renewed from the moment its lock is taken until
// [Lock.Release], which must be called, also after [Lock.Lost].
func (l *Locker) Acquire(ctx context.Context, holder kv.Lock) (*Lock, error) {
	if holder.Instance == "" || holder.Operation == "" {
		return nil, errors.New("lock holder needs an instance and an operation")
	}

	value, err := json.Marshal(holder)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", kv.LockKey, err)
	}

	renewCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	lk := &Lock{locker: l, value: value, lost: make(chan struct{}), stop: stop}

	for _, s := range l.kvs {
		h, down, err := l.take(ctx, s, holder, value)

		switch {
		case err != nil:
			lk.rollback(ctx)

			return nil, err
		case down != nil:
			lk.unreachable = append(lk.unreachable, *down)

			l.log.WarnCtx(ctx, "lock not taken in unreachable kv",
				logging.Str("kv", s.Name), logging.Str("operation", holder.Operation), logging.Err(down.Err))
		default:
			lk.held = append(lk.held, h)
			lk.wg.Go(func() { lk.renew(renewCtx, h) })
		}
	}

	switch {
	case len(lk.held) == 0:
		lk.rollback(ctx)

		return nil, &LockUnavailableError{Unreachable: lk.unreachable}
	case lk.Err() != nil:
		err := lk.Err()
		lk.rollback(ctx)

		return nil, fmt.Errorf("acquire %s: %w", kv.LockKey, err)
	}

	l.log.InfoCtx(ctx, "lock acquired",
		logging.Str("operation", holder.Operation), logging.Any("held", lk.Held()))

	return lk, nil
}

// take locks s. It returns the held lock, or the reason s is skipped, or
// the error that ends the acquisition.
func (l *Locker) take(ctx context.Context, s KV, holder kv.Lock, value []byte) (*held, *Unreachable, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("acquire %s: %w", kv.LockKey, err)
	}

	created := time.Now()

	sid, err := withTimeout(ctx, l.timeout, func(ctx context.Context) (string, error) {
		return s.Client.CreateSession(ctx, "dcctl lock "+holder.Operation)
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("acquire %s in %s: %w", kv.LockKey, s.Name, err)
		}

		return nil, unreachable(s.Name, err), nil
	}

	ok, err := withTimeout(ctx, l.timeout, func(ctx context.Context) (bool, error) {
		return s.Client.Acquire(ctx, kv.LockKey, value, sid)
	})

	switch {
	case err == nil && ok:
		return &held{kv: s, session: sid, created: created}, nil, nil
	case err == nil:
		busy := l.busy(ctx, s)
		l.destroy(ctx, s, sid)

		return nil, nil, busy
	case errors.Is(err, consul.ErrSessionNotFound):
		return nil, nil, fmt.Errorf("acquire %s in %s: %w: %w", kv.LockKey, s.Name, ErrLockLost, err)
	}

	// The acquire may have been applied before the error: the session must
	// not keep the lock.
	l.destroy(ctx, s, sid)

	if ctx.Err() != nil {
		return nil, nil, fmt.Errorf("acquire %s in %s: %w", kv.LockKey, s.Name, err)
	}

	return nil, unreachable(s.Name, err), nil
}

// busy returns the error for the lock in s held by someone else. A key
// without a session is in its lock delay: its value names a holder that no
// longer holds it.
func (l *Locker) busy(ctx context.Context, s KV) *LockBusyError {
	busy := &LockBusyError{Name: s.Name}

	e, err := withTimeout(ctx, l.timeout, func(ctx context.Context) (*consul.KVEntry, error) {
		e, _, err := s.Client.KVGet(ctx, kv.LockKey)

		return e, err
	})

	switch {
	case err != nil:
	case e.Session == "":
		busy.LockDelay = true

		return busy
	default:
		err = json.Unmarshal(e.Value, &busy.Holder)
	}

	if err != nil {
		busy.Holder = kv.Lock{}

		l.log.WarnCtx(ctx, "lock holder unknown", logging.Str("kv", s.Name), logging.Err(err))
	}

	return busy
}

// destroy destroys session and reports whether it did.
func (l *Locker) destroy(ctx context.Context, s KV, session string) bool {
	_, err := withTimeout(context.WithoutCancel(ctx), l.timeout, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.Client.DestroySession(ctx, session)
	})
	if err != nil {
		l.log.WarnCtx(ctx, "lock session not destroyed, it ends with its ttl",
			logging.Str("kv", s.Name), logging.Err(err))
	}

	return err == nil
}

// release releases the lock in every held KV in parallel and destroys the
// sessions. It is not stopped by the end of ctx: each call is bounded by
// the timeout, and a lock left behind would block others for the lock
// delay.
func (l *Locker) release(ctx context.Context, held []*held, value []byte) []Released {
	ctx = context.WithoutCancel(ctx)
	out := make([]Released, len(held))

	var wg sync.WaitGroup

	for i, h := range held {
		wg.Go(func() { out[i] = l.releaseOne(ctx, h, value) })
	}

	wg.Wait()

	return out
}

func (l *Locker) releaseOne(ctx context.Context, h *held, value []byte) Released {
	r := Released{Name: h.kv.Name}

	ok, err := withTimeout(ctx, l.timeout, func(ctx context.Context) (bool, error) {
		return h.kv.Client.Release(ctx, kv.LockKey, value, h.session)
	})

	switch {
	case err == nil && ok:
		r.Outcome = ReleaseDone
	case err == nil:
		r.Outcome = ReleaseNotHeld
	case errors.Is(err, consul.ErrSessionNotFound):
		r.Outcome, r.Err = ReleaseNotHeld, err

		return r
	default:
		r.Outcome, r.Err = ReleaseUnknown, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}

	r.SessionDestroyed = l.destroy(ctx, h.kv, h.session)

	return r
}

// Lock is dcctl/lock held in some KVs. Its sessions are renewed until
// [Lock.Release].
type Lock struct {
	locker      *Locker
	value       []byte
	held        []*held
	unreachable []Unreachable

	stop context.CancelFunc
	wg   sync.WaitGroup

	lost     chan struct{}
	lostOnce sync.Once
	lostErr  error

	releaseOnce sync.Once
	released    []Released
}

type held struct {
	kv      KV
	session string
	// created is when the session was requested; its TTL runs from then.
	created time.Time
}

// Held returns the names of the KVs where the lock is held, in name order.
func (lk *Lock) Held() []string {
	out := make([]string, 0, len(lk.held))
	for _, h := range lk.held {
		out = append(out, h.kv.Name)
	}

	return out
}

// Unreachable returns the KVs skipped because they did not answer.
func (lk *Lock) Unreachable() []Unreachable { return lk.unreachable }

// Lost is closed when a session of the lock is lost, see [ErrLockLost]; the
// operation holding the lock is then interrupted. It is not closed by
// [Lock.Release].
func (lk *Lock) Lost() <-chan struct{} { return lk.lost }

// Err returns why the lock was lost, or nil while it is held.
func (lk *Lock) Err() error {
	select {
	case <-lk.lost:
		return lk.lostErr
	default:
		return nil
	}
}

// Release stops the renewal, releases the lock in every KV it was taken in
// and destroys the sessions. A canceled ctx does not stop it. A later call
// returns the same result.
func (lk *Lock) Release(ctx context.Context) []Released {
	lk.releaseOnce.Do(func() {
		lk.released = lk.stopAndRelease(ctx)

		outcomes := make(map[string]string, len(lk.released))
		for _, r := range lk.released {
			outcomes[r.Name] = string(r.Outcome)
		}

		lk.locker.log.InfoCtx(ctx, "lock released", logging.Any("outcomes", outcomes))
	})

	return lk.released
}

// rollback releases what a failed acquisition took.
func (lk *Lock) rollback(ctx context.Context) {
	released := lk.stopAndRelease(ctx)

	var failed []string

	for _, r := range released {
		if r.Outcome != ReleaseDone {
			failed = append(failed, fmt.Sprintf("%s: %s: %v", r.Name, r.Outcome, r.Err))
		}
	}

	if len(failed) > 0 {
		lk.locker.log.WarnCtx(ctx, "lock not released cleanly after a failed acquisition",
			logging.Any("releases", failed))
	}
}

func (lk *Lock) stopAndRelease(ctx context.Context) []Released {
	lk.stop()
	lk.wg.Wait()

	released := lk.locker.release(ctx, lk.held, lk.value)

	for _, r := range released {
		if r.Outcome == ReleaseUnknown {
			lk.locker.log.WarnCtx(ctx, "lock release outcome is unknown",
				logging.Str("kv", r.Name), logging.Bool("session_destroyed", r.SessionDestroyed), logging.Err(r.Err))
		}
	}

	return released
}

func (lk *Lock) lose(ctx context.Context, err error) {
	lk.lostOnce.Do(func() {
		lk.lostErr = err
		close(lk.lost)

		lk.locker.log.WarnCtx(ctx, "lock lost", logging.Err(err))
	})
}

// renew renews the session of h until ctx ends or the session is gone.
func (lk *Lock) renew(ctx context.Context, h *held) {
	l := lk.locker
	t := time.NewTicker(l.ttl / config.RenewalsPerSessionTTL)

	defer t.Stop()

	confirmed := h.created

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		_, err := withTimeout(ctx, l.timeout, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, h.kv.Client.RenewSession(ctx, h.session)
		})

		switch {
		case err == nil:
			confirmed = time.Now()
		case ctx.Err() != nil:
			return
		case errors.Is(err, consul.ErrSessionNotFound):
			lk.lose(ctx, fmt.Errorf("%s in %s: %w: %w", kv.LockKey, h.kv.Name, ErrLockLost, err))

			return
		case time.Since(confirmed) >= l.ttl:
			// Keep renewing: the session may still be there, and holding it
			// keeps the lock from others until the release.
			lk.lose(ctx, fmt.Errorf("%s in %s: %w: not renewed for %s: %w", kv.LockKey, h.kv.Name, ErrLockLost, l.ttl, err))
		default:
			l.log.WarnCtx(ctx, "lock session not renewed", logging.Str("kv", h.kv.Name), logging.Err(err))
		}
	}
}

func unreachable(name string, err error) *Unreachable {
	reason, _ := source.ReasonOf(err)

	return &Unreachable{Name: name, Reason: reason, Err: err}
}

func withTimeout[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return fn(ctx)
}
