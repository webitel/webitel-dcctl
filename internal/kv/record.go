package kv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// State is the value of [StateKey] and of the arbiter record [StateRecord].
type State struct {
	// Generation is the monotonic number of the active datacenter change.
	Generation int64 `json:"generation"`
	// Revision orders the writes within one generation: the write that
	// creates the generation has 0, and every later write of the same
	// generation has the revision it replaces plus one. A record without
	// the field has 0.
	Revision         int64  `json:"revision"`
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

// stateFields are the JSON names of the fields of [State].
var stateFields = jsonNames(reflect.TypeFor[State]())

func jsonNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())

	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out = append(out, name)
	}

	return out
}

// stateField reports whether encoding/json reads name into a field of
// [State]: it matches names without case.
func stateField(name string) bool {
	return slices.ContainsFunc(stateFields, func(f string) bool { return strings.EqualFold(f, name) })
}

// EncodeState returns the JSON of s followed by the fields of prev, the
// record s replaces, that [State] does not know, so that rewriting a record
// keeps what a newer dcctl added. A field of State is always taken from s,
// also when s leaves it out as empty, and a field of prev whose name differs
// from one of State only in case is dropped, as it would be read into it.
func EncodeState(s State, prev []byte) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", StateKey, err)
	}

	if len(prev) == 0 {
		return b, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(prev, &fields); err != nil {
		return nil, fmt.Errorf("previous %s: %w", StateKey, err)
	}

	if fields == nil {
		return nil, fmt.Errorf("previous %s is not a JSON object", StateKey)
	}

	var unknown []string

	for name := range fields {
		if !stateField(name) {
			unknown = append(unknown, name)
		}
	}

	if len(unknown) == 0 {
		return b, nil
	}

	slices.Sort(unknown)

	var buf bytes.Buffer

	buf.Write(b[:len(b)-1])

	for _, name := range unknown {
		key, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("previous %s field %q: %w", StateKey, name, err)
		}

		buf.WriteByte(',')
		buf.Write(key)
		buf.WriteByte(':')

		if err := json.Compact(&buf, fields[name]); err != nil {
			return nil, fmt.Errorf("previous %s field %q: %w", StateKey, name, err)
		}
	}

	buf.WriteByte('}')

	return buf.Bytes(), nil
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
