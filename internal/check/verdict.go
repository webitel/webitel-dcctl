package check

// Confirmations are the confirmations a request gives.
type Confirmations struct {
	Isolation bool
	NotMaxLSN bool
	// MaxDiscardBytes is the upper bound of WAL the operator agrees to discard.
	MaxDiscardBytes int64
}

// Outcome is what the checks allow.
type Outcome string

const (
	Allowed Outcome = "allowed"
	// Blocked is an action with a failed check that no confirmation accepts.
	Blocked Outcome = "blocked"
	// NeedsConfirmation is an action whose failed checks all have [Confirm], some of
	// them without the confirmation or with a smaller bound.
	NeedsConfirmation Outcome = "needs_confirmation"
)

// Verdict is the decision over the checks of an action.
type Verdict struct {
	Outcome Outcome
	// Checks are all checks of the action, in the order of its set.
	Checks []Check
	// Failed are the failed checks that block the action.
	Failed []Check
	// Unconfirmed are the failed [Confirm] checks the given confirmations do not cover.
	Unconfirmed []Check
	// Warnings are the checks with [Warn].
	Warnings []Check
	// Confirmed are the given confirmations that a failed check needed, set only when
	// the action is allowed; the others do not apply and are not recorded with the
	// operation.
	Confirmed Confirmations
}

// Decide tells what checks allow with the given confirmations. A failed check blocks
// unless its effect is [Confirm]; such a check is covered by its confirmation, a
// [ConfirmMaxDiscardBytes] one only when it knows the amount and the bound is not
// smaller.
func Decide(checks []Check, given Confirmations) Verdict {
	v := Verdict{Checks: checks}

	for _, c := range checks {
		switch c.Result {
		case Warn:
			v.Warnings = append(v.Warnings, c)
		case Fail:
			if c.Effect != Confirm {
				v.Failed = append(v.Failed, c)

				continue
			}

			if !covered(c, given, &v.Confirmed) {
				v.Unconfirmed = append(v.Unconfirmed, c)
			}
		case Pass, Skip:
		}
	}

	switch {
	case len(v.Failed) > 0:
		v.Outcome = Blocked
	case len(v.Unconfirmed) > 0:
		v.Outcome = NeedsConfirmation
	default:
		v.Outcome = Allowed
	}

	if len(v.Unconfirmed) > 0 || len(v.Failed) > 0 {
		// Nothing is recorded for an action that does not start.
		v.Confirmed = Confirmations{}
	}

	return v
}

// covered reports whether given covers c and records in used what it took.
func covered(c Check, given Confirmations, used *Confirmations) bool {
	switch c.Confirmation {
	case ConfirmIsolation:
		used.Isolation = used.Isolation || given.Isolation

		return given.Isolation
	case ConfirmNotMaxLSN:
		used.NotMaxLSN = used.NotMaxLSN || given.NotMaxLSN

		return given.NotMaxLSN
	case ConfirmMaxDiscardBytes:
		n, ok := c.DiscardBytes()
		if !ok || given.MaxDiscardBytes < n {
			return false
		}

		used.MaxDiscardBytes = given.MaxDiscardBytes

		return true
	}

	return false
}
