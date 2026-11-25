package journal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// snapshot is the journal as one KV holds it.
type snapshot struct {
	DC  string
	Err error
	// Entries are the journal keys in key order, oldest first. Keys that
	// are not journal keys are left out.
	Entries []consul.KVEntry
}

func (s snapshot) has(key string) bool {
	_, found := slices.BinarySearchFunc(s.Entries, key, func(e consul.KVEntry, k string) int { return strings.Compare(e.Key, k) })

	return found
}

// snapshots lists the journal of every KV in parallel, in name order.
func (j *Journal) snapshots(ctx context.Context) []snapshot {
	out := make([]snapshot, len(j.kvs))

	var wg sync.WaitGroup

	for i, k := range j.kvs {
		wg.Go(func() {
			out[i] = snapshot{DC: k.Name}

			entries, err := withTimeout(ctx, j.opts.Timeout, func(ctx context.Context) ([]consul.KVEntry, error) {
				return k.Client.KVList(ctx, kv.JournalPrefix)
			})
			if err != nil {
				out[i].Err = fmt.Errorf("list %s in %s: %w", kv.JournalPrefix, k.Name, err)

				return
			}

			entries = slices.DeleteFunc(entries, func(e consul.KVEntry) bool {
				_, ok := suffixOf(e.Key)

				return !ok
			})
			slices.SortFunc(entries, func(a, b consul.KVEntry) int { return strings.Compare(a.Key, b.Key) })

			out[i].Entries = entries
		})
	}

	wg.Wait()

	return out
}

// Alignment is the result of aligning one KV.
type Alignment struct {
	DC string
	// Copied is the number of entries written to the KV.
	Copied int
	// Unknown is the number of writes that may have been applied.
	Unknown int
	// Failed is the number of writes that failed.
	Failed int
	// Err is why the KV was skipped, or the first error of a write.
	Err error
}

// Align copies the entries a KV misses from the KVs that hold them, by key:
// create-only, so two instances aligning at once, or an entry written in
// the meantime, are harmless. Only the newest max_entries of all KVs are
// copied: older ones are what [Journal.Trim] deletes. Entries are copied as
// stored. An unreachable KV is skipped and an unknown outcome is counted,
// not retried; the next run tells. The result has one item per KV, in name
// order. Align fails only when ctx ends.
func (j *Journal) Align(ctx context.Context) ([]Alignment, error) {
	snaps := j.snapshots(ctx)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("align journal: %w", err)
	}

	window := j.window(snaps)
	out := make([]Alignment, len(snaps))

	var wg sync.WaitGroup

	for i, s := range snaps {
		wg.Go(func() { out[i] = j.alignKV(ctx, j.kvs[i], s, window) })
	}

	wg.Wait()

	for _, a := range out {
		if a.Err != nil || a.Copied > 0 {
			j.log.Log(ctx, alignLevel(a), "journal aligned",
				logging.Str("kv", a.DC), logging.Int("copied", a.Copied), logging.Int("unknown", a.Unknown),
				logging.Int("failed", a.Failed), logging.Err(a.Err))
		}
	}

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("align journal: %w", err)
	}

	return out, nil
}

func alignLevel(a Alignment) logging.Level {
	if a.Err != nil {
		return logging.LevelWarn
	}

	return logging.LevelInfo
}

// entryValue is a journal entry to copy.
type entryValue struct {
	key   string
	value []byte
}

// window returns the newest max_entries entries of all KVs that decode,
// newest first. The value of a key is the one of the first KV that holds it.
func (j *Journal) window(snaps []snapshot) []entryValue {
	seen := make(map[string]bool)

	var all []entryValue

	for _, s := range snaps {
		for _, e := range s.Entries {
			if seen[e.Key] || !decodes(e.Value) {
				continue
			}

			seen[e.Key] = true

			all = append(all, entryValue{key: e.Key, value: e.Value})
		}
	}

	slices.SortFunc(all, func(a, b entryValue) int { return strings.Compare(b.key, a.key) })

	if len(all) > j.opts.MaxEntries {
		all = all[:j.opts.MaxEntries]
	}

	return all
}

