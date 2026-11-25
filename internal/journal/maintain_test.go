package journal_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

func TestAlign_copiesMissingEntriesBothWays(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()

	k1 := seed(t, a, operationEntry(), "00000001", 1)
	k2 := seed(t, a, alarmEntry(), "00000002", 2)
	seed(t, b, alarmEntry(), "00000002", 2)
	k3 := seed(t, b, stepEntry(), "00000003", 3)

	j := newJournal(t, 10, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Align(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []journal.Alignment{{DC: "dc_a", Copied: 1}, {DC: "dc_b", Copied: 1}}, got)
	assert.Equal(t, []string{k1, k2, k3}, a.names())
	assert.Equal(t, []string{k1, k2, k3}, b.names())
	assert.Equal(t, a.value(k1), b.value(k1), "copied as stored")
	assert.Equal(t, a.value(k3), b.value(k3))
}

func TestAlign_isIdempotent(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	seed(t, a, operationEntry(), "00000001", 1)

	j := newJournal(t, 10, kvOf("dc_a", a), kvOf("dc_b", b))

	_, err := j.Align(t.Context())
	require.NoError(t, err)

	got, err := j.Align(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []journal.Alignment{{DC: "dc_a"}, {DC: "dc_b"}}, got)
}

func TestAlign_twoInstancesCopyOnce(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	seedN(t, 20, a)

	j1 := newJournal(t, 100, kvOf("dc_a", a), kvOf("dc_b", b))
	j2 := newJournal(t, 100, kvOf("dc_a", a), kvOf("dc_b", b))

	var wg sync.WaitGroup

	for _, j := range []*journal.Journal{j1, j2} {
		wg.Go(func() {
			_, err := j.Align(t.Context())
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	assert.Equal(t, a.names(), b.names())
}

func TestAlign_skipsUnreachableDatacenter(t *testing.T) {
	a, b, c := newFakeConsul(), newFakeConsul(), newFakeConsul()
	seed(t, a, operationEntry(), "00000001", 1)
	b.setDown(true)

	j := newJournal(t, 10, kvOf("dc_a", a), kvOf("dc_b", b), kvOf("dc_c", c))

	got, err := j.Align(t.Context())
	require.NoError(t, err)

	require.Len(t, got, 3)
	assert.Equal(t, 0, got[0].Copied)
	require.Error(t, got[1].Err, "dc_b is reported")
	assert.Equal(t, journal.Alignment{DC: "dc_c", Copied: 1}, got[2])
	assert.Len(t, c.names(), 1)
	assert.Empty(t, b.names())
}

func TestAlign_copiesOnlyTheNewestMaxEntries(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	keys := seedN(t, 6, a)

	j := newJournal(t, 3, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Align(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 3, got[1].Copied, "entries trimming would delete again are not copied")
	assert.Equal(t, keys[3:], b.names())
}

func TestAlign_reportsUnknownOutcome(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	seed(t, a, operationEntry(), "00000001", 1)

	b.casErr = outcomeUnknown()

	j := newJournal(t, 10, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Align(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, got[1].Unknown)
	assert.Zero(t, got[1].Copied)
	require.ErrorIs(t, got[1].Err, consul.ErrOutcomeUnknown)
	assert.Equal(t, 1, b.casCalls, "not retried")
}

func TestAlign_ignoresForeignKeysAndCorruptValues(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	good := seed(t, a, operationEntry(), "00000001", 1)
	a.set(kv.JournalPrefix+"not-a-journal-key", []byte(`{}`))
	a.set(kv.JournalKey(at(2), "00000002"), []byte(`not json`))

	j := newJournal(t, 10, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Align(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, got[1].Copied)
	assert.Equal(t, []string{good}, b.names())
}

func TestTrim_deletesTheOldestEntries(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	keys := seedN(t, 7, a)
	seedN(t, 4, b)

	j := newJournal(t, 5, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Trim(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []journal.Trimming{{DC: "dc_a", Deleted: 2}, {DC: "dc_b"}}, got)
	assert.Equal(t, keys[2:], a.names())
	assert.Len(t, b.names(), 4)
}

func TestTrim_leavesForeignKeys(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 3, a)
	a.set(kv.JournalPrefix+"zz-foreign", []byte(`{}`))

	j := newJournal(t, 2, kvOf("dc_a", a))

	got, err := j.Trim(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, got[0].Deleted)
	assert.Len(t, a.names(), 3)
	assert.Contains(t, a.names(), kv.JournalPrefix+"zz-foreign")
}

func TestTrim_concurrentInstancesKeepExactlyMaxEntries(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	keys := seedN(t, 60, a, b)

	js := make([]*journal.Journal, 0, 6)

	for range 6 {
		js = append(js, newJournal(t, 25, kvOf("dc_a", a), kvOf("dc_b", b)))
	}

	var wg sync.WaitGroup

	for _, j := range js {
		wg.Go(func() {
			_, err := j.Trim(t.Context())
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	assert.Equal(t, keys[35:], a.names())
	assert.Equal(t, keys[35:], b.names())
}

func TestTrim_concurrentWithAppends(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 30, a)

	j := newJournal(t, 20, kvOf("dc_a", a))

	var wg sync.WaitGroup

	wg.Go(func() {
		for range 20 {
			_, err := j.Append(t.Context(), operationEntry())
			assert.NoError(t, err)
		}
	})

	for range 4 {
		wg.Go(func() {
			for range 10 {
				_, err := j.Trim(t.Context())
				assert.NoError(t, err)
			}
		})
	}

	wg.Wait()

	_, err := j.Trim(t.Context())
	require.NoError(t, err)
	assert.Len(t, a.names(), 20, fmt.Sprint(a.names()))
}

func TestTrim_changedEntryIsSkippedNotFailed(t *testing.T) {
	a := newFakeConsul()
	keys := seedN(t, 4, a)

	// Another writer rewrites the oldest key between the list and the delete.
	a.beforeDelete = func(f *fakeConsul, key string) {
		if key == keys[0] {
			f.set(key, f.value(key))
		}
	}

	j := newJournal(t, 2, kvOf("dc_a", a))

	got, err := j.Trim(t.Context())
	require.NoError(t, err)

	assert.Equal(t, journal.Trimming{DC: "dc_a", Deleted: 1, Skipped: 1}, got[0])
	assert.Equal(t, append([]string{keys[0]}, keys[2:]...), a.names(), "the changed entry stays for the next run")
}

func TestTrim_reportsUnreachableAndUnknown(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	seedN(t, 4, a, b)
	a.setDown(true)

	b.delErr = outcomeUnknown()

	j := newJournal(t, 2, kvOf("dc_a", a), kvOf("dc_b", b))

	got, err := j.Trim(t.Context())
	require.NoError(t, err)

	require.Error(t, got[0].Err)
	assert.Zero(t, got[0].Deleted)
	assert.Equal(t, 2, got[1].Unknown)
	require.ErrorIs(t, got[1].Err, consul.ErrOutcomeUnknown)
	assert.Len(t, b.names(), 4)
}

func TestRun_alignsAndTrimsEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := newFakeConsul(), newFakeConsul()
		seedN(t, 6, a)

		j := newJournal(t, 4, kvOf("dc_a", a), kvOf("dc_b", b))

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- j.Run(ctx, time.Minute) }()

		synctest.Wait()
		assert.Len(t, a.names(), 4, "the first round runs at once")
		assert.Equal(t, a.names(), b.names())

		// An entry missed by dc_b, and one entry too many in dc_a.
		seed(t, a, alarmEntry(), "0000ffff", 50)
		time.Sleep(time.Minute)
		synctest.Wait()

		assert.Len(t, a.names(), 4)
		assert.Equal(t, a.names(), b.names())

		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestRun_needsAnInterval(t *testing.T) {
	j := newJournal(t, 4, kvOf("dc_a", newFakeConsul()))

	require.Error(t, j.Run(t.Context(), 0))
}
