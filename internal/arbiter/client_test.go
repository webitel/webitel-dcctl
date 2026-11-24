package arbiter_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

const clientTimeout = 5 * time.Second

func newClient(t *testing.T, addr string, files config.TLS, timeout time.Duration) *arbiter.Client {
	t.Helper()

	c, err := arbiter.NewClient(arbiter.ClientOptions{Address: addr, TLS: files, Timeout: timeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

// allowedClient returns a client of a with a certificate for allowedIP.
func allowedClient(t *testing.T, a *testArbiter) *arbiter.Client {
	t.Helper()

	return newClient(t, a.cfg.Listen.API, a.ca.Files(t, allowedIP), clientTimeout)
}

func requireReasonOf(t *testing.T, err error, want source.Reason) {
	t.Helper()

	got, ok := source.ReasonOf(err)
	require.True(t, ok, "error %v is not classified", err)
	assert.Equal(t, want, got, "error = %v", err)
}

func TestClientRecords(t *testing.T) {
	c := allowedClient(t, startArbiter(t))

	_, err := c.Get(t.Context(), arbiter.RecordState)
	require.ErrorIs(t, err, arbiter.ErrUninitialized)

	_, err = c.CAS(t.Context(), arbiter.RecordLease, 0, leaseJSON, true)
	require.ErrorIs(t, err, arbiter.ErrUninitialized)

	state, err := c.CAS(t.Context(), arbiter.RecordState, 0, stateJSON, true)
	require.NoError(t, err)
	assert.Equal(t, stateJSON, state.Value)
	assert.NotZero(t, state.Etag)
	assert.False(t, state.UpdateTime.IsZero())

	got, err := c.Get(t.Context(), arbiter.RecordState)
	require.NoError(t, err)
	assert.Equal(t, state, got)

	_, err = c.Get(t.Context(), arbiter.RecordLease)
	require.ErrorIs(t, err, arbiter.ErrNotFound)

	_, err = c.CAS(t.Context(), arbiter.RecordLease, state.Etag, leaseJSON, false)
	require.ErrorIs(t, err, arbiter.ErrNotFound)

	next, err := c.CAS(t.Context(), arbiter.RecordState, state.Etag, nextJSON, false)
	require.NoError(t, err)
	assert.Greater(t, next.Etag, state.Etag)

	_, err = c.CAS(t.Context(), arbiter.RecordState, state.Etag, stateJSON, false)
	require.ErrorIs(t, err, arbiter.ErrEtagMismatch)

	mismatch, ok := errors.AsType[*arbiter.MismatchError](err)
	require.True(t, ok, "error %v is not a *MismatchError", err)
	assert.Equal(t, next, mismatch.Current)

	_, err = c.CAS(t.Context(), arbiter.RecordState, 0, stateJSON, true)
	require.ErrorIs(t, err, arbiter.ErrEtagMismatch, "create over an existing record")
}

func TestClientInvalidArguments(t *testing.T) {
	c := newClient(t, freeAddr(t), testpki.New(t, "test ca").Files(t, allowedIP), clientTimeout)

	tests := []struct {
		name         string
		record       string
		etag         uint64
		allowMissing bool
	}{
		{"etag_with_allow_missing", arbiter.RecordState, 7, true},
		{"no_etag_without_allow_missing", arbiter.RecordState, 0, false},
		{"unknown_record", "journal", 7, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CAS(t.Context(), tt.record, tt.etag, stateJSON, tt.allowMissing)
			require.Error(t, err)

			_, classified := source.ReasonOf(err)
			assert.False(t, classified, "a bad argument is not the arbiter's failure: %v", err)
		})
	}
}

func TestClientNotListed(t *testing.T) {
	a := startArbiter(t)
	c := newClient(t, a.cfg.Listen.API, a.ca.Files(t, "10.1.0.99"), clientTimeout)

	_, err := c.Get(t.Context(), arbiter.RecordState)
	requireReasonOf(t, err, source.ReasonAuth)

	_, err = c.CAS(t.Context(), arbiter.RecordState, 0, stateJSON, true)
	requireReasonOf(t, err, source.ReasonAuth)
	assert.NotErrorIs(t, err, arbiter.ErrOutcomeUnknown, "a denied write was not applied")
}

func TestClientRefused(t *testing.T) {
	c := newClient(t, freeAddr(t), testpki.New(t, "test ca").Files(t, allowedIP), clientTimeout)

	_, err := c.Get(t.Context(), arbiter.RecordState)
	requireReasonOf(t, err, source.ReasonRefused)

	_, err = c.CAS(t.Context(), arbiter.RecordState, 0, stateJSON, true)
	requireReasonOf(t, err, source.ReasonRefused)
	assert.ErrorIs(t, err, arbiter.ErrOutcomeUnknown)
}

func TestClientServerCertFromForeignCA(t *testing.T) {
	a := startArbiter(t)
	c := newClient(t, a.cfg.Listen.API, testpki.New(t, "foreign ca").Files(t, allowedIP), clientTimeout)

	_, err := c.Get(t.Context(), arbiter.RecordState)
	requireReasonOf(t, err, source.ReasonTLS)
}

func TestClientCertRejectedByServer(t *testing.T) {
	a := startArbiter(t)

	// The client trusts the arbiter but presents a certificate of another CA.
	files := testpki.New(t, "foreign ca").Files(t, allowedIP)
	files.CA = a.cfg.TLS.CA

	c := newClient(t, a.cfg.Listen.API, files, clientTimeout)

	_, err := c.Get(t.Context(), arbiter.RecordState)
	requireReasonOf(t, err, source.ReasonTLS)
}

// silentListener accepts connections and never answers.
func silentListener(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()

			conns = append(conns, conn)

			mu.Unlock()
		}
	})

	t.Cleanup(func() {
		_ = ln.Close()

		wg.Wait()

		for _, c := range conns {
			_ = c.Close()
		}
	})

	return ln.Addr().String()
}

