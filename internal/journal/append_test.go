package journal_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

var nameRE = regexp.MustCompile(`^journalEntries/\d{23}-[0-9a-f]{8}$`)

func TestAppend_writesEveryDatacenter(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	j, err := journal.New([]journal.KV{kvOf("dc_b", b), kvOf("dc_a", a)},
		journal.Options{MaxEntries: 10, Timeout: testTimeout, Now: func() time.Time { return at(5) }}, quietLog())
	require.NoError(t, err)

	got, err := j.Append(t.Context(), operationEntry())
	require.NoError(t, err)

	assert.Regexp(t, nameRE, got.Entry.GetName())
	assert.True(t, got.Entry.GetCreateTime().AsTime().Equal(at(5)))
	assert.Equal(t, 2, got.Stored())
	assert.Equal(t, []journal.Write{{DC: "dc_a", Outcome: journal.Written}, {DC: "dc_b", Outcome: journal.Written}}, got.Writes)

	key := kv.JournalPrefix + strings.TrimPrefix(got.Entry.GetName(), "journalEntries/")
	assert.Equal(t, kv.JournalKey(at(5), key[len(key)-8:]), key)

	for _, f := range []*fakeConsul{a, b} {
		assert.True(t, proto.Equal(got.Entry, decoded(t, f, key)))
	}
}

func TestAppend_keepsCreateTimeAndReplacesName(t *testing.T) {
	a := newFakeConsul()
	j := newJournal(t, 10, kvOf("dc_a", a))

	e := operationEntry()
	e.Name = "journalEntries/forged"
	e.CreateTime = timestamppb.New(at(1))

	got, err := j.Append(t.Context(), e)
	require.NoError(t, err)

	assert.Regexp(t, nameRE, got.Entry.GetName())
	assert.Contains(t, got.Entry.GetName(), "journalEntries/20261125100001000000000-")
	assert.Equal(t, "journalEntries/forged", e.GetName(), "the caller's entry is not changed")
}

func TestAppend_idsAreUnique(t *testing.T) {
	a := newFakeConsul()
	j := newJournal(t, 10, kvOf("dc_a", a))

	seen := map[string]bool{}

	for range 500 {
		got, err := j.Append(t.Context(), operationEntry())
		require.NoError(t, err)

		id := got.Entry.GetName()[len(got.Entry.GetName())-8:]
		require.False(t, seen[id], id)
		seen[id] = true
	}
}

func TestAppend_everyEntryType(t *testing.T) {
	for name, e := range map[string]*dcctlv1.JournalEntry{
		"operation": operationEntry(), "step": stepEntry(), "system": systemEntry(), "alarm": alarmEntry(),
	} {
		t.Run(name, func(t *testing.T) {
			a := newFakeConsul()
			j := newJournal(t, 10, kvOf("dc_a", a))

			got, err := j.Append(t.Context(), e)
			require.NoError(t, err)
			assert.Equal(t, 1, got.Stored())
		})
	}
}

