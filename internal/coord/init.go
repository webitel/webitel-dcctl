package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/webitel/webitel-dcctl/internal/kv"
)

// ErrInitialized means a voter holds dcctl/state, or may hold it as it does not
// answer: the first record is created only where no voter has one. Nothing is
// written.
var ErrInitialized = errors.New("state is initialized")

// Initialize creates s, the first record, on every KV voter: generation 1,
// revision 0, not forced. It is the initialization of the installation, and
// the only write that creates dcctl/state where no voter holds one.
//
// It is refused with [ErrInitialized] unless every KV answers without the key
// and the arbiter, when there is one, answers without a state: an arbiter
// that does not answer may hold a record from before, which would sit above
// or beside the new one once it is back. The arbiter is never written, only
// its own initialization fills it. A KV that holds s written by
// the same operation, as after an earlier run, counts as accepted; one that
// holds it with another payload refuses it ([ErrBaseChanged]), as two payloads
// of one revision would split the record. Each write is a create, so a key
// that appears meanwhile is not written over. The result tells whether the
// record is committed, as [Coordinator.Advance] does.
//
// updated_at of s is replaced with the time of the write.
func (c *Coordinator) Initialize(ctx context.Context, s kv.State) (WriteResult, error) {
	s.NeedsRecovery = canonicalRecovery(s.NeedsRecovery)
	s.UpdatedAt = time.Now().UTC()

	switch {
	case s.Generation != 1 || s.Revision != 0:
		return WriteResult{}, fmt.Errorf("the first record has generation 1 and revision 0, not %d and %d", s.Generation, s.Revision)
	case s.ActiveDatacenter == "" || s.OpID == "":
		return WriteResult{}, errors.New("the first record needs an active datacenter and an operation")
	case s.Forced:
		return WriteResult{}, errors.New("the first record is not forced")
	}

	if err := validateRecovery(s.NeedsRecovery, s.ActiveDatacenter); err != nil {
		return WriteResult{}, err
	}

	view, err := c.Read(ctx)
	if err != nil {
		return WriteResult{}, err
	}

	plans, err := initPlans(view, s)
	if err != nil {
		return WriteResult{}, err
	}

	res := WriteResult{Mode: WriteQuorum, Majority: view.Majority}
	res.Voters = c.execute(ctx, plans, func(held kv.State) bool { return samePayload(held, s) })
	res.Commitment = commitment(res.Voters, view, false)

	c.logWrite(ctx, "state initialized", s, res)

	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("initialize %s: %w", recordName(s), err)
	}

	return res, resultError(res, recordName(s))
}

// initPlans returns what [Coordinator.Initialize] does on every voter of view,
// or why nothing may be written.
func initPlans(view View, s kv.State) ([]voterPlan, error) {
	value, err := kv.EncodeState(s, nil)
	if err != nil {
		return nil, err
	}

	plans := make([]voterPlan, len(view.Voters))

	for i, vs := range view.Voters {
		switch {
		case vs.Status == StatusPresent && holdsRecordOf(vs.State, s) && vs.Kind == KindKV:
			if !samePayload(vs.State, s) {
				return nil, fmt.Errorf("voter %s holds %s of the operation with another payload: %w", vs.Name, recordName(vs.State),
					ErrBaseChanged)
			}

			plans[i] = voterPlan{held: true}
		case vs.Status == StatusPresent, vs.Status == StatusInvalid:
			return nil, fmt.Errorf("voter %s holds %s: %w", vs.Name, kv.StateKey, ErrInitialized)
		case vs.Kind != KindKV && (vs.Status == StatusUninitialized || vs.Status == StatusAbsent):
			plans[i] = skipped(vs)
		case vs.Kind != KindKV:
			return nil, fmt.Errorf("arbiter %s is %s and may hold %s from before; bring the arbiter back, or clear its data_dir: %w",
				vs.Name, vs.Status, kv.StateKey, ErrInitialized)
		case vs.Status == StatusAbsent:
			plans[i] = voterPlan{value: value}
		default:
			return nil, fmt.Errorf("voter %s is %s and may hold %s: %w", vs.Name, vs.Status, kv.StateKey, ErrInitialized)
		}
	}

	return plans, nil
}
