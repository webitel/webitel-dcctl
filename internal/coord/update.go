package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

// SetTimeline writes timeline as patroni_timeline of rec, the effective
// record, within its generation: on the payload the voters hold now, with
// revision one above it, updated_at the time of the write, and forced, op_id
// and the fields this dcctl does not know kept. rec must be the effective
// record with a settled payload, and the highest generation must not be in
// conflict; otherwise nothing is written.
//
// The voters that hold rec at any revision and the stale ones are written,
// each by a CAS at the version just read; a KV without the key gets it
// created, an arbiter never. A record that is not forced needs a majority
// for the update to be committed; a forced one needs a voter. An update on
// fewer voters is still the newest payload while they answer, and
// [Coordinator.Align] copies it to the rest; it comes with [ErrUncommitted]
// or [ErrUndetermined], as it is lost if those voters are lost first.
func (c *Coordinator) SetTimeline(ctx context.Context, rec RecordKey, timeline int64) (WriteResult, error) {
	if timeline < 1 {
		return WriteResult{}, fmt.Errorf("patroni timeline %d must be positive", timeline)
	}

	return c.update(ctx, rec, func(s *kv.State) { s.PatroniTimeline = timeline })
}

// AddNeedsRecovery adds dc to needs_recovery of rec, as
// [Coordinator.SetTimeline] writes the timeline. The change applies to the
// list the voters hold, so it keeps what other callers changed and adding a
// datacenter that is there already writes the same list. The active
// datacenter of rec cannot wait for recovery.
func (c *Coordinator) AddNeedsRecovery(ctx context.Context, rec RecordKey, dc string) (WriteResult, error) {
	if err := validateRecovery([]string{dc}, rec.ActiveDatacenter); err != nil {
		return WriteResult{}, err
	}

	return c.update(ctx, rec, func(s *kv.State) { s.NeedsRecovery = append(slices.Clone(s.NeedsRecovery), dc) })
}

// RemoveNeedsRecovery removes dc from needs_recovery of rec, as
// [Coordinator.AddNeedsRecovery] adds it; removing a datacenter that is not
// there writes the same list.
func (c *Coordinator) RemoveNeedsRecovery(ctx context.Context, rec RecordKey, dc string) (WriteResult, error) {
	if dc == "" {
		return WriteResult{}, errors.New("needs_recovery has no empty datacenter to remove")
	}

	return c.update(ctx, rec, func(s *kv.State) {
		s.NeedsRecovery = slices.DeleteFunc(slices.Clone(s.NeedsRecovery), func(d string) bool { return d == dc })
	})
}

// update applies change to the effective payload of rec and writes it, see
// [Coordinator.SetTimeline].
func (c *Coordinator) update(ctx context.Context, rec RecordKey, change func(*kv.State)) (WriteResult, error) {
	view, err := c.Read(ctx)
	if err != nil {
		return WriteResult{}, err
	}

	eff := view.Effective

	switch {
	case len(view.Conflict) > 0:
		return WriteResult{}, fmt.Errorf("generation %d: %w", view.Conflict[0].State.Generation, ErrConflict)
	case eff == nil || KeyOf(eff.State) != rec:
		return WriteResult{}, fmt.Errorf("%s generation %d of %s is not the effective record: %w",
			kv.StateKey, rec.Generation, rec.ActiveDatacenter, ErrBaseNotFound)
	case !eff.Settled:
		return WriteResult{}, fmt.Errorf("%s: %w", recordName(eff.State), ErrUnsettled)
	}

	next := eff.State
	change(&next)
	next.Revision++
	next.NeedsRecovery = canonicalRecovery(next.NeedsRecovery)
	next.UpdatedAt = time.Now().UTC()

	value, err := kv.EncodeState(next, eff.Raw)
	if err != nil {
		return WriteResult{}, err
	}

	plans := make([]voterPlan, len(view.Voters))

	for i, vs := range view.Voters {
		if vs.Stale || vs.Status == StatusPresent && KeyOf(vs.State) == rec {
			plans[i] = voterPlan{value: value, version: vs.Version}
		} else {
			plans[i] = skipped(vs)
		}
	}

	res := WriteResult{Mode: WriteQuorum, Majority: view.Majority}
	if next.Forced {
		res.Mode = WriteForced
	}

	res.Voters = c.execute(ctx, plans, func(s kv.State) bool { return samePayload(s, next) })
	res.Commitment = commitment(res.Voters, view, next.Forced)

	c.logWrite(ctx, "record updated", next, res)

	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("update %s: %w", recordName(next), err)
	}

	return res, resultError(res, fmt.Sprintf("%s revision %d", recordName(next), next.Revision))
}
