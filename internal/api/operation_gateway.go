package api

import (
	"context"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// registerOperationGateway registers the routes of OperationService. A watch
// that fails before its first message is answered with the plain error body
// of every other failed call: the gateway writes the status line of a stream
// when it forwards the first message, and answers a later failure only with
// an `{"error": ...}` line.
func registerOperationGateway(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
	return dcctlv1.RegisterOperationServiceHandlerClient(ctx, mux, firstMessageClient{dcctlv1.NewOperationServiceClient(conn)})
}

// firstMessageClient reads the first message of a watch before the gateway
// starts to answer, so that its failure reaches the gateway as the failure of
// the call.
type firstMessageClient struct {
	dcctlv1.OperationServiceClient
}

func (c firstMessageClient) WatchOperation(ctx context.Context, in *dcctlv1.WatchOperationRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[dcctlv1.Operation], error) {
	stream, err := c.OperationServiceClient.WatchOperation(ctx, in, opts...)
	if err != nil {
		return nil, err
	}

	first, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	return &bufferedStream{ServerStreamingClient: stream, first: first}, nil
}

// bufferedStream returns first, then what the stream receives.
type bufferedStream struct {
	grpc.ServerStreamingClient[dcctlv1.Operation]

	first *dcctlv1.Operation
}

func (s *bufferedStream) Recv() (*dcctlv1.Operation, error) {
	if first := s.first; first != nil {
		s.first = nil

		return first, nil
	}

	return s.ServerStreamingClient.Recv()
}
