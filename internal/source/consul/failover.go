package consul

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul/api"

	"github.com/webitel/webitel-dcctl/internal/source"
)

var (
	// ErrKeyNotFound is returned by [Client.KVGet] for a key that does not exist.
	ErrKeyNotFound = errors.New("key not found")
	// ErrSessionNotFound is returned for a session that expired or was destroyed.
	ErrSessionNotFound = errors.New("session not found")
	// ErrOutcomeUnknown marks a write (CAS, delete, release) that was sent to a server that
	// then failed to answer: it may have been applied, and it is not retried on another
	// server. The error is also classified, see [source.ReasonOf].
	ErrOutcomeUnknown = errors.New("outcome of the write is unknown")
	// ErrUnknownDatacenter is returned by [Clients.Client] for a datacenter that is not in consul.servers.
	ErrUnknownDatacenter = errors.New("unknown datacenter")
)

// pool is the servers of one Consul, tried in order. The server that answered last is
// tried first next time. It is shared by every [Client] of that Consul.
type pool struct {
	endpoints []endpoint
	timeout   time.Duration
	current   atomic.Int64
}

type endpoint struct {
	addr   string
	client *api.Client
}

// newPool builds the clients over plain HTTP, which is how Consul is reached without ACL.
// It sets the config itself rather than api.DefaultConfig, so that CONSUL_* variables
// of the environment (a token, TLS) do not change what dcctl talks to.
func newPool(addrs []string, timeout time.Duration) (*pool, error) {
	p := &pool{timeout: timeout}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	hc := &http.Client{Transport: transport}

	for _, addr := range addrs {
		client, err := api.NewClient(&api.Config{Address: addr, Scheme: "http", HttpClient: hc})
		if err != nil {
			return nil, fmt.Errorf("consul client for %s: %w", addr, err)
		}

		p.endpoints = append(p.endpoints, endpoint{addr: addr, client: client})
	}

	return p, nil
}

// do runs fn against the current server and then the others until one succeeds. fn must
// only publish results on success.
//
//   - A 4xx is the answer of a working server and ends the walk.
//   - A write that is not idempotent also ends it once its request has been sent: a server
//     that applied it and lost the response would make a retry report a lost race. The
//     error then matches [ErrOutcomeUnknown].
//   - A session that is gone ([ErrSessionNotFound]) and a canceled context are returned as is.
//
// Every other failure is classified by [source.Classify], one per server tried, and joined.
func (p *pool) do(ctx context.Context, idempotent bool, fn func(context.Context, *api.Client) error) error {
	var (
		n     = len(p.endpoints)
		start = int(p.current.Load()) % n
		errs  []error
	)

	for i := range n {
		idx := (start + i) % n
		ep := p.endpoints[idx]

		sent, err := p.attempt(ctx, ep, fn)
		if err == nil {
			p.current.Store(int64(idx))

			return nil
		}

		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, context.Canceled) {
			return err
		}

		err = source.Classify(ep.addr, sourceErr(err))
		answer := isClientError(err)

		if !idempotent && sent && !answer {
			return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
		}

		errs = append(errs, err)

		if answer || ctx.Err() != nil {
			break
		}
	}

	if len(errs) == 1 {
		return errs[0]
	}

	return errors.Join(errs...)
}

// attempt runs fn once and reports whether its request was completely written.
func (p *pool) attempt(ctx context.Context, ep endpoint, fn func(context.Context, *api.Client) error) (bool, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	var sent atomic.Bool

	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				sent.Store(true)
			}
		},
	})

	err := fn(ctx, ep.client)

	return sent.Load(), err
}

// sourceErr maps what the Consul API client returns onto what [source.Classify] knows,
// keeping the original chain.
func sourceErr(err error) error {
	if se, ok := errors.AsType[api.StatusError](err); ok {
		return fmt.Errorf("%w: %w", &source.StatusError{Code: se.Code}, err)
	}

	return err
}

func isClientError(err error) bool {
	se, ok := errors.AsType[*source.StatusError](err)

	return ok && se.Code < http.StatusInternalServerError
}

// invalidSession turns the answer of Consul to a lock call with a session that is gone
// into [ErrSessionNotFound]; the text of the body is the only signal Consul gives.
func invalidSession(err error) error {
	if se, ok := errors.AsType[api.StatusError](err); ok && strings.Contains(se.Body, "invalid session") {
		return fmt.Errorf("%w: %w", ErrSessionNotFound, err)
	}

	return err
}
