// Package coord keeps every dcctl instance agreeing on the active
// datacenter: it reads dcctl/state from the voters, tells which record is in
// effect and aligns voters that lag behind. It also holds what keeps two
// operations from running at once: dcctl/lock and the operation record
// dcctl/op in the Consul KV.
package coord
