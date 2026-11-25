package collect

import (
	"time"

	"github.com/webitel/webitel-dcctl/internal/source"
)

// Source is what one source gave on the last cycle. An unreachable source keeps the data
// of its last successful read, with the time of that read.
type Source[T any] struct {
	Reachable bool
	// Reason and Err are why the last read failed; empty when Reachable.
	Reason source.Reason
	Err    error
	// UnreachableSince is when the source stopped answering; zero when Reachable.
	UnreachableSince time.Time
	// Data is the result of the last successful read; the zero value when there was none.
	Data T
	// ObserveTime is when Data was read; zero when it never was.
	ObserveTime time.Time
}

// observe returns the source after a read at now that gave data or failed with err.
func observe[T any](prev Source[T], data T, err error, now time.Time) Source[T] {
	if err == nil {
		return Source[T]{Reachable: true, Data: data, ObserveTime: now}
	}

	next := prev
	next.Reachable = false
	next.Err = err
	next.Reason = reasonOf(err)

	if prev.Reachable || prev.UnreachableSince.IsZero() {
		next.UnreachableSince = now
	}

	return next
}

// reasonOf never returns an empty reason. The only error Classify leaves without one is
// context.Canceled, which a read gets when the cycle ends before it does: that is a
// timeout of the source, since a cycle canceled from outside is not published.
func reasonOf(err error) source.Reason {
	if reason, ok := source.ReasonOf(source.Classify("", err)); ok {
		return reason
	}

	return source.ReasonTimeout
}
