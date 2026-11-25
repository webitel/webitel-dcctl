package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRPCError(t *testing.T) {
	const addr = "10.2.0.89:8500"

	tests := []struct {
		name     string
		err      error
		want     codes.Code
		wantText string
	}{
		{
			name: "status",
			err:  status.Error(codes.Unavailable, "no datacenter answers"),
			want: codes.Unavailable, wantText: "no datacenter answers",
		},
		{
			name: "wrapped_status",
			err:  fmt.Errorf("kv on %s: %w", addr, status.Error(codes.Unavailable, "no datacenter answers")),
			want: codes.Unavailable, wantText: "no datacenter answers",
		},
		{
			name: "wrapped_deadline",
			err:  fmt.Errorf("kv on %s: %w", addr, context.DeadlineExceeded),
			want: codes.DeadlineExceeded, wantText: "request deadline exceeded",
		},
		{
			name: "wrapped_canceled",
			err:  fmt.Errorf("kv on %s: %w", addr, context.Canceled),
			want: codes.Canceled, wantText: "request canceled",
		},
		{
			name: "raw",
			err:  errors.New("dial tcp " + addr + ": connection refused"),
			want: codes.Internal, wantText: "read failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := assertCode(t, tt.want, rpcError(t.Context(), testLogger(), "read failed", tt.err))

			assert.Equal(t, tt.wantText, st.Message())
			assert.NotContains(t, st.Message(), addr)
		})
	}
}
