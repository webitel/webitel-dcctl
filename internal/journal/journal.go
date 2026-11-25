package journal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// Options configure a [Journal].
type Options struct {
	// MaxEntries is the most entries kept in the KV of one datacenter
	// (journal.max_entries).
	MaxEntries int
	// Timeout bounds each call to a KV.
	Timeout time.Duration
	// Now returns the time of an entry that has no create_time; time.Now
	// when nil.
	Now func() time.Time
}

// Journal writes, aligns, trims and reads the journal in the KVs of every
// datacenter. It is safe for concurrent use; instances of dcctl share the
// journal through the KVs only.
type Journal struct {
	kvs  []KV
	opts Options
	log  *logging.Logger

	newID func() string
}

// New returns the journal of kvs, which must be every KV the journal lives
// in.
func New(kvs []KV, opts Options, log *logging.Logger) (*Journal, error) {
	ordered, err := sorted(kvs)
	if err != nil {
		return nil, err
	}

	switch {
	case opts.MaxEntries <= 0:
		return nil, errors.New("max entries must be positive")
	case opts.Timeout <= 0:
		return nil, errors.New("kv timeout must be positive")
	case log == nil:
		return nil, errors.New("no logger")
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Journal{kvs: ordered, opts: opts, log: log, newID: randomID}, nil
}

// randomID returns 8 lowercase hex characters from crypto/rand. Together
// with the nanosecond time of the key, instances never pick the same key in
// practice, and without any coordination.
func randomID() string {
	var b [4]byte

	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// Outcome is what writing an entry to one KV did.
type Outcome string

const (
	// Written means the entry is in the KV.
	Written Outcome = "written"
	// Exists means the key is taken by another entry, which is left as it
	// is. Align copies this entry to the KVs that miss it.
	Exists Outcome = "exists"
	// Unknown means the write may have been applied and a read right after
	// it did not tell; Err matches [consul.ErrOutcomeUnknown]. Align copies
	// the entry later if it is missing.
	Unknown Outcome = "unknown"
	// Failed means the KV was skipped: it is unreachable or refused the
	// write. Err tells why.
	Failed Outcome = "failed"
)

// Write is the result of writing an entry to one KV.
type Write struct {
	DC      string
	Outcome Outcome
	Err     error
}

// Appended is the result of [Journal.Append].
type Appended struct {
	// Entry is the entry as stored: with its name and create_time.
	Entry *dcctlv1.JournalEntry
	// Writes has one result per KV, in name order.
	Writes []Write
}

// Stored returns the number of KVs that hold the entry.
func (a Appended) Stored() int {
	var n int

	for _, w := range a.Writes {
		if w.Outcome == Written {
			n++
		}
	}

	return n
}

// Append journals e: it logs it, then writes it to the KV of every
// datacenter that answers. The name, and the create_time when it is not
// set, are chosen here; e is not changed. An unreachable KV is skipped and
// an unknown outcome is read once, not retried: both are in the result,
// and [Journal.Align] later copies the entry to the KVs that miss it.
//
// Append is not stopped by the cancellation of ctx: the last entries of an
// operation are written when it is canceled. Each KV call is bounded by
// the timeout of the journal.
//
// Secrets are not journaled. The value of a params or error metadata key
// that names a secret, and the value after `token:`, `password=` or
// `Bearer` in a text, is replaced by `set` or `empty` and the rest of the
// entry is kept; the callers must still leave secrets out. An entry that
// cannot be stored at all, one without a type, action or result, or too
// big, is [ErrInvalidEntry]: it is error-logged without its detail and
// params.
//
// Append fails only for [ErrInvalidEntry]; an entry stored nowhere is
// [Appended.Stored] of 0, and the caller decides what that means for its
// action. The entry is in the log in any case.
//
// What to journal is the caller's job. These are not journaled: reads,
// requests with validate_only, and failed logins.
func (j *Journal) Append(ctx context.Context, e *dcctlv1.JournalEntry) (Appended, error) {
	ctx = context.WithoutCancel(ctx)

	entry := proto.CloneOf(e)
	if entry.GetCreateTime() == nil {
		entry.CreateTime = timestamppb.New(j.opts.Now())
	}

	if err := validate(entry); err != nil {
		j.logRejected(ctx, entry, err)

		return Appended{}, err
	}

	redacted := redactEntry(entry)

	var got Appended

	// A key that is taken by another entry is tried once more with a new id.
	for range 2 {
		key := kv.JournalKey(entry.GetCreateTime().AsTime(), j.newID())
		entry.Name = nameOf(key)

		value, err := kv.EncodeProto(entry)
		if err != nil {
			return Appended{}, fmt.Errorf("encode journal entry: %w", err)
		}

		if len(value) > maxEntrySize {
			err := invalid("entry is %d bytes, more than %d", len(value), maxEntrySize)
			j.logRejected(ctx, entry, err)

			return Appended{}, err
		}

		if got.Entry == nil {
			if redacted {
				j.log.WarnCtx(ctx, "journal entry had secrets, they are redacted", logging.Str("entry", entry.GetName()))
			}

			j.logEntry(ctx, entry)
		}

		got = Appended{Entry: proto.CloneOf(entry), Writes: j.writeAll(ctx, key, value)}

		if !slices.ContainsFunc(got.Writes, func(w Write) bool { return w.Outcome == Exists }) {
			break
		}

		j.log.WarnCtx(ctx, "journal key is taken by another entry", logging.Str("entry", entry.GetName()))
	}

	for _, w := range got.Writes {
		if w.Outcome != Written {
			j.log.WarnCtx(ctx, "journal entry not written",
				logging.Str("entry", got.Entry.GetName()), logging.Str("kv", w.DC),
				logging.Str("outcome", string(w.Outcome)), logging.Err(w.Err))
		}
	}

	return got, nil
}

// writeAll writes value to every KV in parallel, in name order.
func (j *Journal) writeAll(ctx context.Context, key string, value []byte) []Write {
	writes := make([]Write, len(j.kvs))

	var wg sync.WaitGroup

	for i, k := range j.kvs {
		wg.Go(func() { writes[i] = j.create(ctx, k, key, value) })
	}

	wg.Wait()

	return writes
}

// create writes value under key only when the key does not exist.
func (j *Journal) create(ctx context.Context, k KV, key string, value []byte) Write {
	w := Write{DC: k.Name}

	ok, err := withTimeout(ctx, j.opts.Timeout, func(ctx context.Context) (bool, error) {
		return k.Client.KVCAS(ctx, key, value, 0)
	})

	switch {
	case err == nil && ok:
		w.Outcome = Written
	case err == nil:
		w.Outcome = Exists
	case errors.Is(err, consul.ErrOutcomeUnknown):
		return j.resolve(ctx, k, key, value, err)
	default:
		w.Outcome, w.Err = Failed, err
	}

	return w
}

// resolve reads key once after a write of unknown outcome: the entry is
// either there, or the outcome stays unknown.
func (j *Journal) resolve(ctx context.Context, k KV, key string, value []byte, unknown error) Write {
	w := Write{DC: k.Name, Outcome: Unknown, Err: unknown}

	e, err := withTimeout(ctx, j.opts.Timeout, func(ctx context.Context) (*consul.KVEntry, error) {
		e, _, err := k.Client.KVGet(ctx, key)

		return e, err
	})

	switch {
	case err == nil && string(e.Value) == string(value):
		return Write{DC: k.Name, Outcome: Written}
	case err == nil:
		return Write{DC: k.Name, Outcome: Exists}
	case errors.Is(err, consul.ErrKeyNotFound):
		// Not applied so far; a late apply is possible and harmless.
	default:
		w.Err = errors.Join(unknown, err)
	}

	return w
}

// logEntry writes e to the service log: the second place of the journal.
func (j *Journal) logEntry(ctx context.Context, e *dcctlv1.JournalEntry) {
	in := e.GetInitiator()

	fields := append(safeFields(e),
		logging.Str("detail", e.GetDetail()),
		logging.Bool("initiator.system", in.GetSystem()),
	)

	if in.GetUser() != "" {
		fields = append(fields, logging.KV(semconv.UserName(in.GetUser())))
	}

	if in.GetAddress() != "" {
		fields = append(fields, logging.KV(semconv.ClientAddress(in.GetAddress())))
	}

	if node := in.GetInstance().GetNode(); node != "" {
		fields = append(fields, logging.KV(semconv.ServiceInstanceID(node)))
	}

	if er := e.GetError(); er != nil {
		fields = append(fields, logging.Str("error.reason", er.GetReason()), logging.Str("error.detail", er.GetDetail()))
	}

	if e.GetParams() != nil {
		if b, err := protojson.Marshal(e.GetParams()); err == nil {
			fields = append(fields, logging.Str("params", string(b)))
		}
	}

	level := logging.LevelInfo

	switch e.GetResult() {
	case dcctlv1.JournalEntry_ERROR:
		level = logging.LevelError
	case dcctlv1.JournalEntry_WARNING:
		level = logging.LevelWarn
	case dcctlv1.JournalEntry_OK, dcctlv1.JournalEntry_RESULT_UNSPECIFIED:
	}

	j.log.Log(ctx, level, "journal entry", fields...)
}

// logRejected logs an entry that cannot be journaled, with the fields that
// hold no free text: not its detail or params.
func (j *Journal) logRejected(ctx context.Context, e *dcctlv1.JournalEntry, err error) {
	j.log.ErrorCtx(ctx, "journal entry rejected", append(safeFields(e), logging.Err(err))...)
}

// safeFields are the fields of an entry that hold no free text.
func safeFields(e *dcctlv1.JournalEntry) []logging.Field {
	return []logging.Field{
		logging.Str("entry", e.GetName()),
		logging.Str("type", e.GetType().String()),
		logging.Str("action", actionOf(e)),
		logging.Str("result", e.GetResult().String()),
		logging.Str("datacenter", e.GetDatacenter()),
		logging.Str("operation", e.GetOperation()),
	}
}

// actionOf returns the name of the action member that matches e.type.
func actionOf(e *dcctlv1.JournalEntry) string {
	switch e.GetAction().(type) {
	case *dcctlv1.JournalEntry_OperationType:
		return e.GetOperationType().String()
	case *dcctlv1.JournalEntry_StepType:
		return e.GetStepType().String()
	case *dcctlv1.JournalEntry_SystemAction_:
		return e.GetSystemAction().String()
	case *dcctlv1.JournalEntry_AlarmType:
		return e.GetAlarmType().String()
	}

	return ""
}

func withTimeout[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return fn(ctx)
}
