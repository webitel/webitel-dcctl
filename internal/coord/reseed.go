package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

var (
	// ErrReseedRefused means the voters do not allow reseeding the voter.
	// Nothing is written.
	ErrReseedRefused = errors.New("reseed refused")
	// ErrConfirmedRecordChanged means the voters hold another record, or
	// another payload of it, than the one the operator confirmed. Nothing
	// is written.
	ErrConfirmedRecordChanged = errors.New("voters hold another record than the one confirmed")
)

// Reseed creates dcctl/state on voter, a KV that answers without the key, as
// a copy of the record every other voter holds. It is the way back for 2
// datacenters without an arbiter where one Consul lost the key: the record
// is then on one voter of two, never effective, so [Coordinator.Align] does
// not copy it. The operator confirms the record to copy, as the copy makes
// it committed even if it was not: confirmed is that record as shown to
// them, compared by everything but updated_at. Without it the result is
// [ErrConfirmationRequired], and when the voters hold anything else,
// [ErrConfirmedRecordChanged].
//
// It is refused with [ErrReseedRefused] unless every other voter answers
// with a record it can be judged by (an arbiter without state does not
// vote and is allowed), the voters that hold dcctl/state all hold one
// record with a settled payload, and voter is a KV that answers without the
// key: an arbiter gets its records only from its initialization. The write
// is a create only if the key is still absent; the result tells what it
// did and comes with its error when the voter was not written.
func (c *Coordinator) Reseed(ctx context.Context, voter string, confirmed kv.State) (Alignment, error) {
	idx := slices.IndexFunc(c.voters, func(v Voter) bool { return v.Name() == voter })

	switch {
	case idx < 0:
		return Alignment{}, fmt.Errorf("unknown voter %q", voter)
	case c.voters[idx].Kind() != KindKV:
		return Alignment{}, fmt.Errorf("voter %s is not a KV, only its initialization creates its records: %w",
			voter, ErrReseedRefused)
	case confirmed.Generation < 1 || confirmed.ActiveDatacenter == "":
		return Alignment{}, fmt.Errorf("reseed %s of voter %s needs the record to copy confirmed: %w",
			kv.StateKey, voter, ErrConfirmationRequired)
	}

	view, err := c.Read(ctx)
	if err != nil {
		return Alignment{}, err
	}

	rec, err := reseedRecord(view, idx)
	if err != nil {
		return Alignment{}, err
	}

	if !samePayload(rec.State, confirmed) {
		return Alignment{}, fmt.Errorf("voters hold %s revision %d, confirmed %s revision %d: %w",
			recordName(rec.State), rec.State.Revision, recordName(confirmed), confirmed.Revision, ErrConfirmedRecordChanged)
	}

	casCtx, cancel := context.WithTimeout(ctx, c.timeout)
	err = c.voters[idx].CAS(casCtx, KeyState, rec.Raw, 0)

	cancel()

	a := Alignment{Voter: voter}
	a.Outcome, a.Err = alignOutcome(err)

	c.log.InfoCtx(ctx, "voter reseeded",
		logging.Str("voter", voter), logging.Int64("generation", rec.State.Generation),
		logging.Str("active_datacenter", rec.State.ActiveDatacenter), logging.Str("outcome", string(a.Outcome)))

	if a.Err != nil {
		return a, fmt.Errorf("reseed %s of voter %s: %w", kv.StateKey, voter, a.Err)
	}

	return a, nil
}

// reseedRecord returns the record to copy to the voter idx of view.
func reseedRecord(view View, idx int) (*Record, error) {
	target := view.Voters[idx]
	if target.Status != StatusAbsent {
		return nil, fmt.Errorf("voter %s is %s, not without the key: %w", target.Name, target.Status, ErrReseedRefused)
	}

	var held *kv.State

	for _, vs := range view.Voters {
		switch {
		case vs.Status.Unknown():
			return nil, fmt.Errorf("voter %s is %s and may hold another record: %w", vs.Name, vs.Status, ErrReseedRefused)
		case vs.Status != StatusPresent:
		case held == nil:
			held = &vs.State
		case !sameRecord(vs.State, *held):
			return nil, fmt.Errorf("voters hold %s and %s: %w", recordName(*held), recordName(vs.State), ErrReseedRefused)
		}
	}

	if held == nil {
		return nil, fmt.Errorf("no voter holds %s: %w", kv.StateKey, ErrReseedRefused)
	}

	rec := findRecord(*held, effectiveList(view), view.Uncommitted, view.Undetermined)

	switch {
	case rec == nil:
		return nil, fmt.Errorf("%s is not evaluated: %w", recordName(*held), ErrReseedRefused)
	case !rec.Settled:
		return nil, fmt.Errorf("%s: %w", recordName(*held), ErrUnsettled)
	}

	return rec, nil
}
