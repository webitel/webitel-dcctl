package api

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	dcstatus "github.com/webitel/webitel-dcctl/internal/status"
)

// StatusServer implements StatusService.
type StatusServer struct {
	dcctlv1.UnimplementedStatusServiceServer

	src     StatusSource
	address string
	// alarms and verifier are nil until something provides them.
	alarms   Alarms
	verifier Verifier
	log      *logging.Logger
	now      func() time.Time
}

// NewStatusServer returns the StatusService of src. address is listen.peer of
// this instance; alarms and verifier may be nil.
func NewStatusServer(src StatusSource, address string, alarms Alarms, verifier Verifier, log *logging.Logger) *StatusServer {
	return &StatusServer{src: src, address: address, alarms: alarms, verifier: verifier, log: log, now: time.Now}
}

// GetStatus returns the status of the last collection cycle.
func (s *StatusServer) GetStatus(ctx context.Context, _ *dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
	st, err := statusAt(s.src, s.now())
	if err != nil {
		return nil, err
	}

	m := newMapper(&st, s.address)

	out := m.status()
	if m.err != nil {
		return nil, rpcError(ctx, s.log, "status cannot be shown", m.err)
	}

	if s.alarms != nil {
		out.Alarms = s.alarms.Alarms(ctx)
	}

	return out, nil
}

// VerifyStatus runs the verify checks.
func (s *StatusServer) VerifyStatus(ctx context.Context, _ *dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error) {
	if s.verifier == nil {
		return nil, status.Error(codes.Unimplemented, "verify checks are not available yet")
	}

	checks, err := s.verifier.Verify(ctx)
	if err != nil {
		return nil, rpcError(ctx, s.log, "verify checks failed", err)
	}

	return &dcctlv1.VerifyStatusResponse{Checks: checks}, nil
}

// statusAt returns the model, or errNoCycle before the first cycle.
func statusAt(src StatusSource, now time.Time) (dcstatus.Status, error) {
	st, ok := src.Status(now)
	if !ok {
		return dcstatus.Status{}, errNoCycle
	}

	return st, nil
}
