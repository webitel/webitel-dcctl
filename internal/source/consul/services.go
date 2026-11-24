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
func (c *Client) Instances(ctx context.Context, service, tag string, passingOnly bool) ([]ServiceNode, error) {
	var out []ServiceNode

	err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		opts := new(api.QueryOptions).WithContext(ctx)
		opts.NodeMeta = c.nodeMeta

		entries, _, err := client.Health().Service(service, tag, passingOnly, opts)
		if err != nil {
			return err
		}

		out = serviceNodes(entries)

		return nil
	})

	return out, err
}

// ExecuteQuery executes the prepared query name, for example webitel-api. The nodes
// carry their node_meta. The query is not filtered by datacenter: in stretch it is one
// for the whole cluster.
func (c *Client) ExecuteQuery(ctx context.Context, name string) ([]ServiceNode, error) {
	var out []ServiceNode

	err := c.pool.do(ctx, true, func(ctx context.Context, client *api.Client) error {
		resp, _, err := client.PreparedQuery().Execute(name, new(api.QueryOptions).WithContext(ctx))
		if err != nil {
			return err
		}

		entries := make([]*api.ServiceEntry, len(resp.Nodes))
		for i := range resp.Nodes {
			entries[i] = &resp.Nodes[i]
		}

		out = serviceNodes(entries)

		return nil
	})

	return out, err
}

// HealthyAddrs returns host:port of the passing instances of service with tag (any when
// empty) in datacenter dc. In stretch the query is filtered by node-meta
// <consul.node_meta_key>:<dc>.
func (c *Clients) HealthyAddrs(ctx context.Context, dc, service, tag string) ([]string, error) {
	cl, err := c.Client(dc)
	if err != nil {
		return nil, err
	}

	nodes, err := cl.Instances(ctx, service, tag, true)
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
