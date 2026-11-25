package status

import (
	"fmt"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// CheckID is the stable identifier of a check.
type CheckID string

// Informational checks. They affect neither state, health, alarms nor preflight.
const (
	// CheckInfoAPI is the prepared query webitel-api that nginx of the datacenter
	// resolves /api with. In stretch the query is one for the cluster, so every
	// datacenter gets the same result.
	CheckInfoAPI CheckID = "info.api"
	// CheckInfoServices is status.info_services in the datacenter.
	CheckInfoServices CheckID = "info.services"
	// CheckInfoRabbitMQ is status.rabbitmq_service for the datacenter.
	CheckInfoRabbitMQ CheckID = "info.rabbitmq"
)

// CheckResult is the result of a check.
type CheckResult string

const (
	CheckPass CheckResult = "pass"
	// CheckFail is a condition that does not hold, or that cannot be told because its
	// source does not answer.
	CheckFail CheckResult = "fail"
)

// Check is the result of one check.
type Check struct {
	ID         CheckID
	Result     CheckResult
	Datacenter string
	Detail     string
	// Facts are the data the result is based on. They hold only what
	// google.protobuf.Struct takes: strings, booleans, ints and maps of them.
	Facts map[string]any
}

const apiQuery = "webitel-api"

func (b *builder) infoChecks(d collect.Datacenter) []Check {
	return []Check{b.apiCheck(d.Name), b.servicesCheck(d), b.rabbitMQCheck(d)}
}

// unreachableCheck fails a check whose source does not answer: its last data is not
// evidence of anything now.
func unreachableCheck[T any](id CheckID, dc, what string, s collect.Source[T]) Check {
	return Check{
		ID: id, Result: CheckFail, Datacenter: dc,
		Detail: fmt.Sprintf("%s cannot be read: %s", what, s.Reason),
		Facts:  map[string]any{"reachable": false, "reason": string(s.Reason)},
	}
}

func (b *builder) apiCheck(dc string) Check {
	cl, ok := b.snap.ClusterOf(dc)
	if !ok {
		return Check{ID: CheckInfoAPI, Result: CheckFail, Datacenter: dc, Detail: "no Patroni cluster spans the datacenter"}
	}

	if !cl.API.Reachable {
		return unreachableCheck(CheckInfoAPI, dc, "prepared query "+apiQuery, cl.API)
	}

	counts := make(map[string]int)

	for _, n := range cl.API.Data.Nodes {
		if at := n.NodeMeta[b.cfg.NodeMetaKey]; at != "" {
			counts[at]++
		}
	}

	byDC := make(map[string]any, len(counts))
	for at, n := range counts {
		byDC[at] = n
	}

	count := len(cl.API.Data.Nodes)
	c := Check{
		ID: CheckInfoAPI, Result: CheckPass, Datacenter: dc,
		Detail: fmt.Sprintf("prepared query %s gives %d %s", apiQuery, count, plural(count, "instance")),
		Facts:  map[string]any{"count": count, "datacenters": byDC},
	}

	if count == 0 {
		c.Result = CheckFail
	}

	return c
}

func (b *builder) servicesCheck(d collect.Datacenter) Check {
	if !d.Services.Reachable {
		return unreachableCheck(CheckInfoServices, d.Name, "consul catalog", d.Services)
	}

	c := Check{
		ID: CheckInfoServices, Result: CheckPass, Datacenter: d.Name, Detail: "every service has a passing instance",
	}
	counts := make(map[string]any, len(b.cfg.InfoServices))

	for _, name := range b.cfg.InfoServices {
		passing, total := b.count(d.Name, d.Services.Data.Info[name])
		counts[name] = map[string]any{"passing": passing, "total": total}

		if passing == 0 && c.Result == CheckPass {
			c.Result, c.Detail = CheckFail, "no passing instance of "+name
		}
	}

	c.Facts = map[string]any{"services": counts}

	return c
}

func (b *builder) rabbitMQCheck(d collect.Datacenter) Check {
	if !d.Services.Reachable {
		return unreachableCheck(CheckInfoRabbitMQ, d.Name, "consul catalog", d.Services)
	}

	name := config.Status{RabbitMQService: b.cfg.RabbitMQService}.RabbitMQServiceFor(d.Name)
	passing, total := b.count(d.Name, d.Services.Data.RabbitMQ)

	c := Check{
		ID: CheckInfoRabbitMQ, Result: CheckPass, Datacenter: d.Name,
		Detail: fmt.Sprintf("service %s has %d passing %s of %d", name, passing, plural(passing, "instance"), total),
		Facts:  map[string]any{"service": name, "passing": passing, "total": total},
	}

	if passing == 0 {
		c.Result = CheckFail
	}

	return c
}

// count counts the instances of a service in dc and the passing ones. In stretch the
// one Consul holds the instances of every datacenter, so only those on nodes of dc
// count. The collector reads them filtered already; this guards against a snapshot
// that is not.
func (b *builder) count(dc string, nodes []consul.ServiceNode) (passing, total int) {
	for _, n := range nodes {
		if !b.warm() && n.NodeMeta[b.cfg.NodeMetaKey] != dc {
			continue
		}

		total++

		if n.Status == "passing" {
			passing++
		}
	}

	return passing, total
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}

	return word + "s"
}
