package collect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

func TestObserve(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)
	refused := &source.Error{Endpoint: "10.0.0.1:8500", Reason: source.ReasonRefused, Err: errors.New("connection refused")}

	reachable := Source[string]{Reachable: true, Data: "a", ObserveTime: t0}
	failing := Source[string]{Reason: source.ReasonRefused, Err: refused, UnreachableSince: t0, Data: "a", ObserveTime: t0}

	tests := []struct {
		name string
		prev Source[string]
		data string
		err  error
		want Source[string]
	}{
		{
			name: "reachable",
			prev: Source[string]{},
			data: "a",
			want: Source[string]{Reachable: true, Data: "a", ObserveTime: t1},
		},
		{
			name: "first_failure",
			prev: Source[string]{},
			err:  refused,
			want: Source[string]{Reason: source.ReasonRefused, Err: refused, UnreachableSince: t1},
		},
		{
			name: "failure_keeps_last_data",
			prev: reachable,
			err:  refused,
			want: Source[string]{Reason: source.ReasonRefused, Err: refused, UnreachableSince: t1, Data: "a", ObserveTime: t0},
		},
		{
			name: "still_failing_keeps_since",
			prev: failing,
			err:  refused,
			want: failing,
		},
		{
			name: "recovered",
			prev: failing,
			data: "b",
			want: Source[string]{Reachable: true, Data: "b", ObserveTime: t1},
		},
		{
			name: "unclassified_deadline_is_timeout",
			prev: Source[string]{},
			err:  fmt.Errorf("read: %w", context.DeadlineExceeded),
			want: Source[string]{Reason: source.ReasonTimeout, UnreachableSince: t1},
		},
		{
			name: "canceled_by_the_cycle_is_timeout",
			prev: Source[string]{},
			err:  fmt.Errorf("get: %w", context.Canceled),
			want: Source[string]{Reason: source.ReasonTimeout, UnreachableSince: t1},
		},
		{
			name: "no_endpoints",
			prev: Source[string]{},
			err:  fmt.Errorf("resolve: %w", nomad.ErrNoEndpoints),
			want: Source[string]{Reason: source.ReasonNoEndpoints, UnreachableSince: t1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := observe(tt.prev, tt.data, tt.err, t1)

			if tt.err != nil {
				assert.ErrorIs(t, got.Err, tt.err)

				got.Err, tt.want.Err = nil, nil
			}

			assert.Equal(t, tt.want, got)
		})
	}
}
