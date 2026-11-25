package kv

// Lock is the value of [LockKey]: who holds the lock.
type Lock struct {
	// Instance is the Consul node name of the dcctl instance that holds the lock.
	Instance string `json:"instance"`
	// Datacenter is the datacenter of that instance.
	Datacenter string `json:"datacenter"`
	// Operation is the ID of the operation the lock is held for, as in operations/{id}.
	Operation string `json:"operation"`
}
