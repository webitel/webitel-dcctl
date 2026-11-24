package validation_test

import (
	"context"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/validation"
)

func newValidator(t *testing.T) protovalidate.Validator {
	t.Helper()

	v, err := protovalidate.New()
	require.NoError(t, err)

	return v
}

// requireInvalid checks err is REQUEST_INVALID with one violation of field.
func requireInvalid(t *testing.T, err error, field string) {
	t.Helper()

	st := status.Convert(err)
	require.Equal(t, codes.InvalidArgument, st.Code(), "error = %v", err)

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
	assert.Equal(t, apierr.Domain, info.GetDomain())
	assert.Equal(t, string(apierr.ReasonRequestInvalid), info.GetReason())

	require.NotNil(t, bad)
	require.Len(t, bad.GetFieldViolations(), 1)
	assert.Equal(t, field, bad.GetFieldViolations()[0].GetField())
	assert.NotEmpty(t, bad.GetFieldViolations()[0].GetDescription())
}

func TestUnaryServerInterceptor(t *testing.T) {
	intercept := validation.UnaryServerInterceptor(newValidator(t))
	info := &grpc.UnaryServerInfo{FullMethod: dcctlv1.OperationService_GetOperation_FullMethodName}

	t.Run("invalid", func(t *testing.T) {
		called := false
		handler := func(context.Context, any) (any, error) {
			called = true

			return &dcctlv1.Operation{}, nil
		}

		_, err := intercept(t.Context(), &dcctlv1.GetOperationRequest{Name: "op1"}, info, handler)

		requireInvalid(t, err, "name")
		assert.False(t, called, "handler must not run for an invalid request")
	})

	t.Run("valid", func(t *testing.T) {
		handler := func(context.Context, any) (any, error) {
			return &dcctlv1.Operation{Name: "operations/op1"}, nil
		}

		got, err := intercept(t.Context(), &dcctlv1.GetOperationRequest{Name: "operations/op1"}, info, handler)

		require.NoError(t, err)
		assert.Equal(t, "operations/op1", got.(*dcctlv1.Operation).GetName())
	})
}

// recvStream serves one request to RecvMsg.
type recvStream struct {
	grpc.ServerStream

	req *dcctlv1.WatchOperationRequest
}

func (s *recvStream) Context() context.Context { return context.Background() }

func (s *recvStream) RecvMsg(m any) error {
	m.(*dcctlv1.WatchOperationRequest).Name = s.req.GetName()

	return nil
}

// watch runs the stream interceptor around a handler that receives a request
// with name.
func watch(t *testing.T, name string) error {
	t.Helper()

	intercept := validation.StreamServerInterceptor(newValidator(t))
	info := &grpc.StreamServerInfo{FullMethod: dcctlv1.OperationService_WatchOperation_FullMethodName, IsServerStream: true}

	handler := func(_ any, ss grpc.ServerStream) error {
		return ss.RecvMsg(new(dcctlv1.WatchOperationRequest))
	}

	return intercept(nil, &recvStream{req: &dcctlv1.WatchOperationRequest{Name: name}}, info, handler)
}

func TestStreamServerInterceptor(t *testing.T) {
	t.Run("invalid", func(t *testing.T) {
		requireInvalid(t, watch(t, "op1"), "name")
	})

	t.Run("valid", func(t *testing.T) {
		require.NoError(t, watch(t, "operations/op1"))
	})
}
