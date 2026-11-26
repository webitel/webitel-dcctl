package coord_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
)

func TestSupersedes(t *testing.T) {
	at := func(rev int64, s dcctlv1.Operation_State) *dcctlv1.Operation {
		return &dcctlv1.Operation{Revision: rev, State: s}
	}

	const (
		running     = dcctlv1.Operation_RUNNING
		interrupted = dcctlv1.Operation_INTERRUPTED
		succeeded   = dcctlv1.Operation_SUCCEEDED
	)

	tests := []struct {
		name string
		a, b *dcctlv1.Operation
		want bool
	}{
		{"higher_revision", at(2, running), at(1, succeeded), true},
		{"lower_revision", at(1, succeeded), at(2, running), false},
		{"finished_over_interrupted", at(5, succeeded), at(5, interrupted), true},
		{"interrupted_over_running", at(5, interrupted), at(5, running), true},
		{"interrupted_not_over_finished", at(5, interrupted), at(5, succeeded), false},
		{"same_change", at(5, succeeded), at(5, succeeded), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, coord.Supersedes(tt.a, tt.b))
		})
	}
}
