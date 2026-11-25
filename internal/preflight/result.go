package preflight

import (
	"fmt"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// spec is a check before it has a result.
type spec struct {
	id           check.ID
	effect       check.Effect
	confirmation check.Confirmation
	datacenter   string
}

func block(id check.ID, dc string) spec { return spec{id: id, effect: check.Block, datacenter: dc} }

func warnOnly(id check.ID, dc string) spec {
	return spec{id: id, effect: check.WarnOnly, datacenter: dc}
}

func confirm(id check.ID, c check.Confirmation, dc string) spec {
	return spec{id: id, effect: check.Confirm, confirmation: c, datacenter: dc}
}

// info is a verify check, which has no effect on an action.
func info(id check.ID, dc string) spec { return spec{id: id, datacenter: dc} }

func (s spec) with(r check.Result, detail string, facts map[string]any) check.Check {
	return check.Check{
		ID: s.id, Result: r, Effect: s.effect, Confirmation: s.confirmation,
		Datacenter: s.datacenter, Detail: detail, Facts: facts,
	}
}

func (s spec) pass(detail string, facts map[string]any) check.Check {
	return s.with(check.Pass, detail, facts)
}

// fail is a condition that does not hold; with [check.WarnOnly] it is a warning.
func (s spec) fail(detail string, facts map[string]any) check.Check {
	if s.effect == check.WarnOnly {
		return s.with(check.Warn, detail, facts)
	}

	return s.with(check.Fail, detail, facts)
}

// discard fails a [check.ConfirmMaxDiscardBytes] check that would discard n bytes.
func (s spec) discard(detail string, facts map[string]any, n int64) check.Check {
	c := s.with(check.Fail, detail, facts)
	c.DiscardBytes = &n

	return c
}

// warn does not block whatever the effect, as for an unreachable replica of a
// datacenter whose leader answers.
func (s spec) warn(detail string, facts map[string]any) check.Check {
	return s.with(check.Warn, detail, facts)
}

func (s spec) skip(detail string) check.Check {
	return s.with(check.Skip, detail, nil)
}

// unreachable fails the check because what it needs does not answer.
func (s spec) unreachable(what string, reason source.Reason) check.Check {
	return s.fail(fmt.Sprintf("%s does not answer: %s", what, reason),
		map[string]any{"reachable": false, "reason": string(reason)})
}

// list converts xs for facts, which take []any and not typed slices.
func list[T any](xs []T, fn func(T) any) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = fn(x)
	}

	return out
}

func strs(xs []string) []any {
	return list(xs, func(s string) any { return s })
}
