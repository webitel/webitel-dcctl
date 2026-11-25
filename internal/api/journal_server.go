package api

import (
	"context"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// JournalServer implements JournalService.
type JournalServer struct {
	dcctlv1.UnimplementedJournalServiceServer

	journal Journal
	log     *logging.Logger
}

// NewJournalServer returns the JournalService of j.
func NewJournalServer(j Journal, log *logging.Logger) *JournalServer {
	return &JournalServer{journal: j, log: log}
}

// ListJournalEntries returns a page of the journal, newest first, merged from
// the KV of every datacenter that answers. Entries only an unreachable
// datacenter holds are missing from it.
func (s *JournalServer) ListJournalEntries(ctx context.Context, req *dcctlv1.ListJournalEntriesRequest) (*dcctlv1.ListJournalEntriesResponse, error) {
	resp, err := s.journal.List(ctx, req)
	if err != nil {
		return nil, rpcError(ctx, s.log, "journal cannot be read", err)
	}

	return resp, nil
}