func TestClientTimeout(t *testing.T) {
	c := newClient(t, silentListener(t), testpki.New(t, "test ca").Files(t, allowedIP), 200*time.Millisecond)

	start := time.Now()
	_, err := c.Get(t.Context(), arbiter.RecordState)
	requireReasonOf(t, err, source.ReasonTimeout)
	assert.Less(t, time.Since(start), 3*time.Second, "the call must end at the client timeout")

	_, err = c.CAS(t.Context(), arbiter.RecordState, 0, stateJSON, true)
	requireReasonOf(t, err, source.ReasonTimeout)
	assert.ErrorIs(t, err, arbiter.ErrOutcomeUnknown)
}

func TestClientCanceled(t *testing.T) {
	c := newClient(t, silentListener(t), testpki.New(t, "test ca").Files(t, allowedIP), clientTimeout)

	t.Run("get", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(100*time.Millisecond, cancel)

		_, err := c.Get(ctx, arbiter.RecordState)
		require.ErrorIs(t, err, context.Canceled)

		_, classified := source.ReasonOf(err)
		assert.False(t, classified, "the caller giving up is not the arbiter's failure")
	})

	t.Run("cas", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(100*time.Millisecond, cancel)

		_, err := c.CAS(ctx, arbiter.RecordState, 0, stateJSON, true)
		require.ErrorIs(t, err, context.Canceled)
		assert.ErrorIs(t, err, arbiter.ErrOutcomeUnknown, "a canceled write may have been applied")

		_, classified := source.ReasonOf(err)
		assert.False(t, classified, "the caller giving up is not the arbiter's failure")
	})
}

func TestClientReconnectsAfterRestart(t *testing.T) {
	a := startArbiter(t)
	c := allowedClient(t, a)

	state, err := c.CAS(t.Context(), arbiter.RecordState, 0, stateJSON, true)
	require.NoError(t, err)

	require.NoError(t, a.stop())

	// Calls while the arbiter is down fail and push the reconnect backoff.
	for range 3 {
		_, err := c.Get(t.Context(), arbiter.RecordState)
		requireReasonOf(t, err, source.ReasonRefused)

		time.Sleep(time.Second)
	}

	a.restart(t)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		got, err := c.Get(t.Context(), arbiter.RecordState)
		if assert.NoError(ct, err) {
			assert.Equal(ct, state, got)
		}
	}, 7*time.Second, 100*time.Millisecond, "the client must reconnect within its backoff cap")
}

