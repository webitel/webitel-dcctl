package check_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/check"
)

func pass(id check.ID) check.Check {
	return check.Check{ID: id, Result: check.Pass, Effect: check.Block}
}

func blockFail(id check.ID) check.Check {
	return check.Check{ID: id, Result: check.Fail, Effect: check.Block}
}

func confirm(id check.ID, c check.Confirmation, facts map[string]any) check.Check {
	return check.Check{ID: id, Result: check.Fail, Effect: check.Confirm, Confirmation: c, Facts: facts}
}

func discard(n int64) map[string]any { return map[string]any{check.FactDiscardBytes: n} }

func TestDecide(t *testing.T) {
	isolation := confirm(check.CoordIsolation, check.ConfirmIsolation, nil)
	notMax := confirm(check.PatroniTargetMaxLSN, check.ConfirmNotMaxLSN, nil)
	diverged := confirm(check.PatroniDivergence, check.ConfirmMaxDiscardBytes, discard(100))
	unknown := confirm(check.PatroniDivergence, check.ConfirmMaxDiscardBytes, map[string]any{"known": false})
	warned := check.Check{ID: check.PatroniOthersFollowSource, Result: check.Warn, Effect: check.WarnOnly}
	skipped := check.Check{ID: check.PatroniTargetCandidate, Result: check.Skip, Effect: check.Block}
	lockBusy := blockFail(check.CoordLockFree)

	tests := []struct {
		name   string
		checks []check.Check
		given  check.Confirmations
		want   check.Verdict
	}{
		{
			name:   "all_pass",
			checks: []check.Check{pass(check.CoordStatePresent), skipped},
			want:   check.Verdict{Outcome: check.Allowed},
		},
		{
			name:   "block_wins_over_confirmation",
			checks: []check.Check{lockBusy, isolation},
			want: check.Verdict{
				Outcome: check.Blocked, Failed: []check.Check{lockBusy}, Unconfirmed: []check.Check{isolation},
			},
		},
		{
			name:   "isolation_missing",
			checks: []check.Check{isolation, notMax},
			given:  check.Confirmations{NotMaxLSN: true},
			want: check.Verdict{
				Outcome: check.NeedsConfirmation, Unconfirmed: []check.Check{isolation},
			},
		},
		{
			name:   "isolation_given",
			checks: []check.Check{isolation},
			given:  check.Confirmations{Isolation: true, NotMaxLSN: true, MaxDiscardBytes: 7},
			want:   check.Verdict{Outcome: check.Allowed, Confirmed: check.Confirmations{Isolation: true}},
		},
		{
			name:   "bound_equals_discard",
			checks: []check.Check{diverged},
			given:  check.Confirmations{MaxDiscardBytes: 100},
			want:   check.Verdict{Outcome: check.Allowed, Confirmed: check.Confirmations{MaxDiscardBytes: 100}},
		},
		{
			name:   "bound_below_discard",
			checks: []check.Check{diverged},
			given:  check.Confirmations{MaxDiscardBytes: 99},
			want:   check.Verdict{Outcome: check.NeedsConfirmation, Unconfirmed: []check.Check{diverged}},
		},
		{
			name:   "unknown_discard_is_never_covered",
			checks: []check.Check{unknown},
			given:  check.Confirmations{MaxDiscardBytes: 1 << 62},
			want:   check.Verdict{Outcome: check.NeedsConfirmation, Unconfirmed: []check.Check{unknown}},
		},
		{
			name:   "warn_does_not_block",
			checks: []check.Check{warned, pass(check.CoordQuorum)},
			want:   check.Verdict{Outcome: check.Allowed, Warnings: []check.Check{warned}},
		},
		{
			name:   "fail_of_informational_check_blocks",
			checks: []check.Check{{ID: check.VerifyState, Result: check.Fail}},
			want: check.Verdict{
				Outcome: check.Blocked, Failed: []check.Check{{ID: check.VerifyState, Result: check.Fail}},
			},
		},
		{
			name:   "confirm_without_field_is_never_covered",
			checks: []check.Check{{ID: check.CoordIsolation, Result: check.Fail, Effect: check.Confirm}},
			given:  check.Confirmations{Isolation: true, NotMaxLSN: true, MaxDiscardBytes: 1 << 62},
			want: check.Verdict{
				Outcome:     check.NeedsConfirmation,
				Unconfirmed: []check.Check{{ID: check.CoordIsolation, Result: check.Fail, Effect: check.Confirm}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := check.Decide(tt.checks, tt.given)

			tt.want.Checks = tt.checks
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecide_wrongTypeOfDiscardIsNotCovered(t *testing.T) {
	c := confirm(check.PatroniCandidateLoss, check.ConfirmMaxDiscardBytes, map[string]any{check.FactDiscardBytes: "100"})

	got := check.Decide([]check.Check{c}, check.Confirmations{MaxDiscardBytes: 1000})

	assert.Equal(t, check.NeedsConfirmation, got.Outcome)
}

func TestCheck_discardBytes(t *testing.T) {
	n, ok := confirm(check.PatroniDivergence, check.ConfirmMaxDiscardBytes, discard(42)).DiscardBytes()
	assert.True(t, ok)
	assert.Equal(t, int64(42), n)

	_, ok = check.Check{}.DiscardBytes()
	assert.False(t, ok)
}
