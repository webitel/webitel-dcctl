// Package source holds what the Consul, Nomad and Patroni clients share: the reason an
// endpoint of a source could not be reached or used. The reason is shown in the status.
package source

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
)

// Reason is why an endpoint could not be reached or used.
type Reason string

const (
	ReasonTimeout Reason = "timeout"
	ReasonRefused Reason = "refused"
	ReasonDNS     Reason = "dns"
	ReasonTLS     Reason = "tls"
	// ReasonAuth is an HTTP 401 or 403.
	ReasonAuth Reason = "auth"
	// ReasonHTTP is any other non-2xx status or failure of the HTTP exchange.
	ReasonHTTP Reason = "http"
	// ReasonInvalid is a response that cannot be decoded or breaks the source's API.
	ReasonInvalid Reason = "invalid"
	// ReasonNoEndpoints is a source with no endpoint to ask, such as a service without
	// passing instances in Consul.
	ReasonNoEndpoints Reason = "no_endpoints"
	// ReasonNoLeader is a Consul that answered a stale read without knowing its leader,
	// as the minority side of a partition does: what it gives may be outdated, and a
	// consistent read would fail there.
	ReasonNoLeader Reason = "no_leader"
)

var (
	// ErrInvalidResponse marks a response that cannot be decoded or breaks the source's API.
	ErrInvalidResponse = errors.New("invalid response")
	// ErrNoEndpoints marks a source with no endpoint to ask.
	ErrNoEndpoints = errors.New("no endpoints")
)

// Error reports that an endpoint could not be reached or used.
type Error struct {
	// Endpoint is host:port of the endpoint.
	Endpoint string
	Reason   Reason
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s: %v", e.Endpoint, e.Reason, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// StatusError is a non-2xx HTTP response. Clients return it so that [Classify] tells
// [ReasonAuth] from [ReasonHTTP].
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected HTTP status %d", e.Code)
}

// Classify wraps err into an [*Error] for endpoint with the reason err matches.
// It returns nil for nil, err as is when it already holds an [*Error], and err as is
// for [context.Canceled], which is the caller giving up rather than the endpoint failing.
func Classify(endpoint string, err error) error {
	if err == nil {
		return nil
	}

	if _, ok := errors.AsType[*Error](err); ok {
		return err
	}

	if errors.Is(err, context.Canceled) {
		return err
	}

	return &Error{Endpoint: endpoint, Reason: reasonFor(err), Err: err}
}

// ReasonOf returns the reason of the first [*Error] in err's chain.
func ReasonOf(err error) (Reason, bool) {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		return "", false
	}

	return e.Reason, true
}

func reasonFor(err error) Reason {
	if errors.Is(err, ErrNoEndpoints) {
		return ReasonNoEndpoints
	}

	if se, ok := errors.AsType[*StatusError](err); ok {
		if se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden {
			return ReasonAuth
		}

		return ReasonHTTP
	}

	if isInvalid(err) {
		return ReasonInvalid
	}

	// A DNS error is also a net.Error, so it goes before the timeout check.
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return ReasonDNS
	}

	if isTLS(err) {
		return ReasonTLS
	}

	if isTimeout(err) {
		return ReasonTimeout
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return ReasonRefused
	}

	return ReasonHTTP
}

func isInvalid(err error) bool {
	if errors.Is(err, ErrInvalidResponse) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// An EOF inside a *url.Error is the connection closing before any response, which is
	// not an invalid response; a bare EOF is a body that ended where JSON was expected.
	if errors.Is(err, io.EOF) {
		_, transport := errors.AsType[*url.Error](err)

		return !transport
	}

	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		return true
	}

	_, ok := errors.AsType[*json.UnmarshalTypeError](err)

	return ok
}

func isTLS(err error) bool {
	// A plain HTTP answer to an HTTPS request means the endpoint does not speak TLS.
	if errors.Is(err, http.ErrSchemeMismatch) {
		return true
	}

	// An alert sent by the server, such as a missing client certificate, reaches the client
	// as a "remote error" after the handshake, in a type the tls package does not export.
	if oe, ok := errors.AsType[*net.OpError](err); ok && oe.Op == "remote error" {
		return true
	}

	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}

	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}

	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}

	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return true
	}

	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}

	_, ok := errors.AsType[tls.AlertError](err)

	return ok
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	ne, ok := errors.AsType[net.Error](err)

	return ok && ne.Timeout()
}
