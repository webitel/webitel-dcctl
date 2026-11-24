package consul

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applyThenHang applies the write and never answers, like a server that loses the response.
func applyThenHang(applied *atomic.Int32) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		applied.Add(1)
		<-r.Context().Done()
	}
}

func TestWritesAreNotRetriedAfterSend(t *testing.T) {
	ctx := t.Context()

	writes := map[string]func(*Client) error{
		"kv_cas": func(c *Client) error {
			_, err := c.KVCAS(ctx, "k", []byte("v"), 1)

			return err
		},
		"kv_delete_cas": func(c *Client) error {
			_, err := c.KVDeleteCAS(ctx, "k", 1)

			return err
		},
		"release": func(c *Client) error {
			_, err := c.Release(ctx, "k", nil, "sid")

			return err
		},
	}

	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			var applied atomic.Int32

			first := newFake(t, applyThenHang(&applied))
			second := newFake(t, respond(http.StatusOK, "true"))
			c := newTestClient(t, first.addr(), second.addr())

			err := write(c)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			assert.Equal(t, int32(1), applied.Load())
			assert.Zero(t, second.hits.Load())
		})
	}

	t.Run("5xx after send is unknown too", func(t *testing.T) {
		first := newFake(t, respond(http.StatusInternalServerError, "rpc error"))
		second := newFake(t, respond(http.StatusOK, "true"))
		c := newTestClient(t, first.addr(), second.addr())

		_, err := c.KVCAS(ctx, "k", nil, 1)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		assert.Zero(t, second.hits.Load())
	})

	t.Run("connection refused is retried because nothing was sent", func(t *testing.T) {
		dead := newFake(t, respond(http.StatusOK, ""))
		dead.Close()

		ok := newFake(t, respond(http.StatusOK, "true"))
		c := newTestClient(t, dead.addr(), ok.addr())

		done, err := c.KVCAS(ctx, "k", nil, 1)
		require.NoError(t, err)
		assert.True(t, done)
	})

	t.Run("acquire with the same session is retried", func(t *testing.T) {
		var applied atomic.Int32

		first := newFake(t, applyThenHang(&applied))
		second := newFake(t, respond(http.StatusOK, "true"))
		c := newTestClient(t, first.addr(), second.addr())

		ok, err := c.Acquire(ctx, "k", nil, "sid")
		require.NoError(t, err)
		assert.True(t, ok)
	})
}

func TestInvalidSession(t *testing.T) {
	ctx := t.Context()
	body := `rpc error making call: raft apply failed: invalid session "sid"`

	calls := map[string]func(*Client) error{
		"acquire": func(c *Client) error {
			_, err := c.Acquire(ctx, "k", nil, "sid")

			return err
		},
		"release": func(c *Client) error {
			_, err := c.Release(ctx, "k", nil, "sid")

			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			first := newFake(t, respond(http.StatusInternalServerError, body))
			second := newFake(t, respond(http.StatusOK, "true"))
			c := newTestClient(t, first.addr(), second.addr())

			err := call(c)
			require.ErrorIs(t, err, ErrSessionNotFound)
			assert.NotErrorIs(t, err, ErrOutcomeUnknown)
			assert.Zero(t, second.hits.Load())
		})
	}
}

func TestTagKVListDeleteCAS(t *testing.T) {
	ctx := t.Context()

	t.Run("tag is sent", func(t *testing.T) {
		var tag string

		srv := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			tag = r.URL.Query().Get("tag")
			_, _ = w.Write([]byte(healthBody))
		})
		c := newTestClient(t, srv.addr())

		_, _, err := c.Instances(ctx, "web", "http", false)
		require.NoError(t, err)
		assert.Equal(t, "http", tag)
	})

	t.Run("list", func(t *testing.T) {
		var query string

		srv := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.RawQuery
			_, _ = w.Write([]byte(`[{"Key":"p/a","Value":"YQ==","ModifyIndex":3},{"Key":"p/b","ModifyIndex":4}]`))
		})
		c := newTestClient(t, srv.addr())

		got, err := c.KVList(ctx, "p/")
		require.NoError(t, err)
		assert.Equal(t, "recurse=", query)
		assert.Equal(t, []KVEntry{{Key: "p/a", Value: []byte("a"), ModifyIndex: 3}, {Key: "p/b", ModifyIndex: 4}}, got)
	})

	t.Run("list of nothing", func(t *testing.T) {
		srv := newFake(t, respond(http.StatusNotFound, ""))
		c := newTestClient(t, srv.addr())

		got, err := c.KVList(ctx, "p/")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("delete cas sends the index", func(t *testing.T) {
		var method, cas string

		srv := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			method, cas = r.Method, r.URL.Query().Get("cas")
			_, _ = w.Write([]byte("true"))
		})
		c := newTestClient(t, srv.addr())

		ok, err := c.KVDeleteCAS(ctx, "k", 9)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, http.MethodDelete, method)
		assert.Equal(t, "9", cas)
	})
}
