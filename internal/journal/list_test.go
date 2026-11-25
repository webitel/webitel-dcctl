package journal_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/journal"
)

// mixed seeds 8 entries of every kind into f, oldest first:
//
//	1 OPERATION dc_b op1   2 ALARM dc_a   3 OPERATION dc_a op2   4 STEP op1
//	5 ALARM dc_b           6 SYSTEM dc_a  7 ALARM dc_b            8 OPERATION dc_b op2
func mixed(t *testing.T, fs ...*fakeConsul) {
	t.Helper()

	build := []struct {
		e  func() *dcctlv1.JournalEntry
		dc string
		op string
	}{
		{operationEntry, "datacenters/dc_b", "operations/op1"},
		{alarmEntry, "datacenters/dc_a", ""},
		{operationEntry, "datacenters/dc_a", "operations/op2"},
		{stepEntry, "", "operations/op1"},
		{alarmEntry, "datacenters/dc_b", ""},
		{systemEntry, "datacenters/dc_a", ""},
		{alarmEntry, "datacenters/dc_b", ""},
		{operationEntry, "datacenters/dc_b", "operations/op2"},
	}

	for i, b := range build {
		for _, f := range fs {
			e := b.e()
			e.Datacenter, e.Operation = b.dc, b.op
			seed(t, f, e, fmt.Sprintf("%08x", i+1), i+1)
		}
	}
}

func list(t *testing.T, j *journal.Journal, size int32, filter, token string) *dcctlv1.ListJournalEntriesResponse {
	t.Helper()

	got, err := j.List(t.Context(), &dcctlv1.ListJournalEntriesRequest{PageSize: size, Filter: filter, PageToken: token})
	require.NoError(t, err)

	return got
}

func ids(r *dcctlv1.ListJournalEntriesResponse) []string {
	out := make([]string, 0, len(r.GetJournalEntries()))
	for _, e := range r.GetJournalEntries() {
		out = append(out, e.GetName()[len(e.GetName())-8:])
	}

	return out
}

func invalidRequest(t *testing.T, err error) *errdetails.BadRequest {
	t.Helper()

	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.InvalidArgument, st.Code(), err)

	var (
		info *errdetails.ErrorInfo
		bad  *errdetails.BadRequest
	)

	for _, d := range st.Details() {
		switch d := d.(type) {
		case *errdetails.ErrorInfo:
			info = d
		case *errdetails.BadRequest:
			bad = d
		}
	}

	require.NotNil(t, info)
	assert.Equal(t, string(apierr.ReasonRequestInvalid), info.GetReason())
	require.NotNil(t, bad)
	require.NotEmpty(t, bad.GetFieldViolations())

	return bad
}

func TestList_newestFirst(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)

	j := newJournal(t, 100, kvOf("dc_a", a))

	got := list(t, j, 0, "", "")

	assert.Equal(t, []string{"00000008", "00000007", "00000006", "00000005", "00000004", "00000003", "00000002", "00000001"}, ids(got))
	assert.Empty(t, got.GetNextPageToken())
	assert.Equal(t, dcctlv1.JournalEntry_OPERATION, got.GetJournalEntries()[0].GetType())
	assert.NotNil(t, got.GetJournalEntries()[0].GetCreateTime())
}

func TestList_readsTheMostCompleteCopy(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	// dc_a missed entries 2 and 3; dc_b missed 1: neither copy is complete.
	for n := 1; n <= 4; n++ {
		if n != 2 && n != 3 {
			seed(t, a, operationEntry(), fmt.Sprintf("%08x", n), n)
		}

		if n != 1 {
			seed(t, b, operationEntry(), fmt.Sprintf("%08x", n), n)
		}
	}

	j := newJournal(t, 100, kvOf("dc_a", a), kvOf("dc_b", b))

	assert.Equal(t, []string{"00000004", "00000003", "00000002", "00000001"}, ids(list(t, j, 0, "", "")))
}

func TestList_oneDatacenterDownStillAnswers(t *testing.T) {
	a, b := newFakeConsul(), newFakeConsul()
	mixed(t, a, b)
	b.setDown(true)

	j := newJournal(t, 100, kvOf("dc_a", a), kvOf("dc_b", b))

	assert.Len(t, list(t, j, 0, "", "").GetJournalEntries(), 8)
}

