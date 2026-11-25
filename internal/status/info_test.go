package status_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func check(t *testing.T, d status.Datacenter, id status.CheckID) status.Check {
	t.Helper()

	for _, c := range d.InfoChecks {
		if c.ID == id {
			return c
		}
	}

	t.Fatalf("no check %s in %s", id, d.Name)

	return status.Check{}
}

func TestBuild_infoChecks(t *testing.T) {
	d := datacenter(t, status.Build(warm(), testConfig(), now), dcB)

	assert.Equal(t, status.Check{
		ID: status.CheckInfoAPI, Result: status.CheckPass, Datacenter: dcB,
		Detail: "prepared query webitel-api gives 1 instance",
		Facts:  map[string]any{"count": 1, "datacenters": map[string]any{dcB: 1}},
	}, check(t, d, status.CheckInfoAPI))

	assert.Equal(t, status.Check{
		ID: status.CheckInfoServices, Result: status.CheckPass, Datacenter: dcB,
		Detail: "every service has a passing instance",
		Facts: map[string]any{"services": map[string]any{
			"engine":   map[string]any{"passing": 1, "total": 1},
			"opensips": map[string]any{"passing": 1, "total": 2},
		}},
	}, check(t, d, status.CheckInfoServices))

	assert.Equal(t, status.Check{
		ID: status.CheckInfoRabbitMQ, Result: status.CheckPass, Datacenter: dcB,
		Detail: "service rabbitmq has 1 passing instance of 1",
		Facts:  map[string]any{"service": "rabbitmq", "passing": 1, "total": 1},
	}, check(t, d, status.CheckInfoRabbitMQ))

	for _, c := range d.InfoChecks {
		_, err := structpb.NewStruct(c.Facts)
		require.NoError(t, err, "facts of %s go into google.protobuf.Struct", c.ID)
	}
}

func TestBuild_infoChecksFail(t *testing.T) {
	t.Run("no_passing_instance", func(t *testing.T) {
		s := warm()
		svc := services(dcB)
		svc.RabbitMQ = []consul.ServiceNode{instance("rabbitmq", dcB, false)}
		svc.Info["engine"] = nil
		dcOf(s, dcB).Services = up(svc)
		s.Clusters[1].API = up(collect.Instances{})

		d := datacenter(t, status.Build(s, testConfig(), now), dcB)
		assert.Equal(t, status.CheckFail, check(t, d, status.CheckInfoRabbitMQ).Result)
		assert.Equal(t, status.CheckFail, check(t, d, status.CheckInfoServices).Result)
		assert.Equal(t, "no passing instance of engine", check(t, d, status.CheckInfoServices).Detail)
		assert.Equal(t, status.CheckFail, check(t, d, status.CheckInfoAPI).Result)
	})

	t.Run("source_unreachable", func(t *testing.T) {
		s := warm()
		dcOf(s, dcB).Services = unreachable(dcOf(s, dcB).Services)
		s.Clusters[1].API = unreachable(s.Clusters[1].API)

		d := datacenter(t, status.Build(s, testConfig(), now), dcB)
		for _, c := range d.InfoChecks {
			assert.Equal(t, status.CheckFail, c.Result, c.ID)
			assert.Equal(t, map[string]any{"reachable": false, "reason": "refused"}, c.Facts, c.ID)
		}
	})
}

// In stretch one Consul holds the services of every datacenter, so the checks of a
// datacenter count only the instances on its nodes.
func TestBuild_infoChecksStretchFilter(t *testing.T) {
	s := stretch()
	cfg := testConfig()
	cfg.RabbitMQService = "rabbitmq-{datacenter}"

	svc := services(dcB)
	svc.RabbitMQ = []consul.ServiceNode{instance("rabbitmq-dc_b", dcA, true), instance("rabbitmq-dc_b", dcB, false)}
	svc.Info["engine"] = []consul.ServiceNode{instance("engine", dcA, true), instance("engine", dcB, false)}
	dcOf(s, dcB).Services = up(svc)

	d := datacenter(t, status.Build(s, cfg, now), dcB)

	rabbit := check(t, d, status.CheckInfoRabbitMQ)
	assert.Equal(t, status.CheckFail, rabbit.Result)
	assert.Equal(t, map[string]any{"service": "rabbitmq-dc_b", "passing": 0, "total": 1}, rabbit.Facts)

	info := check(t, d, status.CheckInfoServices)
	assert.Equal(t, status.CheckFail, info.Result)
	assert.Equal(t, map[string]any{"passing": 0, "total": 1}, info.Facts["services"].(map[string]any)["engine"])

	api := check(t, d, status.CheckInfoAPI)
	assert.Equal(t, status.CheckPass, api.Result, "the query is one for the cluster")
	assert.Equal(t, map[string]any{"count": 2, "datacenters": map[string]any{dcA: 2}}, api.Facts)
}

func TestBuild_sources(t *testing.T) {
	s := warm()
	dcOf(s, dcB).Nomad = unreachable(dcOf(s, dcB).Nomad)

	d := datacenter(t, status.Build(s, testConfig(), now), dcB)
	assert.Equal(t, []status.Source{
		{System: status.SystemConsul, Reachable: true, ObserveTime: observed},
		{System: status.SystemNomad, Reason: "refused", UnreachableSince: lost, ObserveTime: lost},
		{System: status.SystemPatroni, Reachable: true, ObserveTime: observed},
	}, d.Sources)
}
