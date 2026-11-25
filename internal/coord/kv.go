package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// KVClient is the Consul of one voter as the lock and the operation record
// use it; [*consul.Client] implements it.
type KVClient interface {
	CreateSession(ctx context.Context, name string) (string, error)
	RenewSession(ctx context.Context, id string) error
	DestroySession(ctx context.Context, id string) error
	Acquire(ctx context.Context, key string, value []byte, session string) (bool, error)
	Release(ctx context.Context, key string, value []byte, session string) (bool, error)
	KVGet(ctx context.Context, key string) (*consul.KVEntry, consul.QueryMeta, error)
	KVCAS(ctx context.Context, key string, value []byte, modifyIndex uint64) (bool, error)
}

// KV is the Consul KV of one voter: of a datacenter in warm_standby, named
// after it, or the global Consul in stretch, named [StretchName].
type KV struct {
	Name   string
	Client KVClient
}

// NamedClient is the Consul client of one voter under the name of its KV.
type NamedClient struct {
	Name   string
	Client *consul.Client
}

// NamedClients returns the clients of the voters of profile in name order:
// of a datacenter in warm_standby, named after it, or the global Consul in
// stretch, named [StretchName]. They must not allow stale reads: the lock,
// the operation record and the journal decide on what they read.
func NamedClients(profile config.Profile, clients *consul.Clients) ([]NamedClient, error) {
	if clients == nil {
		return nil, errors.New("no consul clients")
	}

	voters := clients.Voters()

	for _, c := range voters {
		if c.AllowStale() {
			return nil, fmt.Errorf("consul client of %s allows stale reads", c.Datacenter())
		}
	}

	switch profile {
	case config.ProfileStretch:
		if len(voters) != 1 {
			return nil, fmt.Errorf("profile %s needs one consul voter, got %d", profile, len(voters))
		}

		return []NamedClient{{Name: StretchName, Client: voters[0]}}, nil
	case config.ProfileWarmStandby:
		out := make([]NamedClient, 0, len(voters))
		for _, c := range voters {
			out = append(out, NamedClient{Name: c.Datacenter(), Client: c})
		}

		slices.SortFunc(out, func(a, b NamedClient) int { return strings.Compare(a.Name, b.Name) })

		return out, nil
	}

	return nil, fmt.Errorf("unknown profile %q", profile)
}

// NewKVs returns the KVs of profile in name order, see [NamedClients].
func NewKVs(profile config.Profile, clients *consul.Clients) ([]KV, error) {
	named, err := NamedClients(profile, clients)
	if err != nil {
		return nil, err
	}

	kvs := make([]KV, 0, len(named))
	for _, n := range named {
		kvs = append(kvs, KV{Name: n.Name, Client: n.Client})
	}

	return kvs, nil
}

// sortedKVs checks kvs and returns them sorted by name.
func sortedKVs(kvs []KV) ([]KV, error) {
	if len(kvs) == 0 {
		return nil, errors.New("no consul kv")
	}

	out := slices.Clone(kvs)
	slices.SortFunc(out, func(a, b KV) int { return strings.Compare(a.Name, b.Name) })

	for i, s := range out {
		switch {
		case s.Name == "":
			return nil, errors.New("consul kv without a name")
		case s.Client == nil:
			return nil, fmt.Errorf("consul kv %s has no client", s.Name)
		case i > 0 && out[i-1].Name == s.Name:
			return nil, fmt.Errorf("duplicate consul kv %q", s.Name)
		}
	}

	return out, nil
}
