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
	// Failed are the checks that block the action: failed ones no confirmation can
	// accept, and those with a result Decide does not know.
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

// Decide tells what checks allow with the given confirmations. It fails closed: a
// check with a result it does not know blocks, and so does a failed check unless its
// effect is [Confirm] with a known confirmation. A [ConfirmMaxDiscardBytes] check that
// does not know the amount blocks as well, since no bound can cover it; otherwise a
// confirm check is covered by its confirmation, a discard one when the bound is not
// smaller than the amount.
func Decide(checks []Check, given Confirmations) Verdict {
	v := Verdict{Checks: checks}

	for _, c := range checks {
		switch {
		case c.Result == Pass || c.Result == Skip:
		case c.Result == Warn:
			v.Warnings = append(v.Warnings, c)
		case c.Result != Fail || !confirmable(c):
			v.Failed = append(v.Failed, c)
		case !covered(c, given, &v.Confirmed):
			v.Unconfirmed = append(v.Unconfirmed, c)
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

// confirmable reports whether some confirmation can accept the failed check c.
func confirmable(c Check) bool {
	if c.Effect != Confirm {
		return false
	}

	switch c.Confirmation {
	case ConfirmIsolation, ConfirmNotMaxLSN:
		return true
	case ConfirmMaxDiscardBytes:
		return c.DiscardBytes != nil
	}

	return false
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
		if c.DiscardBytes == nil || given.MaxDiscardBytes < *c.DiscardBytes {
			return false
		}

		used.MaxDiscardBytes = given.MaxDiscardBytes

		return true
	}

	return false
}
