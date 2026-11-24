package coord_test

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/arbiter/store"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/testpki"
)

const testTimeout = 5 * time.Second

// consulKV serves the KV endpoints of the Consul HTTP API the client uses.
type consulKV struct {
	mu      sync.Mutex
	index   uint64
	values  map[string][]byte
	indexes map[string]uint64
	// failWrites answers every write with 500 after reading it.
	failWrites bool
}

type kvPair struct {
	Key         string
	Value       []byte
	ModifyIndex uint64
}

func (f *consulKV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, "/v1/kv/")
	if !ok {
		http.NotFound(w, r)

		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		v, ok := f.values[key]
		if !ok {
			http.NotFound(w, r)

			return
		}

		_ = json.NewEncoder(w).Encode([]kvPair{{Key: key, Value: v, ModifyIndex: f.indexes[key]}})
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)

		if f.failWrites {
			http.Error(w, "rpc error", http.StatusInternalServerError)

			return
		}

		cas, err := strconv.ParseUint(r.URL.Query().Get("cas"), 10, 64)
		if err != nil {
			http.Error(w, "cas is required", http.StatusBadRequest)

			return
		}

		if f.indexes[key] != cas {
			_, _ = io.WriteString(w, "false")

			return
		}

		f.index++
		f.values[key] = body
		f.indexes[key] = f.index
		_, _ = io.WriteString(w, "true")
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func startConsulKV(t *testing.T) (*consulKV, string) {
	t.Helper()

	f := &consulKV{index: 100, values: map[string][]byte{}, indexes: map[string]uint64{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	return f, strings.TrimPrefix(srv.URL, "http://")
}

func newConsulClients(t *testing.T, profile config.Profile, servers ...config.ConsulServer) *consul.Clients {
	t.Helper()

	clients, err := consul.New(consul.Options{
		Profile:     profile,
		NodeMetaKey: "dc",
		Servers:     servers,
		SessionTTL:  15 * time.Second,
		Timeout:     testTimeout,
	})
	require.NoError(t, err)

	return clients
}

func newKVVoter(t *testing.T, addr string) coord.Voter {
	t.Helper()

	clients := newConsulClients(t, config.ProfileWarmStandby, config.ConsulServer{Address: addr, Datacenter: "dc_a"})

	c, err := clients.Client("dc_a")
	require.NoError(t, err)

	return coord.NewKVVoter("dc_a", c)
}

func TestKVVoter(t *testing.T) {
	ctx := t.Context()
	fake, addr := startConsulKV(t)
	v := newKVVoter(t, addr)

	assert.Equal(t, "dc_a", v.Name())
	assert.Equal(t, coord.KindKV, v.Kind())

	_, err := v.Get(ctx, coord.KeyState)
	require.ErrorIs(t, err, coord.ErrNotFound)

	require.NoError(t, v.CAS(ctx, coord.KeyState, []byte(`{"generation":1}`), 0))
	assert.Equal(t, []byte(`{"generation":1}`), fake.values[kv.StateKey])

	got, err := v.Get(ctx, coord.KeyState)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"generation":1}`), got.Value)
	assert.Equal(t, coord.Version(fake.indexes[kv.StateKey]), got.Version)

	err = v.CAS(ctx, coord.KeyState, []byte(`{"generation":2}`), 0)
	require.ErrorIs(t, err, coord.ErrVersionMismatch, "create over an existing key")

	err = v.CAS(ctx, coord.KeyState, []byte(`{"generation":2}`), got.Version+1)
	require.ErrorIs(t, err, coord.ErrVersionMismatch)

	require.NoError(t, v.CAS(ctx, coord.KeyState, []byte(`{"generation":2}`), got.Version))
	assert.Equal(t, []byte(`{"generation":2}`), fake.values[kv.StateKey])

	require.NoError(t, v.CAS(ctx, coord.KeyLease, []byte(`{"counter":1}`), 0))
	assert.Equal(t, []byte(`{"counter":1}`), fake.values[kv.LeaseKey])
}

func TestKVVoterUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	dead := ln.Addr().String()
	require.NoError(t, ln.Close())

	v := newKVVoter(t, dead)

	_, err = v.Get(t.Context(), coord.KeyState)
	require.Error(t, err)
	assert.NotErrorIs(t, err, coord.ErrNotFound)

	reason, ok := source.ReasonOf(err)
	require.True(t, ok, "error %v is not classified", err)
	assert.Equal(t, source.ReasonRefused, reason)
}

func TestKVVoterOutcomeUnknown(t *testing.T) {
	fake, addr := startConsulKV(t)
	fake.failWrites = true
	v := newKVVoter(t, addr)

	err := v.CAS(t.Context(), coord.KeyState, []byte(`{}`), 0)
	require.ErrorIs(t, err, coord.ErrOutcomeUnknown)

	_, ok := source.ReasonOf(err)
	assert.True(t, ok, "error %v lost its classification", err)
}

// startArbiter serves the arbiter over an empty store and returns its
// client.
func startArbiter(t *testing.T) *arbiter.Client {
	t.Helper()

	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ca := testpki.New(t, "test ca")
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "127.0.0.1")},
		ClientCAs:    ca.Pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	})))
	srv.RegisterService(&dcctlv1.ArbiterService_ServiceDesc,
		arbiter.NewServer(st, logging.New(logging.WithProvider(logtest.NewRecorder()))))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = srv.Serve(ln)
	}()

	t.Cleanup(func() {
		srv.Stop()
		<-done
	})

	c, err := arbiter.NewClient(arbiter.ClientOptions{
		Address: ln.Addr().String(),
		TLS:     ca.Files(t, "127.0.0.1"),
		Timeout: testTimeout,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestArbiterVoter(t *testing.T) {
	ctx := t.Context()
	client := startArbiter(t)
	v := coord.NewArbiterVoter(client)

	assert.Equal(t, coord.ArbiterName, v.Name())
	assert.Equal(t, coord.KindArbiter, v.Kind())

	_, err := v.Get(ctx, coord.KeyState)
	require.ErrorIs(t, err, coord.ErrUninitialized)

	rec, err := client.CAS(ctx, arbiter.RecordState, 0, []byte(`{"generation":1}`), true)
	require.NoError(t, err)

	got, err := v.Get(ctx, coord.KeyState)
	require.NoError(t, err)
	assert.Equal(t, coord.Entry{Value: []byte(`{"generation":1}`), Version: coord.Version(rec.Etag)}, got)

	_, err = v.Get(ctx, coord.KeyLease)
	require.ErrorIs(t, err, coord.ErrNotFound)

	err = v.CAS(ctx, coord.KeyState, []byte(`{"generation":2}`), got.Version+1)
	require.ErrorIs(t, err, coord.ErrVersionMismatch)

	require.NoError(t, v.CAS(ctx, coord.KeyState, []byte(`{"generation":2}`), got.Version))

	after, err := client.Get(ctx, arbiter.RecordState)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"generation":2}`), after.Value)
}

