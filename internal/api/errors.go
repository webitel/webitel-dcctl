package api

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// errNoCycle is the answer of a read from the status before the first
// collection cycle finished: an empty status would look like an installation
// with nothing wrong.
var errNoCycle = status.Error(codes.Unavailable, "no collection cycle has finished yet")

// rpcError returns err as the API answers it. A status error in the chain is
// answered as it is, without the text that wraps it; a context error with its
// code and a fixed message. Anything else is logged and answered with
// Internal and msg. The text around an error may name addresses of the
// installation, and the client sees the message.
func rpcError(ctx context.Context, log *logging.Logger, msg string, err error) error {
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		return gs.GRPCStatus().Err()
	}

	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}

	log.ErrorCtx(ctx, msg, logging.Err(err))

	return status.Error(codes.Internal, msg)
}

func datacenterNotFound(dc string) error {
	return apierr.Status(apierr.ReasonDatacenterNotFound, fmt.Sprintf("datacenter %s not found", dc),
		map[string]string{"datacenter": datacenterName(dc)}).Err()
}