func decodes(value []byte) bool {
	return kv.DecodeProto(value, new(dcctlv1.JournalEntry)) == nil
}

func (j *Journal) alignKV(ctx context.Context, k KV, s snapshot, window []entryValue) Alignment {
	a := Alignment{DC: k.Name, Err: s.Err}
	if s.Err != nil {
		return a
	}

	for _, e := range window {
		if s.has(e.key) {
			continue
		}

		w := j.create(ctx, k, e.key, e.value)

		switch w.Outcome {
		case Written:
			a.Copied++
		case Unknown:
			a.Unknown++
		case Failed:
			a.Failed++
		case Exists:
		}

		if w.Err != nil && a.Err == nil {
			a.Err = w.Err
		}

		if w.Outcome == Failed || ctx.Err() != nil {
			// The KV does not answer: the rest would fail the same way.
			break
		}
	}

	return a
}

// Trimming is the result of trimming one KV.
type Trimming struct {
	DC string
	// Deleted is the number of entries deleted.
	Deleted int
	// Skipped is the number of entries that changed or were deleted by
	// another instance between the list and the delete.
	Skipped int
	// Unknown is the number of deletes that may have been applied.
	Unknown int
	// Failed is the number of deletes that failed.
	Failed int
	// Err is why the KV was skipped, or the first error of a delete.
	Err error
}

// Trim deletes the oldest entries of every KV that holds more than
// max_entries, each at the version it was listed with. Instances trimming at
// once list the same oldest entries: a delete of an entry another instance
// already deleted does nothing, so together they never delete more than the
// excess. A key that is not a journal entry is left alone. The result has
// one item per KV, in name order. Trim fails only when ctx ends.
func (j *Journal) Trim(ctx context.Context) ([]Trimming, error) {
	snaps := j.snapshots(ctx)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("trim journal: %w", err)
	}

	out := make([]Trimming, len(snaps))

	var wg sync.WaitGroup

	for i, s := range snaps {
		wg.Go(func() { out[i] = j.trimKV(ctx, j.kvs[i], s) })
	}

	wg.Wait()

	for _, t := range out {
		if t.Err != nil || t.Deleted > 0 {
			j.log.Log(ctx, trimLevel(t), "journal trimmed",
				logging.Str("kv", t.DC), logging.Int("deleted", t.Deleted), logging.Int("skipped", t.Skipped),
				logging.Int("unknown", t.Unknown), logging.Int("failed", t.Failed), logging.Err(t.Err))
		}
	}

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("trim journal: %w", err)
	}

	return out, nil
}

func trimLevel(t Trimming) logging.Level {
	if t.Err != nil {
		return logging.LevelWarn
	}

	return logging.LevelInfo
}

func (j *Journal) trimKV(ctx context.Context, k KV, s snapshot) Trimming {
	t := Trimming{DC: k.Name, Err: s.Err}
	if s.Err != nil {
		return t
	}

	excess := len(s.Entries) - j.opts.MaxEntries
	if excess <= 0 {
		return t
	}

	for _, e := range s.Entries[:excess] {
		ok, err := withTimeout(ctx, j.opts.Timeout, func(ctx context.Context) (bool, error) {
			return k.Client.KVDeleteCAS(ctx, e.Key, e.ModifyIndex)
		})

		switch {
		case err == nil && ok:
			t.Deleted++
		case err == nil:
			t.Skipped++
		case errors.Is(err, consul.ErrOutcomeUnknown):
			t.Unknown++
		default:
			t.Failed++
		}

		if err != nil && t.Err == nil {
			t.Err = err
		}

		if err != nil && !errors.Is(err, consul.ErrOutcomeUnknown) {
			// The KV does not answer: the rest would fail the same way.
			break
		}
	}

	return t
}

// Run aligns and trims the journal every interval until ctx ends, first
// right away, and returns the error of ctx. Failures are logged and the
// next round tries again.
func (j *Journal) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("interval must be positive")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Align and Trim fail only when ctx ends, which the select below returns.
		_, _ = j.Align(ctx)
		_, _ = j.Trim(ctx)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
