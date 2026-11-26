package apierr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func TestReasonOf(t *testing.T) {
	foreign, err := status.New(codes.Aborted, "busy").WithDetails(&errdetails.ErrorInfo{Domain: "other.example", Reason: "LOCK_BUSY"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		err  error
		want apierr.Reason
	}{
		{"status", apierr.New(apierr.ReasonLockBusy, "busy"), apierr.ReasonLockBusy},
		{"wrapped", fmt.Errorf("start: %w", apierr.New(apierr.ReasonOperationRunning, "running")), apierr.ReasonOperationRunning},
		{"other_domain", foreign.Err(), ""},
		{"no_info", status.Error(codes.Internal, "internal"), ""},
		{"not_a_status", errors.New("plain"), ""},
		{"nil", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, apierr.ReasonOf(tt.err))
		})
	}
}
