package runtime

import (
	"context"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// fakeStatus answers GetStatus, and VerifyStatus with PREFLIGHT_FAILED. With
// entered and release set, GetStatus signals entered and waits for release.
type fakeStatus struct {
	dcctlv1.UnimplementedStatusServiceServer

	entered chan struct{}
	release chan struct{}
}

func (f *fakeStatus) GetStatus(ctx context.Context, _ *dcctlv1.GetStatusRequest) (*dcctlv1.Status, error) {
	if f.entered != nil {
		close(f.entered)

		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return &dcctlv1.Status{Name: "status", DetectedActiveDatacenter: "datacenters/dc_a"}, nil
}

func (f *fakeStatus) VerifyStatus(context.Context, *dcctlv1.VerifyStatusRequest) (*dcctlv1.VerifyStatusResponse, error) {
	st, err := status.New(codes.FailedPrecondition, "preflight failed").WithDetails(
		&errdetails.ErrorInfo{Domain: apierr.Domain, Reason: string(apierr.ReasonPreflightFailed)},
		&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{
			{Type: "check", Subject: "coord.lock_free", Description: "dcctl/lock is held"},
		}},
		protoadapt.MessageV1Of(&dcctlv1.Preflight{
			Checks: []*dcctlv1.Check{{Id: "coord.lock_free", Result: dcctlv1.Check_FAIL}},
		}),
	)
	if err != nil {
		return nil, err
	}

	return nil, st.Err()
}

// missingOperation fails WatchOperation before the first message.
const missingOperation = "operations/missing"

// fakeOperations streams one operation, then OPERATION_NOT_FOUND;
// missingOperation gets OPERATION_NOT_FOUND at once.
type fakeOperations struct {
	dcctlv1.UnimplementedOperationServiceServer
}

func (fakeOperations) WatchOperation(req *dcctlv1.WatchOperationRequest, stream grpc.ServerStreamingServer[dcctlv1.Operation]) error {
	if req.GetName() != missingOperation {
		if err := stream.Send(&dcctlv1.Operation{Name: req.GetName()}); err != nil {
			return err
		}
	}

	st, err := status.New(codes.NotFound, "operation not found").WithDetails(&errdetails.ErrorInfo{
		Domain:   apierr.Domain,
		Reason:   string(apierr.ReasonOperationNotFound),
		Metadata: map[string]string{"operation": req.GetName()},
	})
	if err != nil {
		return err
	}

	return st.Err()
}
