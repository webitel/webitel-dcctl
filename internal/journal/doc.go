// Package journal keeps the journal: a record of every operation, step,
// system action and alarm, in the Consul KV of every reachable datacenter
// and in the service log. It is what support reads to reconstruct an
// outage.
//
// An entry is the protojson of webitel.dcctl.v1.JournalEntry under
// `dcctl/journal/<time>-<id>`. `<time>` is the create_time in UTC as 23
// digits, so keys sort in creation order. `<id>` is 8 lowercase hex
// characters from crypto/rand: instances need no coordination to pick
// distinct keys.
//
// [Journal.Append] writes an entry, [Journal.Align] copies the entries a
// datacenter misses, [Journal.Trim] keeps journal.max_entries per
// datacenter, and [Journal.List] reads for ListJournalEntries. [Journal.Run]
// aligns and trims periodically.
package journal
