package consul

import (
	"context"
	"net"
	"strconv"

	"github.com/hashicorp/consul/api"
)

// ServiceNode is one instance of a service with the node it runs on.
type ServiceNode struct {
	ID      string
	Service string
	// Address is the address the instance registered; empty means the node's.
	Address string
	Port    int
	Tags    []string
	Meta    map[string]string

	Node        string
	NodeAddress string
	NodeMeta    map[string]string

	// Status is the aggregated status of the checks of the instance and its node:
	// passing, warning or critical.
	Status string
}

// HostPort returns host:port of the instance, falling back to the address of the node.
func (n ServiceNode) HostPort() string {
	host := n.Address
	if host == "" {
		host = n.NodeAddress
	}

	return net.JoinHostPort(host, strconv.Itoa(n.Port))
}

// Instances returns the instances of service with tag (any when empty) in the datacenter
// of the client. In stretch only those on nodes of the datacenter, by node_meta.
func (c *Client) Instances(ctx context.Context, service, tag string, passingOnly bool) ([]ServiceNode, QueryMeta, error) {
	return c.queryInstances(ctx, service, tag, passingOnly, c.nodeMeta)
}

// ClusterInstances is [Client.Instances] without the datacenter filter: in stretch it
// returns the instances of the whole cluster, each with the node_meta of its node.
func (c *Client) ClusterInstances(ctx context.Context, service, tag string, passingOnly bool) ([]ServiceNode, QueryMeta, error) {
	return c.queryInstances(ctx, service, tag, passingOnly, nil)
}

func (c *Client) queryInstances(
	ctx context.Context, service, tag string, passingOnly bool, nodeMeta map[string]string,
) ([]ServiceNode, QueryMeta, error) {
	var (
		out  []ServiceNode
		meta QueryMeta
	)

	endpoint, err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		opts := c.queryOptions(ctx)
		opts.NodeMeta = nodeMeta

		entries, qm, err := client.Health().Service(service, tag, passingOnly, opts)
		if err != nil {
			return err
		}

		out, meta = serviceNodes(entries), queryMeta(qm)

		return nil
	})
	if err != nil {
		return nil, QueryMeta{}, err
	}

	meta.Endpoint = endpoint

	return out, meta, nil
}

// ExecuteQuery executes the prepared query name, for example webitel-api. The nodes
// carry their node_meta. The query is not filtered by datacenter: in stretch it is one
// for the whole cluster.
func (c *Client) ExecuteQuery(ctx context.Context, name string) ([]ServiceNode, QueryMeta, error) {
	var (
		out  []ServiceNode
		meta QueryMeta
	)

	endpoint, err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		resp, qm, err := client.PreparedQuery().Execute(name, c.queryOptions(ctx))
		if err != nil {
			return err
		}

		entries := make([]*api.ServiceEntry, len(resp.Nodes))
		for i := range resp.Nodes {
			entries[i] = &resp.Nodes[i]
		}

		out, meta = serviceNodes(entries), queryMeta(qm)

		return nil
	})
	if err != nil {
		return nil, QueryMeta{}, err
	}

	meta.Endpoint = endpoint

	return out, meta, nil
}

// HealthyAddrs returns host:port of the passing instances of service with tag (any when
// empty) in datacenter dc. In stretch the query is filtered by node-meta
// <consul.node_meta_key>:<dc>.
func (c *Clients) HealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error) {
	return c.addrs(ctx, dc, service, tag, true)
}

// RegisteredAddrs returns host:port of every instance of service in datacenter dc,
// whatever the state of its checks. In stretch the query is filtered by node-meta
// <consul.node_meta_key>:<dc>.
func (c *Clients) RegisteredAddrs(ctx context.Context, dc, service string) ([]string, error) {
	return c.addrs(ctx, dc, service, "", false)
}

func (c *Clients) addrs(ctx context.Context, dc, service, tag string, passingOnly bool) ([]string, error) {
	cl, err := c.Client(dc)
	if err != nil {
		return nil, err
	}

	nodes, _, err := cl.Instances(ctx, service, tag, passingOnly)
	if err != nil {
		return nil, err
	}

	addrs := make([]string, len(nodes))
	for i, n := range nodes {
		addrs[i] = n.HostPort()
	}

	return addrs, nil
}

func serviceNodes(entries []*api.ServiceEntry) []ServiceNode {
	out := make([]ServiceNode, 0, len(entries))

	for _, e := range entries {
		n := ServiceNode{Status: e.Checks.AggregatedStatus()}

		if e.Service != nil {
			n.ID, n.Service, n.Address, n.Port = e.Service.ID, e.Service.Service, e.Service.Address, e.Service.Port
			n.Tags, n.Meta = e.Service.Tags, e.Service.Meta
		}

		if e.Node != nil {
			n.Node, n.NodeAddress, n.NodeMeta = e.Node.Node, e.Node.Address, e.Node.Meta
		}

		out = append(out, n)
	}

	return out
}
