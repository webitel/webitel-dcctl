package journal_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

const testTimeout = 3 * time.Second

var baseTime = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

// at returns the n-th second after baseTime.
func at(n int) time.Time { return baseTime.Add(time.Duration(n) * time.Second) }

func quietLog() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

func recordedLog() (*logging.Logger, *logtest.Recorder) {
	rec := logtest.NewRecorder()

	return logging.New(logging.WithProvider(rec)), rec
}

// operationEntry is a valid OPERATION entry.
func operationEntry() *dcctlv1.JournalEntry {
	return &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_OPERATION,
		Initiator:  &dcctlv1.Initiator{User: "support", Address: "10.1.0.15"},
		Datacenter: "datacenters/dc_b",
		Operation:  "operations/0b9f3c2e",
		Action:     &dcctlv1.JournalEntry_OperationType{OperationType: dcctlv1.Operation_SWITCH},
		Result:     dcctlv1.JournalEntry_OK,
		Detail:     "switch to dc_b started",
	}
}

func alarmEntry() *dcctlv1.JournalEntry {
	return &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_ALARM,
		Initiator:  &dcctlv1.Initiator{System: true},
		Datacenter: "datacenters/dc_a",
		Action:     &dcctlv1.JournalEntry_AlarmType{AlarmType: dcctlv1.Status_Alarm_SPLIT_BRAIN},
		Result:     dcctlv1.JournalEntry_ERROR,
		Detail:     "managed jobs run in two datacenters",
	}
}

func systemEntry() *dcctlv1.JournalEntry {
	return &dcctlv1.JournalEntry{
		Type:       dcctlv1.JournalEntry_SYSTEM,
		Initiator:  &dcctlv1.Initiator{System: true},
		Datacenter: "datacenters/dc_a",
		Action:     &dcctlv1.JournalEntry_SystemAction_{SystemAction: dcctlv1.JournalEntry_FENCE_APPS},
		Result:     dcctlv1.JournalEntry_OK,
	}
}

func stepEntry() *dcctlv1.JournalEntry {
	return &dcctlv1.JournalEntry{
		Type:      dcctlv1.JournalEntry_STEP,
		Operation: "operations/0b9f3c2e",
		Action:    &dcctlv1.JournalEntry_StepType{StepType: dcctlv1.Operation_Step_APPS_STOP},
		Result:    dcctlv1.JournalEntry_WARNING,
	}
}

// stored is a journal entry as it is found in the KV: the key time is its
// create_time.
func stored(e *dcctlv1.JournalEntry, id string, created time.Time) (string, []byte, *dcctlv1.JournalEntry) {
	key := kv.JournalKey(created, id)

	out := e
	out.Name = "journalEntries/" + key[len(kv.JournalPrefix):]
	out.CreateTime = timestamppb.New(created)

	b, err := kv.EncodeProto(out)
	if err != nil {
		panic(err)
	}

	return key, b, out
}

// seed puts an entry of e at second n under id into f.
func seed(t *testing.T, f *fakeConsul, e *dcctlv1.JournalEntry, id string, n int) string {
	t.Helper()

	key, b, _ := stored(e, id, at(n))
	f.set(key, b)

	return key
}

// seedN puts n OPERATION entries at seconds 1..n into every f and returns
// their keys oldest first.
func seedN(t *testing.T, n int, fs ...*fakeConsul) []string {
	t.Helper()

	keys := make([]string, 0, n)

	for i := 1; i <= n; i++ {
		var key string

		for _, f := range fs {
			key = seed(t, f, operationEntry(), fmt.Sprintf("%08x", i), i)
		}

		keys = append(keys, key)
	}

	return keys
}

func kvOf(name string, f *fakeConsul) journal.KV {
	return journal.KV{Name: name, Client: f}
}

func newJournal(t *testing.T, maxEntries int, kvs ...journal.KV) *journal.Journal {
	t.Helper()

	j, err := journal.New(kvs, journal.Options{MaxEntries: maxEntries, Timeout: testTimeout}, quietLog())
	require.NoError(t, err)

	return j
}

func decoded(t *testing.T, f *fakeConsul, key string) *dcctlv1.JournalEntry {
	t.Helper()

	b := f.value(key)
	require.NotNil(t, b, key)

	e := new(dcctlv1.JournalEntry)
	require.NoError(t, kv.DecodeProto(b, e))

	return e
}

func withParams(e *dcctlv1.JournalEntry, m map[string]any) *dcctlv1.JournalEntry {
	p, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}

	e.Params = p

	return e
}