func TestAppend_unreachableDatacenterIsSkipped(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	b.setDown(true)

	j := newJournal(t, 10, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Append(t.Context(), operationEntry())
	require.NoError(t, err)

	assert.Equal(t, 1, got.Stored())
	assert.Equal(t, journal.Written, got.Writes[0].Outcome)
	assert.Equal(t, journal.Failed, got.Writes[1].Outcome)

	reason, ok := source.ReasonOf(got.Writes[1].Err)
	require.True(t, ok)
	assert.Equal(t, source.ReasonRefused, reason)
	assert.Empty(t, b.names())
}

func TestAppend_everyDatacenterDownReportsNothingStored(t *testing.T) {
	a := newFakeConsul()
	a.setDown(true)

	j := newJournal(t, 10, kvOf("dc_a", a))

	got, err := j.Append(t.Context(), operationEntry())
	require.NoError(t, err)
	assert.Zero(t, got.Stored())
}

func TestAppend_outcomeUnknownIsReadNotRetried(t *testing.T) {
	t.Run("not_applied", func(t *testing.T) {
		a := newFakeConsul()
		a.casErr = outcomeUnknown()

		j := newJournal(t, 10, kvOf("dc_a", a))

		got, err := j.Append(t.Context(), operationEntry())
		require.NoError(t, err)

		assert.Equal(t, journal.Unknown, got.Writes[0].Outcome)
		require.ErrorIs(t, got.Writes[0].Err, consul.ErrOutcomeUnknown)
		assert.Equal(t, 1, a.casCalls, "the write is not repeated")
		assert.Zero(t, got.Stored())
	})

	t.Run("applied", func(t *testing.T) {
		a := newFakeConsul()
		a.casErr, a.casApplied = outcomeUnknown(), true

		j := newJournal(t, 10, kvOf("dc_a", a))

		got, err := j.Append(t.Context(), operationEntry())
		require.NoError(t, err)

		assert.Equal(t, journal.Written, got.Writes[0].Outcome, "the re-read found the entry")
		assert.Equal(t, 1, a.casCalls)
	})

	t.Run("re_read_fails", func(t *testing.T) {
		a := newFakeConsul()
		a.casErr, a.casApplied = outcomeUnknown(), true
		a.getErr = unreachable()

		j := newJournal(t, 10, kvOf("dc_a", a))

		got, err := j.Append(t.Context(), operationEntry())
		require.NoError(t, err)

		assert.Equal(t, journal.Unknown, got.Writes[0].Outcome)
		require.ErrorIs(t, got.Writes[0].Err, consul.ErrOutcomeUnknown)
	})
}

// idSequence returns an id generator that returns the ids in turn.
func idSequence(ids ...string) func() string {
	var n int

	return func() string {
		id := ids[min(n, len(ids)-1)]
		n++

		return id
	}
}

func TestAppend_takenKeyIsRetriedWithANewID(t *testing.T) {
	a := newFakeConsul()
	j := newJournal(t, 10, kvOf("dc_a", a))
	j.SetNewID(idSequence("deadbeef", "deadbeef", "cafebabe"))

	first, err := j.Append(t.Context(), operationEntry())
	require.NoError(t, err)

	key := kv.JournalPrefix + strings.TrimPrefix(first.Entry.GetName(), "journalEntries/")
	before := a.value(key)

	// Same time and id: the second entry must not overwrite the first.
	second := alarmEntry()
	second.CreateTime = first.Entry.GetCreateTime()

	got, err := j.Append(t.Context(), second)
	require.NoError(t, err)

	assert.Equal(t, before, a.value(key), "the first entry is left alone")
	assert.True(t, strings.HasSuffix(got.Entry.GetName(), "-cafebabe"), got.Entry.GetName())
	assert.Equal(t, journal.Written, got.Writes[0].Outcome)
	assert.Equal(t, 1, got.Stored())
	assert.Len(t, a.names(), 2)
}

func TestAppend_takenKeyTwiceIsNotStored(t *testing.T) {
	a := newFakeConsul()
	j := newJournal(t, 10, kvOf("dc_a", a))
	j.SetNewID(func() string { return "deadbeef" })

	first, err := j.Append(t.Context(), operationEntry())
	require.NoError(t, err)

	second := alarmEntry()
	second.CreateTime = first.Entry.GetCreateTime()

	got, err := j.Append(t.Context(), second)
	require.NoError(t, err)

	assert.Equal(t, journal.Exists, got.Writes[0].Outcome)
	assert.Zero(t, got.Stored(), "an entry that sits at the key is another one")
	assert.Len(t, a.names(), 1)
}

func TestAppend_rejectsEntriesThatCannotBeStored(t *testing.T) {
	tests := map[string]func(e *dcctlv1.JournalEntry){
		"no_type":            func(e *dcctlv1.JournalEntry) { e.Type = dcctlv1.JournalEntry_TYPE_UNSPECIFIED },
		"no_result":          func(e *dcctlv1.JournalEntry) { e.Result = dcctlv1.JournalEntry_RESULT_UNSPECIFIED },
		"no_action":          func(e *dcctlv1.JournalEntry) { e.Action = nil },
		"action_of_a_step":   func(e *dcctlv1.JournalEntry) { e.Action = stepEntry().GetAction() },
		"unspecified_action": func(e *dcctlv1.JournalEntry) { e.Action = &dcctlv1.JournalEntry_OperationType{} },
		"detail_too_long":    func(e *dcctlv1.JournalEntry) { e.Detail = strings.Repeat("x", 5000) },
		"error_detail_too_long": func(e *dcctlv1.JournalEntry) {
			e.Error = &dcctlv1.OperationError{Reason: "X", Detail: strings.Repeat("x", 5000)}
		},
		"entry_too_big": func(e *dcctlv1.JournalEntry) { withParams(e, map[string]any{"blob": strings.Repeat("y", 20_000)}) },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			a := newFakeConsul()
			l, rec := recordedLog()

			j, err := journal.New([]journal.KV{kvOf("dc_a", a)}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
			require.NoError(t, err)

			e := operationEntry()
			e.Detail = "free text that is not logged"
			withParams(e, map[string]any{"mode": "not logged"})
			mutate(e)

			_, err = j.Append(t.Context(), e)
			require.ErrorIs(t, err, journal.ErrInvalidEntry)
			assert.Empty(t, a.names(), "nothing is stored")

			records := rec.Records()
			require.Len(t, records, 1, "the rejection is logged once")
			assert.Equal(t, log.SeverityError, records[0].Severity())

			attrs := attributes(records[0])
			assert.Equal(t, "datacenters/dc_b", attrs["datacenter"])
			assert.Equal(t, "operations/0b9f3c2e", attrs["operation"])
			assert.Contains(t, attrs, "error")

			for _, v := range attrs {
				assert.NotContains(t, v, "not logged", "no detail or params")
			}
		})
	}
}

