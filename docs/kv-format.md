# dcctl keys in Consul KV

dcctl keeps its coordination data under the `dcctl/` prefix in the Consul KV of every datacenter
(`warm_standby`) or in the global Consul (`stretch`). The Ansible role `topology` reads `dcctl/state`
and `dcctl/op` from there to find the active datacenter. Changing the format of these two keys
requires a change in the role.

## Reading a key

```sh
curl -s -o state.json -w '%{http_code}' "http://<consul_server>:8500/v1/kv/dcctl/state?raw"
```

- `200`: the body is the JSON value of the key.
- `404`: the key does not exist.
- Any other status or a connection error: the datacenter is unreachable (step 6 of
  [How `topology` picks the active datacenter](#how-topology-picks-the-active-datacenter)), not a
  missing key. A Consul without a leader returns `500`.

Without `?raw`, Consul returns its envelope with the value in base64 in `Value`.

Readers must ignore fields they do not know: a newer dcctl may add fields, and an older reader must
keep working.

## Keys

| Key | Value |
|---|---|
| `dcctl/state` | JSON, see [`dcctl/state`](#dcctlstate) |
| `dcctl/op` | protojson of `webitel.dcctl.v1.Operation`, see [`dcctl/op`](#dcctlop) |
| `dcctl/lease` | JSON, see [`dcctl/lease`](#dcctllease) |
| `dcctl/lock` | Consul session lock holding JSON, see [`dcctl/lock`](#dcctllock) |
| `dcctl/journal/<time>-<id>` | protojson of `webitel.dcctl.v1.JournalEntry`, one entry per key |

The arbiter stores the same JSON as `dcctl/state` and `dcctl/lease` in its records `records/state`
and `records/lease`.

## `dcctl/state`

```json
{
  "generation": 8,
  "revision": 3,
  "active_datacenter": "dc_b",
  "op_id": "4f0c9a52-7d1e-4b8a-9f3c-2a6e5d7b1c08",
  "forced": true,
  "patroni_timeline": 5,
  "needs_recovery": ["dc_a"],
  "updated_at": "2026-11-23T15:30:00Z"
}
```

| Field | Type | Meaning |
|---|---|---|
| `generation` | integer | Number of the active datacenter change. |
| `revision` | integer | Number of the write within one `generation`, see [Writes within a generation](#writes-within-a-generation). Missing means `0`. |
| `active_datacenter` | string | Consul name of the active datacenter. |
| `op_id` | string | ID of the operation that wrote this `generation` (the `{id}` in `operations/{id}`). |
| `forced` | bool | `true` if the change was written without a majority of voters. |
| `patroni_timeline` | integer | Patroni timeline of the active datacenter leader after the change. |
| `needs_recovery` | array of strings | Datacenters waiting for recovery after a failover. Missing means empty. |
| `updated_at` | string, RFC 3339 | Time of the last write. For display only. |

The role uses `active_datacenter` and `generation`; it does not read `revision`. `dcctl/state` is
first written only by `webitel-dcctl init`. A datacenter whose Consul lost the key gets it back as a
copy of the effective state from the other voters, once that state is on a majority of voters or
forced; if no voter has it, dcctl does not recreate it.

### Writes within a generation

A change of the active datacenter writes the next `generation` with `revision` `0`. Later writes
of the same `generation` (`patroni_timeline`, `needs_recovery`, `forced`) set `revision` to the
revision they replace plus one, and keep `forced`, `op_id` and the other fields they do not change.
Such a write reaches the voters one by one, so for a while voters hold the same `generation` and
`active_datacenter` with different revisions: the highest revision is the current one. A record
written before `revision` existed has none and counts as `0`.

`needs_recovery` is sorted and has no duplicates. A dcctl that rewrites the record keeps the fields
it does not know.

## `dcctl/op`

The last operation, as protojson of `webitel.dcctl.v1.Operation` from `proto/webitel/dcctl/v1/`. The
field list is the proto message.

- Field names are proto names (`request_id`, `new_generation`).
- Enums are value names (`"RUNNING"`).
- `int64` fields are JSON strings (`"new_generation": "8"`).
- Fields with a default value (zero, empty string, empty list) may be missing.

The role reads only `state` and continues only if it is one of the finished states:

| `state` | Meaning for Ansible |
|---|---|
| `SUCCEEDED`, `FAILED`, `ABANDONED` | Finished. Continue. |
| any other value, or `state` missing | Fail. Covers `RUNNING`, `INTERRUPTED` and states a newer dcctl may add. |

A missing `dcctl/op` means no operation in that datacenter; continue.

## `dcctl/lease`

```json
{
  "holder": "dc_b",
  "generation": 8,
  "counter": 1042,
  "fencing": false
}
```

| Field | Type | Meaning |
|---|---|---|
| `holder` | string | Datacenter that renews the lease, normally the active one. |
| `generation` | integer | `generation` of `dcctl/state` the lease was taken for. |
| `counter` | integer | Grows by one on every renewal. |
| `fencing` | bool | `true` if the holder has fencing rules enabled. |

The role does not read the lease.

## `dcctl/lock`

A Consul session lock that dcctl takes for the time of an operation: in every reachable datacenter
in `warm_standby`, once in the global Consul in `stretch`. The `Session` of the key is the holding
session; the value tells who holds it:

```json
{
  "instance": "dcctl-b1",
  "datacenter": "dc_b",
  "operation": "4f0c9a52-7d1e-4b8a-9f3c-2a6e5d7b1c08"
}
```

| Field | Type | Meaning |
|---|---|---|
| `instance` | string | Consul node name of the dcctl instance that holds the lock. |
| `datacenter` | string | Datacenter of that instance. |
| `operation` | string | ID of the operation the lock is held for (the `{id}` in `operations/{id}`). |

The lock is held only while the key has a `Session`. A released lock keeps the value of its last
holder. The role does not read the lock.

## `dcctl/journal/<time>-<id>`

`<time>` is the entry creation time in UTC as 23 digits: `yyyymmddhhmmss` and nanoseconds, for
example `20261123153000000000005`. Keys sort in creation order. `<time>-<id>` is also the ID in the
entry name `journalEntries/<time>-<id>`. `<id>` is 8 lowercase hex characters, random per entry.

An entry is written create-only to every reachable datacenter, and copied to the ones that miss it
under the same key. At most `journal.max_entries` entries are kept per datacenter; the oldest are
deleted.

The role does not read the journal.

## How `topology` picks the active datacenter

1. Read `dcctl/state` and `dcctl/op` from Consul of every datacenter.
2. Any reachable `dcctl/op` has a `state` other than `SUCCEEDED`, `FAILED` or `ABANDONED`, or no
   `state`: fail.
3. Reachable `dcctl/state` values differ in `generation` or `active_datacenter`: fail.
4. At least one `dcctl/state` exists: `active_datacenter` is its `active_datacenter`.
5. No `dcctl/state` anywhere and every datacenter was read: `active_datacenter` is
   `primary_datacenter`, fact `dcctl_state_present: false`.
6. No `dcctl/state` in reachable datacenters and some datacenters are unreachable: fail.

The role does not evaluate which record is valid (T4 §4.2): differing values fail at step 3.
