package journal

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// Client is the Consul of one datacenter as the journal uses it;
// [*consul.Client] implements it.
type Client interface {
	KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error)
	KVList(ctx context.Context, prefix string) ([]consul.KVEntry, error)
	KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error)
	KVDeleteCAS(ctx context.Context, key string, modifyIndex uint64) (bool, error)
}

// KV is the Consul KV of one datacenter, or the global Consul in stretch.
type KV struct {
	Name   string
	Client Client
}

// KVs returns the journal KVs of the voter clients of [coord.NamedClients].
func KVs(clients []coord.NamedClient) []KV {
	out := make([]KV, 0, len(clients))
	for _, c := range clients {
		out = append(out, KV{Name: c.Name, Client: c.Client})
	}

	return out
}

// sorted returns kvs in name order.
func sorted(kvs []KV) ([]KV, error) {
	if len(kvs) == 0 {
		return nil, errors.New("no consul kv")
	}

	out := slices.Clone(kvs)
	slices.SortFunc(out, func(a, b KV) int { return strings.Compare(a.Name, b.Name) })

	return out, nil
}
