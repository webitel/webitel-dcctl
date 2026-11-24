package coord

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/arbiter"
)

// The arbiter never answers NotFound for its state, so this maps the
// errors directly instead of through a server.
func TestArbiterError(t *testing.T) {
	tests := []struct {
		name string
		key  Key
		err  error
		want error
	}{
		{name: "state_not_found", key: KeyState, err: arbiter.ErrNotFound, want: ErrUninitialized},
		{name: "lease_not_found", key: KeyLease, err: arbiter.ErrNotFound, want: ErrNotFound},
		{name: "uninitialized", key: KeyLease, err: arbiter.ErrUninitialized, want: ErrUninitialized},
		{name: "mismatch", key: KeyState, err: &arbiter.MismatchError{}, want: ErrVersionMismatch},
		{name: "outcome_unknown", key: KeyState, err: arbiter.ErrOutcomeUnknown, want: ErrOutcomeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := arbiterError(tt.key, fmt.Errorf("call: %w", tt.err))
			require.ErrorIs(t, got, tt.want)
			assert.ErrorIs(t, got, tt.err, "the arbiter error is lost")
		})
	}
}
