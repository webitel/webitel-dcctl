// Package kv defines the keys and records dcctl keeps in Consul KV and in the arbiter.
//
// The format is a contract with the Ansible role topology, described in docs/kv-format.md.
// Records are read with encoding/json defaults: unknown fields are ignored, so an older
// dcctl keeps reading records written by a newer one; a rewritten [State] keeps them
// through [EncodeState]. Proto records go through [EncodeProto] and [DecodeProto].
package kv

import (
	"fmt"
	"time"
)

// Keys in Consul KV of every voter datacenter.
const (
	StateKey = "dcctl/state"
	LeaseKey = "dcctl/lease"
	LockKey  = "dcctl/lock"
	// OpKey holds the protojson of webitel.dcctl.v1.Operation.
	OpKey = "dcctl/op"
	// JournalPrefix holds one protojson webitel.dcctl.v1.JournalEntry per key, see [JournalKey].
	JournalPrefix = "dcctl/journal/"
)

// Names of the arbiter records. A record holds the same JSON as the KV key.
const (
	StateRecord = "records/state"
	LeaseRecord = "records/lease"
)

// JournalKey returns the key of a journal entry: JournalPrefix + "<time>-<id>".
// The time is createTime in UTC as 23 digits (yyyymmddhhmmss and nanoseconds),
// so keys sort in creation order.
func JournalKey(createTime time.Time, id string) string {
	t := createTime.UTC()

	return fmt.Sprintf("%s%s%09d-%s", JournalPrefix, t.Format("20060102150405"), t.Nanosecond(), id)
}
