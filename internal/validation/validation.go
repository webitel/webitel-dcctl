// Package validation checks gRPC requests against their protovalidate rules
// before the handler runs.
package validation

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// UnaryServerInterceptor rejects a request that breaks its rules with
// REQUEST_INVALID and the violations as BadRequest field violations.
func UnaryServerInterceptor(v protovalidate.Validator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := validate(v, req); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor checks every message the handler receives, like
// UnaryServerInterceptor.
func StreamServerInterceptor(v protovalidate.Validator) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &stream{ServerStream: ss, v: v})
	}
}

type stream struct {
	grpc.ServerStream

	v protovalidate.Validator
}

func (s *stream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	return validate(s.v, m)
}

func validate(v protovalidate.Validator, req any) error {
	msg, ok := req.(proto.Message)
	if !ok {
		return nil
	}

	err := v.Validate(msg)
	if err == nil {
		return nil
	}

	var invalid *protovalidate.ValidationError
	if !errors.As(err, &invalid) {
		// The rules of the message do not compile or cannot be evaluated:
		// a server bug, not a bad request.
		return status.Errorf(codes.Internal, "validate request: %v", err)
	}

	bad := &errdetails.BadRequest{FieldViolations: make([]*errdetails.BadRequest_FieldViolation, 0, len(invalid.Violations))}
	for _, violation := range invalid.Violations {
		bad.FieldViolations = append(bad.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       protovalidate.FieldPathString(violation.Proto.GetField()),
			Description: violation.Proto.GetMessage(),
		})
	}

	return apierr.New(apierr.ReasonRequestInvalid, "invalid request", bad)
}