func TestArbiterVoterRefusesCreate(t *testing.T) {
	ctx := t.Context()
	client := startArbiter(t)
	v := coord.NewArbiterVoter(client)

	for _, key := range []coord.Key{coord.KeyState, coord.KeyLease} {
		err := v.CAS(ctx, key, []byte(`{"generation":1}`), 0)
		require.ErrorIs(t, err, coord.ErrCreateRefused, "key %s", key)
	}

	_, err := client.Get(ctx, arbiter.RecordState)
	require.ErrorIs(t, err, arbiter.ErrUninitialized, "the refused create reached the arbiter")
}

func TestNewVoters(t *testing.T) {
	warm := newConsulClients(t, config.ProfileWarmStandby,
		config.ConsulServer{Address: "10.0.0.2:8500", Datacenter: "dc_b"},
		config.ConsulServer{Address: "10.0.0.1:8500", Datacenter: "dc_a"},
	)
	stretch := newConsulClients(t, config.ProfileStretch,
		config.ConsulServer{Address: "10.0.0.1:8500", Datacenter: "dc_a"},
		config.ConsulServer{Address: "10.0.0.2:8500", Datacenter: "dc_b"},
	)
	arb := startArbiter(t)

	names := func(vs []coord.Voter) (out []string) {
		for _, v := range vs {
			out = append(out, v.Name()+"/"+string(v.Kind()))
		}

		return out
	}

	t.Run("warm_standby", func(t *testing.T) {
		vs, err := coord.NewVoters(config.ProfileWarmStandby, warm, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"dc_a/kv", "dc_b/kv"}, names(vs))
	})

	t.Run("warm_standby_arbiter", func(t *testing.T) {
		vs, err := coord.NewVoters(config.ProfileWarmStandby, warm, arb)
		require.NoError(t, err)
		assert.Equal(t, []string{"dc_a/kv", "dc_b/kv", "arbiter/arbiter"}, names(vs))
	})

	t.Run("stretch", func(t *testing.T) {
		vs, err := coord.NewVoters(config.ProfileStretch, stretch, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{coord.StretchName + "/kv"}, names(vs))
	})

	t.Run("stretch_arbiter", func(t *testing.T) {
		_, err := coord.NewVoters(config.ProfileStretch, stretch, arb)
		require.Error(t, err)
	})

	t.Run("no_consul", func(t *testing.T) {
		_, err := coord.NewVoters(config.ProfileWarmStandby, nil, nil)
		require.Error(t, err)
	})
}