// fakeArbiter answers every call with resp and err.
type fakeArbiter struct {
	dcctlv1.UnimplementedArbiterServiceServer

	resp *dcctlv1.Record
	err  error
}

func (f fakeArbiter) GetRecord(context.Context, *dcctlv1.GetRecordRequest) (*dcctlv1.Record, error) {
	return f.resp, f.err
}

func (f fakeArbiter) UpdateRecord(context.Context, *dcctlv1.UpdateRecordRequest) (*dcctlv1.Record, error) {
	return f.resp, f.err
}

// serveFake serves f over mTLS and returns its address and the client's TLS files.
func serveFake(t *testing.T, f fakeArbiter) (string, config.TLS) {
	t.Helper()

	ca := testpki.New(t, "test ca")
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "127.0.0.1")},
		ClientCAs:    ca.Pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	})))
	dcctlv1.RegisterArbiterServiceServer(srv, f)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup

	wg.Go(func() { _ = srv.Serve(ln) })
	t.Cleanup(func() {
		srv.Stop()
		wg.Wait()
	})

	return ln.Addr().String(), ca.Files(t, allowedIP)
}

func TestClientInvalidResponse(t *testing.T) {
	goodRecord := &dcctlv1.Record{Name: "records/state", Value: stateJSON, Etag: "7"}

	tests := []struct {
		name string
		f    fakeArbiter
	}{
		{"mismatch_without_record", fakeArbiter{err: apierr.New(apierr.ReasonEtagMismatch, "etag does not match")}},
		{"mismatch_with_bad_etag", fakeArbiter{err: apierr.New(apierr.ReasonEtagMismatch, "etag does not match",
			protoadapt.MessageV1Of(&dcctlv1.Record{Name: "records/state", Value: stateJSON, Etag: "x"}))}},
		{"bad_etag", fakeArbiter{resp: &dcctlv1.Record{Name: "records/state", Value: stateJSON, Etag: "-1"}}},
		{"other_record", fakeArbiter{resp: &dcctlv1.Record{Name: "records/lease", Value: leaseJSON, Etag: "7"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, files := serveFake(t, tt.f)
			c := newClient(t, addr, files, clientTimeout)

			_, err := c.CAS(t.Context(), arbiter.RecordState, 3, stateJSON, false)
			requireReasonOf(t, err, source.ReasonInvalid)
		})
	}

	t.Run("good_record", func(t *testing.T) {
		addr, files := serveFake(t, fakeArbiter{resp: goodRecord})
		c := newClient(t, addr, files, clientTimeout)

		got, err := c.Get(t.Context(), arbiter.RecordState)
		require.NoError(t, err)
		assert.Equal(t, uint64(7), got.Etag)
	})
}

func TestClientServerError(t *testing.T) {
	addr, files := serveFake(t, fakeArbiter{err: status.Error(codes.Internal, "arbiter store failed")})
	c := newClient(t, addr, files, clientTimeout)

	_, err := c.Get(t.Context(), arbiter.RecordState)
	requireReasonOf(t, err, source.ReasonHTTP)
}

func TestNewClientErrors(t *testing.T) {
	files := testpki.New(t, "test ca").Files(t, allowedIP)

	tests := []struct {
		name string
		opts arbiter.ClientOptions
		want string
	}{
		{"no_address", arbiter.ClientOptions{TLS: files, Timeout: time.Second}, "address"},
		{"no_timeout", arbiter.ClientOptions{Address: "10.9.0.10:10024", TLS: files}, "timeout"},
		{"missing_cert", arbiter.ClientOptions{Address: "10.9.0.10:10024", TLS: config.TLS{CA: files.CA, Cert: "/nonexistent", Key: files.Key}, Timeout: time.Second}, "tls.cert"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := arbiter.NewClient(tt.opts)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
