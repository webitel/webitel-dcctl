package consul

import (
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
)

func TestIntegrationServiceTags(t *testing.T) {
	ctx := t.Context()
	addr := startConsul(t)

	raw, err := api.NewClient(&api.Config{Address: addr, Scheme: "http"})
	require.NoError(t, err)

	for _, svc := range []*api.AgentService{
		{ID: "nomad-http", Service: "nomad", Port: 4646, Tags: []string{"http"}},
		{ID: "nomad-rpc", Service: "nomad", Port: 4647, Tags: []string{"rpc"}},
	} {
		_, err := raw.Catalog().Register(&api.CatalogRegistration{
			Node:     "node-a",
			Address:  "10.1.0.11",
			NodeMeta: map[string]string{"webitel_datacenter": "dc_a"},
			Service:  svc,
		}, nil)
		require.NoError(t, err)
	}

	c := integrationClients(t, config.ProfileStretch, addr)

	http, err := c.HealthyAddrs(ctx, "dc_a", "nomad", "http")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.1.0.11:4646"}, http)

	all, err := c.HealthyAddrs(ctx, "dc_a", "nomad", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"10.1.0.11:4646", "10.1.0.11:4647"}, all)
}

func TestIntegrationKVListDeleteCASAndGoneSession(t *testing.T) {
	ctx := t.Context()
	c := integrationClients(t, config.ProfileWarmStandby, startConsul(t)).Voters()[0]

	for _, k := range []string{"dcctl/journal/1", "dcctl/journal/2"} {
		ok, err := c.KVCAS(ctx, k, []byte("e"), 0)
		require.NoError(t, err)
		require.True(t, ok)
	}

	list, err := c.KVList(ctx, "dcctl/journal/")
	require.NoError(t, err)
	require.Len(t, list, 2)

	ok, err := c.KVDeleteCAS(ctx, list[0].Key, list[0].ModifyIndex+100)
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = c.KVDeleteCAS(ctx, list[0].Key, list[0].ModifyIndex)
	require.NoError(t, err)
	assert.True(t, ok)

	sid, err := c.CreateSession(ctx, "op")
	require.NoError(t, err)
	require.NoError(t, c.DestroySession(ctx, sid))

	_, err = c.Acquire(ctx, "dcctl/lock", []byte("me"), sid)
	require.ErrorIs(t, err, ErrSessionNotFound)

	// Consul 1.15 answers false, not an error, to a release of a key the gone session does not hold.
	ok, err = c.Release(ctx, "dcctl/lock", []byte("me"), sid)
	if err != nil {
		require.ErrorIs(t, err, ErrSessionNotFound)
	} else {
		assert.False(t, ok)
	}
}