// storedEntry returns the only entry of f.
func storedEntry(t *testing.T, f *fakeConsul) *dcctlv1.JournalEntry {
	t.Helper()

	names := f.names()
	require.Len(t, names, 1)

	return decoded(t, f, names[0])
}

func TestAppend_redactsSecretsAndKeepsTheRest(t *testing.T) {
	tests := map[string]struct {
		mutate func(e *dcctlv1.JournalEntry)
		check  func(a *assert.Assertions, e *dcctlv1.JournalEntry)
	}{
		"password_param": {
			mutate: func(e *dcctlv1.JournalEntry) { withParams(e, map[string]any{"password": "hunter2", "mode": "QUORUM"}) },
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("set", e.GetParams().AsMap()["password"])
				a.Equal("QUORUM", e.GetParams().AsMap()["mode"])
			},
		},
		"empty_password_param": {
			mutate: func(e *dcctlv1.JournalEntry) { withParams(e, map[string]any{"password": ""}) },
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("empty", e.GetParams().AsMap()["password"])
			},
		},
		"nested_secret": {
			mutate: func(e *dcctlv1.JournalEntry) {
				withParams(e, map[string]any{"body": map[string]any{"list": []any{map[string]any{"Api-Key": "k3y", "id": 1}}}})
			},
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				item := e.GetParams().AsMap()["body"].(map[string]any)["list"].([]any)[0].(map[string]any)
				a.Equal("set", item["Api-Key"])
				a.InDelta(1, item["id"], 0)
			},
		},
		"secret_structure": {
			mutate: func(e *dcctlv1.JournalEntry) {
				withParams(e, map[string]any{"credentials": map[string]any{"user": "u", "pass": "p"}})
			},
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("set", e.GetParams().AsMap()["credentials"])
			},
		},
		"token_in_detail": {
			mutate: func(e *dcctlv1.JournalEntry) { e.Detail = "login failed, token: abc123 was refused" },
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("login failed, token: set was refused", e.GetDetail())
			},
		},
		"password_in_param_text": {
			mutate: func(e *dcctlv1.JournalEntry) { withParams(e, map[string]any{"cmd": "psql password=hunter2 -c select"}) },
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("psql password=set -c select", e.GetParams().AsMap()["cmd"])
			},
		},
		"bearer_token": {
			mutate: func(e *dcctlv1.JournalEntry) { e.Detail = "sent Authorization: Bearer abcdef123456 to consul" },
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("sent Authorization: set to consul", e.GetDetail())
			},
		},
		"error_metadata_secret": {
			mutate: func(e *dcctlv1.JournalEntry) {
				e.Error = &dcctlv1.OperationError{Reason: "X", Metadata: map[string]string{"secret": "s3", "holder": "node-a"}}
			},
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal(map[string]string{"secret": "set", "holder": "node-a"}, e.GetError().GetMetadata())
			},
		},
		"error_detail_secret": {
			mutate: func(e *dcctlv1.JournalEntry) {
				e.Error = &dcctlv1.OperationError{Reason: "X", Detail: "password=hunter2 rejected"}
			},
			check: func(a *assert.Assertions, e *dcctlv1.JournalEntry) {
				a.Equal("password=set rejected", e.GetError().GetDetail())
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := newFakeConsul()
			l, rec := recordedLog()

			j, err := journal.New([]journal.KV{kvOf("dc_a", a)}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
			require.NoError(t, err)

			e := operationEntry()
			tt.mutate(e)

			got, err := j.Append(t.Context(), e)
			require.NoError(t, err)
			assert.Equal(t, 1, got.Stored(), "the entry is stored")

			checks := assert.New(t)
			tt.check(checks, storedEntry(t, a))
			tt.check(checks, got.Entry)

			var redactedWarnings int

			for _, r := range rec.Records() {
				if r.Body().AsString() == "journal entry had secrets, they are redacted" {
					redactedWarnings++
				}

				for _, v := range attributes(r) {
					assert.NotContains(t, v, "hunter2")
					assert.NotContains(t, v, "abc123")
					assert.NotContains(t, v, "abcdef123456")
					assert.NotContains(t, v, "k3y")
					assert.NotContains(t, v, "s3")
				}
			}

			assert.Equal(t, 1, redactedWarnings, "the redaction is warned once")
		})
	}
}

