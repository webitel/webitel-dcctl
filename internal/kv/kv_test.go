package kv_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestJournalKey(t *testing.T) {
	createTime := time.Date(2026, 11, 23, 17, 30, 0, 5, time.FixedZone("EET", 2*60*60))

	got := kv.JournalKey(createTime, "0b6c2f1e")

	assert.Equal(t, "dcctl/journal/20261123153000000000005-0b6c2f1e", got)
}

func TestJournalKeyOrder(t *testing.T) {
	base := time.Date(2026, 11, 23, 15, 30, 0, 0, time.UTC)
	keys := []string{
		kv.JournalKey(base, "c"),
		kv.JournalKey(base.Add(5*time.Nanosecond), "b"),
		kv.JournalKey(base.Add(500*time.Millisecond), "a"),
		kv.JournalKey(base.Add(time.Second), "a"),
	}

	assert.IsIncreasing(t, keys)
}
