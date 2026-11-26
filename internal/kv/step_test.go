package kv_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

func TestRollbackDetail(t *testing.T) {
	tests := []struct {
		name   string
		detail string
		err    error
		want   string
	}{
		{name: "rolled_back", want: "rolled back"},
		{name: "rolled_back_after_detail", detail: "lag does not shrink", want: "lag does not shrink; rolled back"},
		{name: "failed", err: errors.New("start refused"), want: "rollback failed: start refused"},
		{name: "failed_after_detail", detail: "stopped", err: errors.New("start refused"), want: "stopped; rollback failed: start refused"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kv.RollbackDetail(tt.detail, tt.err)
			assert.Equal(t, tt.want, got)
			assert.True(t, kv.StepRolledBack(&dcctlv1.Operation_Step{Detail: got}))
		})
	}
}

func TestStepRolledBack(t *testing.T) {
	tests := []struct {
		name   string
		detail string
		want   bool
	}{
		{name: "empty", detail: "", want: false},
		{name: "progress", detail: "lag 300", want: false},
		{name: "mentions_rollback", detail: "nothing was rolled back here, waiting", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kv.StepRolledBack(&dcctlv1.Operation_Step{Detail: tt.detail}))
		})
	}
}
