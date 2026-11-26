package operation

import (
	"context"
	"fmt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
)

// maintain runs one round of [Engine.Run].
func (e *Engine) maintain(ctx context.Context) {
	e.sweep(ctx)
	e.alignOperations(ctx)
	e.alignVoters(ctx)
}

// alignOperations copies the final record of an operation to the copies of
// dcctl/op that still hold it unfinished and journals each copy written or
// whose write has an unknown outcome.
func (e *Engine) alignOperations(ctx context.Context) {
	view, err := e.cfg.Ops.Read(ctx)
	if err != nil {
		return
	}

	aligned, _ := e.cfg.Ops.Align(ctx)

	for _, a := range aligned {
		name := ""
		if c := copyOf(view, a.Voter); c != nil {
			name = c.Operation.GetName()
		}

		switch a.Outcome {
		case coord.AlignWritten:
			e.journalSystem(ctx, dcctlv1.JournalEntry_ALIGN_OPERATION, name, a.Voter, dcctlv1.JournalEntry_OK,
				"the final record of the operation is copied to "+a.Voter, nil)
		case coord.AlignUnknown:
			e.journalSystem(ctx, dcctlv1.JournalEntry_OUTCOME_UNKNOWN, name, a.Voter, dcctlv1.JournalEntry_WARNING,
				"aligning the operation record may have been applied: "+a.Err.Error(),
				map[string]any{"key": kv.OpKey})
		case coord.AlignSkipped, coord.AlignMismatch, coord.AlignFailed:
		}
	}
}

// alignVoters copies the effective state to the voters that are stale or
// behind it and journals each voter written or whose write has an unknown
// outcome.
func (e *Engine) alignVoters(ctx context.Context) {
	view, err := e.cfg.Coordinator.Read(ctx)
	if err != nil || view.Effective == nil {
		return
	}

	aligned, _ := e.cfg.Coordinator.Align(ctx, view)
	eff := view.Effective.State

	for _, a := range aligned {
		params := map[string]any{
			"voter": a.Voter, "generation": eff.Generation, "revision": eff.Revision, "active_datacenter": eff.ActiveDatacenter,
		}

		if vs := voterOf(view, a.Voter); vs != nil {
			params["behind"] = vs.Behind
			params["generation_from"] = vs.State.Generation
			params["revision_from"] = vs.State.Revision
		}

		switch a.Outcome {
		case coord.AlignWritten:
			e.journalSystem(ctx, dcctlv1.JournalEntry_ALIGN_GENERATION, "", "", dcctlv1.JournalEntry_OK,
				fmt.Sprintf("voter %s aligned to generation %d revision %d", a.Voter, eff.Generation, eff.Revision), params)
		case coord.AlignUnknown:
			e.journalSystem(ctx, dcctlv1.JournalEntry_ALIGN_GENERATION, "", "", dcctlv1.JournalEntry_WARNING,
				fmt.Sprintf("aligning voter %s may have been applied: %v", a.Voter, a.Err), params)
		case coord.AlignSkipped, coord.AlignMismatch, coord.AlignFailed:
		}
	}
}

func copyOf(view coord.OpView, name string) *coord.OpCopy {
	for i := range view.Copies {
		if c := &view.Copies[i]; c.Name == name && c.Status == coord.StatusPresent {
			return c
		}
	}

	return nil
}

func voterOf(view coord.View, name string) *coord.VoterState {
	for i := range view.Voters {
		if view.Voters[i].Name == name {
			return &view.Voters[i]
		}
	}

	return nil
}
