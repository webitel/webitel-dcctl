package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func TestListJournalEntries(t *testing.T) {
	example := swaggerExample(t, "/v1/journalEntries", "get", "200")

	resp := new(dcctlv1.ListJournalEntriesResponse)
	require.NoError(t, protojson.Unmarshal([]byte(example), resp))

	opts := testOptions()
	opts.Journal = fakeJournal{resp: resp}
	h := startAPI(t, opts)

	got, err := dcctlv1.NewJournalServiceClient(h.conn).ListJournalEntries(callCtx(t),
		&dcctlv1.ListJournalEntriesRequest{Filter: `type = "ALARM"`})
	require.NoError(t, err)
	assertExample(t, example, gatewayJSON(t, got), nil)

	httpResp := h.get(t, `/api/v1/journalEntries?filter=type%20%3D%20%22ALARM%22`)
	require.Equal(t, http.StatusOK, httpResp.code, httpResp.body)
	assertExample(t, example, httpResp.body, nil)
}

func TestListJournalEntriesUnreachable(t *testing.T) {
	opts := testOptions()
	opts.Journal = fakeJournal{resp: &dcctlv1.ListJournalEntriesResponse{Unreachable: []string{"datacenters/dc_b"}}}
	h := startAPI(t, opts)

	resp := h.get(t, "/api/v1/journalEntries")
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	assert.JSONEq(t, `{"unreachable":["datacenters/dc_b"]}`, resp.body)
}

func TestListJournalEntriesKVUnavailable(t *testing.T) {
	opts := testOptions()
	opts.Journal = fakeJournal{err: apierr.KVUnavailable("the journal is not readable: no datacenter answers",
		[]string{"dc_a", "dc_b"})}
	h := startAPI(t, opts)

	resp := h.get(t, "/api/v1/journalEntries")
	assert.Equal(t, http.StatusServiceUnavailable, resp.code)
	assert.JSONEq(t, swaggerExample(t, "/v1/journalEntries", "get", "503"), resp.body)
}

func TestListJournalEntriesErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		want       codes.Code
		wantDetail string
	}{
		{
			name: "unavailable",
			err:  status.Error(codes.Unavailable, "the journal is not readable: no datacenter answers"),
			want: codes.Unavailable, wantDetail: "the journal is not readable: no datacenter answers",
		},
		{
			name: "raw",
			err:  errors.New("list dcctl/journal on 10.2.0.89:8500: refused"),
			want: codes.Internal, wantDetail: "journal cannot be read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewJournalServer(fakeJournal{err: tt.err}, testLogger())

			_, err := s.ListJournalEntries(t.Context(), &dcctlv1.ListJournalEntriesRequest{})
			st := assertCode(t, tt.want, err)
			assert.Equal(t, tt.wantDetail, st.Message())
		})
	}
}