func TestAppend_acceptsSafeLookalikes(t *testing.T) {
	texts := map[string]string{
		"rpc_error":         "rpc error: code = Unauthenticated desc = the request was refused",
		"consul_acl":        "Unexpected response code: 403 (ACL not found)",
		"postgres_password": `pq: password authentication failed for user "replicator"`,
		"patroni_auth":      "patroni: GET /cluster: 401 Unauthorized, WWW-Authenticate: Basic",
		"empty_secret":      "check failed: secret=",
		"working_directory": "pwd: /var/lib",
		"plural":            "the passwords of patroni were not read",
	}

	keys := []string{"token_ttl", "private_key_path", "secret_name", "credential_count", "page_token", "tokens_seen", "api_key_id", "session_token_ttl", "previous_generation"}

	for name, text := range texts {
		t.Run("text_"+name, func(t *testing.T) {
			a := newFakeConsul()
			l, rec := recordedLog()

			j, err := journal.New([]journal.KV{kvOf("dc_a", a)}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
			require.NoError(t, err)

			e := operationEntry()
			e.Detail = text

			got, err := j.Append(t.Context(), e)
			require.NoError(t, err)
			assert.Equal(t, 1, got.Stored())
			assert.Equal(t, text, storedEntry(t, a).GetDetail(), "text without a secret is kept as it is")

			for _, r := range rec.Records() {
				assert.NotEqual(t, "journal entry had secrets, they are redacted", r.Body().AsString())
			}
		})
	}

	for _, key := range keys {
		t.Run("key_"+key, func(t *testing.T) {
			a := newFakeConsul()
			j := newJournal(t, 10, kvOf("dc_a", a))

			e := withParams(operationEntry(), map[string]any{key: "value"})
			e.Error = &dcctlv1.OperationError{Reason: "X", Metadata: map[string]string{key: "value"}}

			_, err := j.Append(t.Context(), e)
			require.NoError(t, err)

			stored := storedEntry(t, a)
			assert.Equal(t, "value", stored.GetParams().AsMap()[key])
			assert.Equal(t, "value", stored.GetError().GetMetadata()[key])
		})
	}
}

// attributes returns the attributes of a record as text by key, with the
// body under "body".
func attributes(r logtest.Record) map[string]string {
	out := map[string]string{"body": r.Body().AsString()}

	for _, a := range r.Attributes() {
		out[string(a.Key)] = a.Value.String()
	}

	return out
}

func TestAppend_logsTheEntry(t *testing.T) {
	a := newFakeConsul()
	l, rec := recordedLog()

	j, err := journal.New([]journal.KV{kvOf("dc_a", a)}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
	require.NoError(t, err)

	e := withParams(operationEntry(), map[string]any{"mode": "QUORUM", "new_generation": 8})
	e.Initiator.Instance = &dcctlv1.Instance{Node: "dcctl-a1", Datacenter: "datacenters/dc_a"}

	got, err := j.Append(t.Context(), e)
	require.NoError(t, err)

	records := rec.Records()
	require.Len(t, records, 1)
	assert.Equal(t, log.SeverityInfo, records[0].Severity())

	attrs := attributes(records[0])
	assert.Equal(t, got.Entry.GetName(), attrs["entry"])
	assert.Equal(t, "OPERATION", attrs["type"])
	assert.Equal(t, "SWITCH", attrs["action"])
	assert.Equal(t, "OK", attrs["result"])
	assert.Equal(t, "datacenters/dc_b", attrs["datacenter"])
	assert.Equal(t, "operations/0b9f3c2e", attrs["operation"])
	assert.Equal(t, "switch to dc_b started", attrs["detail"])
	assert.JSONEq(t, `{"mode":"QUORUM","new_generation":8}`, attrs["params"])
	assert.Equal(t, "support", attrs["user.name"], "the attribute of a login")
	assert.Equal(t, "10.1.0.15", attrs["client.address"])
	assert.Equal(t, "dcctl-a1", attrs["service.instance.id"])
}

func TestAppend_logOfASystemEntryHasNoUser(t *testing.T) {
	l, rec := recordedLog()

	j, err := journal.New([]journal.KV{kvOf("dc_a", newFakeConsul())}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
	require.NoError(t, err)

	_, err = j.Append(t.Context(), alarmEntry())
	require.NoError(t, err)

	attrs := attributes(rec.Records()[0])
	assert.NotContains(t, attrs, "user.name")
	assert.NotContains(t, attrs, "client.address")
	assert.Equal(t, "true", attrs["initiator.system"])
}

func TestAppend_logSeverityFollowsResult(t *testing.T) {
	tests := map[string]struct {
		result dcctlv1.JournalEntry_Result
		want   log.Severity
	}{
		"ok":      {dcctlv1.JournalEntry_OK, log.SeverityInfo},
		"warning": {dcctlv1.JournalEntry_WARNING, log.SeverityWarn},
		"error":   {dcctlv1.JournalEntry_ERROR, log.SeverityError},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			l, rec := recordedLog()

			j, err := journal.New([]journal.KV{kvOf("dc_a", newFakeConsul())}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
			require.NoError(t, err)

			e := operationEntry()
			e.Result = tt.result

			_, err = j.Append(t.Context(), e)
			require.NoError(t, err)
			require.Len(t, rec.Records(), 1)
			assert.Equal(t, tt.want, rec.Records()[0].Severity())
		})
	}
}

func TestAppend_logsEvenWhenNoDatacenterAnswers(t *testing.T) {
	a := newFakeConsul()
	a.setDown(true)

	l, rec := recordedLog()

	j, err := journal.New([]journal.KV{kvOf("dc_a", a)}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
	require.NoError(t, err)

	_, err = j.Append(t.Context(), operationEntry())
	require.NoError(t, err)

	var entryRecords int

	for _, r := range rec.Records() {
		if r.Body().AsString() == "journal entry" {
			entryRecords++
		}
	}

	assert.Equal(t, 1, entryRecords)
}

func TestAppend_cancelledContextStillStoresAndLogs(t *testing.T) {
	a := newFakeConsul()
	l, rec := recordedLog()

	j, err := journal.New([]journal.KV{kvOf("dc_a", a)}, journal.Options{MaxEntries: 10, Timeout: testTimeout}, l)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := j.Append(ctx, operationEntry())
	require.NoError(t, err)

	assert.Equal(t, 1, got.Stored())
	assert.Len(t, a.names(), 1)
	require.Len(t, rec.Records(), 1)
	assert.Equal(t, "journal entry", rec.Records()[0].Body().AsString())
}
