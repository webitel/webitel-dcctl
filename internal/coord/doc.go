// Package coord keeps every dcctl instance agreeing on the active
// datacenter: it reads dcctl/state from the voters, tells which record is in
// effect, writes the next generation and the updates within one, and aligns
// voters that lag behind. It also holds what keeps two
// operations from running at once: dcctl/lock and the operation record
// dcctl/op in the Consul KV. The active datacenter renews dcctl/lease on
// the voters, and every instance evaluates whether that lease is held.
package coord
