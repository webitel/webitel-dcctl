package kv

import "time"

// State is the value of [StateKey] and of the arbiter record [StateRecord].
type State struct {
	// Generation is the monotonic number of the active datacenter change.
	Generation       int64  `json:"generation"`
	ActiveDatacenter string `json:"active_datacenter"`
	// OpID is the ID of the operation that wrote this generation, as in operations/{id}.
	OpID string `json:"op_id"`
	// Forced is set when the generation was written without a majority of voters.
	Forced          bool  `json:"forced"`
	PatroniTimeline int64 `json:"patroni_timeline"`
	// NeedsRecovery lists datacenters waiting for recovery after a failover.
	NeedsRecovery []string `json:"needs_recovery,omitempty"`
	// UpdatedAt is for display only; no decision is based on it.
	UpdatedAt time.Time `json:"updated_at"`
}

// Lease is the value of [LeaseKey] and of the arbiter record [LeaseRecord].
type Lease struct {
	// Holder is the datacenter that renews the lease.
	Holder string `json:"holder"`
	// Generation equals [State.Generation] of the holder.
	Generation int64 `json:"generation"`
	// Counter grows by one on every renewal.
	Counter int64 `json:"counter"`
	// Fencing is set when the holder has fencing rules enabled.
	Fencing bool `json:"fencing"`
}