func TestList_noDatacenterAnswersIsUnavailable(t *testing.T) {
	a := newFakeConsul()
	a.setDown(true)

	j := newJournal(t, 100, kvOf("dc_a", a))

	_, err := j.List(t.Context(), &dcctlv1.ListJournalEntriesRequest{})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestList_skipsEntriesThatDoNotDecode(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)
	a.set("dcctl/journal/20261125100009000000000-baadf00d", []byte(`not json`))

	j := newJournal(t, 100, kvOf("dc_a", a))

	assert.Len(t, list(t, j, 0, "", "").GetJournalEntries(), 8)
}

func TestList_pageSize(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 60, a)

	j := newJournal(t, 1000, kvOf("dc_a", a))

	tests := []struct {
		size int32
		want int
	}{{0, 50}, {1, 1}, {7, 7}, {60, 60}, {500, 60}}

	for _, tt := range tests {
		assert.Len(t, list(t, j, tt.size, "", "").GetJournalEntries(), tt.want, "page_size %d", tt.size)
	}
}

func TestList_pagesAreStable(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 20, a)

	j := newJournal(t, 1000, kvOf("dc_a", a))

	var (
		all   []string
		token string
		pages int
	)

	for {
		page := list(t, j, 7, "", token)
		all = append(all, ids(page)...)
		pages++

		token = page.GetNextPageToken()
		if token == "" {
			break
		}

		require.LessOrEqual(t, len(token), 1024)
		require.Less(t, pages, 10)
	}

	assert.Equal(t, 3, pages)
	require.Len(t, all, 20)

	for i, id := range all {
		assert.Equal(t, fmt.Sprintf("%08x", 20-i), id)
	}
}

func TestList_lastFullPageHasNoToken(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 6, a)

	j := newJournal(t, 1000, kvOf("dc_a", a))

	first := list(t, j, 3, "", "")
	require.NotEmpty(t, first.GetNextPageToken())

	second := list(t, j, 3, "", first.GetNextPageToken())
	assert.Equal(t, []string{"00000003", "00000002", "00000001"}, ids(second))
	assert.Empty(t, second.GetNextPageToken())
}

func TestList_newEntriesDoNotMovePages(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 10, a)

	j := newJournal(t, 1000, kvOf("dc_a", a))

	first := list(t, j, 4, "", "")

	// An entry is journaled, and the oldest one trimmed, between the pages.
	seed(t, a, alarmEntry(), "0000ffff", 50)
	_, err := newJournal(t, 9, kvOf("dc_a", a)).Trim(t.Context())
	require.NoError(t, err)

	second := list(t, j, 4, "", first.GetNextPageToken())
	assert.Equal(t, []string{"00000006", "00000005", "00000004", "00000003"}, ids(second))
}

func TestList_pageSizeMayChangeBetweenPages(t *testing.T) {
	a := newFakeConsul()
	seedN(t, 10, a)

	j := newJournal(t, 1000, kvOf("dc_a", a))

	first := list(t, j, 4, "", "")
	second := list(t, j, 2, "", first.GetNextPageToken())

	assert.Equal(t, []string{"00000006", "00000005"}, ids(second))
}

func TestList_filters(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)

	j := newJournal(t, 100, kvOf("dc_a", a))

	tests := map[string]struct {
		filter string
		want   []string
	}{
		"type":               {`type = "ALARM"`, []string{"00000007", "00000005", "00000002"}},
		"type_operation":     {`type = "OPERATION"`, []string{"00000008", "00000003", "00000001"}},
		"type_step":          {`type = "STEP"`, []string{"00000004"}},
		"type_system":        {`type = "SYSTEM"`, []string{"00000006"}},
		"datacenter":         {`datacenter = "datacenters/dc_a"`, []string{"00000006", "00000003", "00000002"}},
		"operation":          {`operation = "operations/op1"`, []string{"00000004", "00000001"}},
		"documented_example": {`type = "ALARM" AND datacenter = "datacenters/dc_b"`, []string{"00000007", "00000005"}},
		"three_terms":        {`type = "OPERATION" AND datacenter = "datacenters/dc_b" AND operation = "operations/op2"`, []string{"00000008"}},
		"implicit_and":       {`type = "ALARM" datacenter = "datacenters/dc_a"`, []string{"00000002"}},
		"or":                 {`type = "STEP" OR type = "SYSTEM"`, []string{"00000006", "00000004"}},
		"parentheses":        {`(type = "STEP" OR type = "SYSTEM") AND operation = "operations/op1"`, []string{"00000004"}},
		"not":                {`NOT type = "ALARM" AND datacenter = "datacenters/dc_b"`, []string{"00000008", "00000001"}},
		"not_equal":          {`type != "OPERATION" AND type != "ALARM"`, []string{"00000006", "00000004"}},
		"minus":              {`-type = "ALARM" AND operation = "operations/op2"`, []string{"00000008", "00000003"}},
		"no_match":           {`datacenter = "datacenters/dc_x"`, []string{}},
		"empty_value":        {`operation = ""`, []string{"00000007", "00000006", "00000005", "00000002"}},
		"empty_filter":       {``, []string{"00000008", "00000007", "00000006", "00000005", "00000004", "00000003", "00000002", "00000001"}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, ids(list(t, j, 0, tt.filter, "")))
		})
	}
}

