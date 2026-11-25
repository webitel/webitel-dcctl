// Package check is the result of a dcctl check: what preflight evaluates before an
// action, what verify reports and what the informational checks of the status show.
// It maps one to one to webitel.dcctl.v1.Check.
package check

// ID is the stable identifier of a check.
type ID string

// Result is the result of a check.
type Result string

const (
	Pass Result = "pass"
	// Fail is a condition that does not hold, or that cannot be told because its source
	// does not answer.
	Fail Result = "fail"
	// Warn is a condition that does not hold without blocking the action: the result of
	// every failure of a check with [WarnOnly].
	Warn Result = "warn"
	// Skip is a check that does not apply to the situation.
	Skip Result = "skip"
)

// Effect is what a failed check does to the action.
type Effect string

const (
	// EffectNone is the effect of informational and verify checks.
	EffectNone Effect = ""
	// Block rejects the action.
	Block Effect = "block"
	// WarnOnly reports the failure as [Warn].
	WarnOnly Effect = "warn"
	// Confirm requires the confirmation named by [Check.Confirmation].
	Confirm Effect = "confirm"
)

// Confirmation is the field of the confirmation that accepts a failed check.
type Confirmation string

const (
	// ConfirmIsolation states that the old primary is isolated.
	ConfirmIsolation Confirmation = "isolation"
	// ConfirmNotMaxLSN accepts a failover target without the largest received LSN.
	ConfirmNotMaxLSN Confirmation = "not_max_lsn"
	// ConfirmMaxDiscardBytes bounds the WAL the operator agrees to discard; the amount a
	// check needs covered is its fact [FactDiscardBytes].
	ConfirmMaxDiscardBytes Confirmation = "max_discard_bytes"
)

// FactDiscardBytes is the fact, an int64, with the WAL a [ConfirmMaxDiscardBytes] check
// would discard. A check without it does not know the amount, which no bound covers.
const FactDiscardBytes = "discard_bytes"

// Check is the result of one check.
type Check struct {
	ID     ID
	Result Result
	Effect Effect
	// Confirmation is set when Effect is [Confirm].
	Confirmation Confirmation
	// Datacenter is the datacenter the check is about, when it is about one.
	Datacenter string
	Detail     string
	// Facts are the data the result is based on. They hold only what
	// google.protobuf.Struct takes: strings, booleans, ints, maps of them and []any.
	Facts map[string]any
}

// DiscardBytes returns the fact [FactDiscardBytes]; false when the check does not know
// the amount.
func (c Check) DiscardBytes() (int64, bool) {
	n, ok := c.Facts[FactDiscardBytes].(int64)

	return n, ok
}
