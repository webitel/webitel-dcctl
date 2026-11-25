package status

import (
	"fmt"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

const apiQuery = "webitel-api"

func (b *builder) infoChecks(d collect.Datacenter) []check.Check {
	return []check.Check{b.apiCheck(d.Name), b.servicesCheck(d), b.rabbitMQCheck(d)}
}

// unreachableCheck fails a check whose source does not answer: its last data is not
// evidence of anything now.
func unreachableCheck[T any](id check.ID, dc, what string, s collect.Source[T]) check.Check {
	return check.Check{
		ID: id, Result: check.Fail, Datacenter: dc,
		Detail: fmt.Sprintf("%s cannot be read: %s", what, s.Reason),
		Facts:  map[string]any{"reachable": false, "reason": string(s.Reason)},
	}
}

func (b *builder) apiCheck(dc string) check.Check {
	cl, ok := b.snap.ClusterOf(dc)
	if !ok {
		return check.Check{ID: check.InfoAPI, Result: check.Fail, Datacenter: dc, Detail: "no Patroni cluster spans the datacenter"}
	}

	if !cl.API.Reachable {
		return unreachableCheck(check.InfoAPI, dc, "prepared query "+apiQuery, cl.API)
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
	c := check.Check{
		ID: check.InfoAPI, Result: check.Pass, Datacenter: dc,
		Detail: fmt.Sprintf("prepared query %s gives %d %s", apiQuery, count, plural(count, "instance")),
		Facts:  map[string]any{"count": count, "datacenters": byDC},
	}

	if count == 0 {
		c.Result = check.Fail
	}

	return c
}

func (b *builder) servicesCheck(d collect.Datacenter) check.Check {
	if !d.Services.Reachable {
		return unreachableCheck(check.InfoServices, d.Name, "consul catalog", d.Services)
	}

	c := check.Check{
		ID: check.InfoServices, Result: check.Pass, Datacenter: d.Name, Detail: "every service has a passing instance",
	}
	counts := make(map[string]any, len(b.cfg.InfoServices))

	for _, name := range b.cfg.InfoServices {
		passing, total := b.count(d.Name, d.Services.Data.Info[name])
		counts[name] = map[string]any{"passing": passing, "total": total}

		if passing == 0 && c.Result == check.Pass {
			c.Result, c.Detail = check.Fail, "no passing instance of "+name
		}
	}

	c.Facts = map[string]any{"services": counts}

	return c
}

func (b *builder) rabbitMQCheck(d collect.Datacenter) check.Check {
	if !d.Services.Reachable {
		return unreachableCheck(check.InfoRabbitMQ, d.Name, "consul catalog", d.Services)
	}

	name := config.Status{RabbitMQService: b.cfg.RabbitMQService}.RabbitMQServiceFor(d.Name)
	passing, total := b.count(d.Name, d.Services.Data.RabbitMQ)

	c := check.Check{
		ID: check.InfoRabbitMQ, Result: check.Pass, Datacenter: d.Name,
		Detail: fmt.Sprintf("service %s has %d passing %s of %d", name, passing, plural(passing, "instance"), total),
		Facts:  map[string]any{"service": name, "passing": passing, "total": total},
	}

	if passing == 0 {
		c.Result = check.Fail
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