func TestList_filterAcrossPages(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)

	j := newJournal(t, 100, kvOf("dc_a", a))
	filter := `type = "ALARM"`

	first := list(t, j, 2, filter, "")
	assert.Equal(t, []string{"00000007", "00000005"}, ids(first))
	require.NotEmpty(t, first.GetNextPageToken())

	second := list(t, j, 2, filter, first.GetNextPageToken())
	assert.Equal(t, []string{"00000002"}, ids(second))
	assert.Empty(t, second.GetNextPageToken())
}

func TestList_invalidFilter(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)

	j := newJournal(t, 100, kvOf("dc_a", a))

	for name, filter := range map[string]string{
		"unknown_field":    `severity = "ERROR"`,
		"unterminated":     `type = "ALARM`,
		"no_value":         `type =`,
		"bare_word":        `type = ALARM`,
		"bad_type_value":   `type = "BOGUS"`,
		"unspecified_type": `type = "TYPE_UNSPECIFIED"`,
		"lowercase_type":   `type = "alarm"`,
		"ordering":         `type > "ALARM"`,
		"has_operator":     `datacenter:"datacenters/dc_a"`,
		"number":           `type = 1`,
		"function":         `timestamp("2026-11-25T10:00:00Z") = type`,
		"garbage":          `((`,
		"member":           `initiator.user = "support"`,
		"field_to_field":   `type = datacenter`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := j.List(t.Context(), &dcctlv1.ListJournalEntriesRequest{Filter: filter})
			bad := invalidRequest(t, err)
			assert.Equal(t, "filter", bad.GetFieldViolations()[0].GetField())
			assert.NotEmpty(t, bad.GetFieldViolations()[0].GetDescription())
		})
	}
}

func TestList_tokenIsBoundToTheFilter(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)

	j := newJournal(t, 100, kvOf("dc_a", a))

	first := list(t, j, 1, `type = "ALARM"`, "")
	token := first.GetNextPageToken()
	require.NotEmpty(t, token)

	for name, filter := range map[string]string{
		"other_filter": `type = "OPERATION"`,
		"no_filter":    ``,
		"extended":     `type = "ALARM" AND datacenter = "datacenters/dc_b"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := j.List(t.Context(), &dcctlv1.ListJournalEntriesRequest{Filter: filter, PageToken: token})
			bad := invalidRequest(t, err)
			assert.Equal(t, "filter", bad.GetFieldViolations()[0].GetField())
		})
	}

	got := list(t, j, 1, `type = "ALARM"`, token)
	assert.Equal(t, []string{"00000005"}, ids(got))

	// A token of a request without a filter does not fit a filtered one.
	plain := list(t, j, 1, "", "").GetNextPageToken()
	_, err := j.List(t.Context(), &dcctlv1.ListJournalEntriesRequest{Filter: `type = "ALARM"`, PageToken: plain})
	invalidRequest(t, err)
}

func TestList_invalidToken(t *testing.T) {
	a := newFakeConsul()
	mixed(t, a)

	j := newJournal(t, 100, kvOf("dc_a", a))

	for name, token := range map[string]string{
		"not_base64":      "!!!",
		"not_a_token":     "bm90LWEtdG9rZW4",
		"empty_key":       "fDAwMDAwMDAwMDAwMDAwMA",
		"truncated":       list(t, j, 1, "", "").GetNextPageToken()[:10],
		"standard_base64": "MjAyNjExMjMxMDEyNDAwMDAwMDAwMDAtYTkxYzAzZDc=",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := j.List(t.Context(), &dcctlv1.ListJournalEntriesRequest{PageToken: token})
			bad := invalidRequest(t, err)
			assert.Equal(t, "page_token", bad.GetFieldViolations()[0].GetField())
		})
	}
}
