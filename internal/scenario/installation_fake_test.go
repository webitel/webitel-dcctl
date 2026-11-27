package scenario_test

import (
	"context"
	"slices"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/pgctl"
	"github.com/webitel/webitel-dcctl/internal/preflight"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// installationChecks decides apps sync, resolve, init and the arbiter initialization as
// their sets do, on the voters and the infrastructure of the world:
//
//   - apps sync: dcctl/state is there, no conflict, no record above the effective one
//     that is not committed, and the active datacenter, which the record tells, runs the
//     primary;
//   - resolve: there is a conflict, a record of it names the winner, and the isolation of
//     the other side is confirmed;
//   - init: no KV holds dcctl/state and every KV answers, the arbiter answers without
//     one, and the primary runs only in the datacenter to make active, every cluster
//     read;
//   - arbiter init: the arbiter holds no state and every KV holds one payload.
func (f fakePreflight) installationChecks(ctx context.Context, req preflight.Request) ([]check.Check, error) {
	view, err := f.view(ctx)
	if err != nil {
		return nil, err
	}

	dc := req.Params.Datacenter

	//exhaustive:ignore // the other actions are decided elsewhere
	switch req.Action {
	case preflight.AppsSync:
		base := coord.Base(view, false)

		return []check.Check{
			blocking(check.CoordStatePresent, view.Initialized),
			blocking(check.CoordNoConflict, len(view.Conflict) == 0),
			blocking(check.CoordNoUncommitted, len(view.Uncommitted) == 0 && len(view.Undetermined) == 0),
			blocking(check.PatroniActivePrimary, base != nil && f.primaryOnlyIn(base.State.ActiveDatacenter)),
		}, nil
	case preflight.Resolve:
		return []check.Check{
			blocking(check.ResolveConflict, len(view.Conflict) > 0),
			blocking(check.ResolveWinner, slices.ContainsFunc(view.Conflict, func(r coord.Record) bool { return r.State.ActiveDatacenter == dc })),
			isolationCheck(),
		}, nil
	case preflight.Init:
		absent, reachable := true, true

		for _, vs := range view.Voters {
			switch vs.Kind {
			case coord.KindKV:
				absent = absent && vs.Status == coord.StatusAbsent
				reachable = reachable && !vs.Status.Unknown()
			case coord.KindArbiter:
				absent = absent && (vs.Status == coord.StatusAbsent || vs.Status == coord.StatusUninitialized)
			}
		}

		return []check.Check{
			blocking(check.InitStateAbsent, absent), blocking(check.InitAllReachable, reachable),
			blocking(check.InitActiveHasPrimary, f.primaryOnlyIn(dc)),
		}, nil
	case preflight.ArbiterInit:
		empty, equal := false, true

		var seed *coord.VoterState

		for i, vs := range view.Voters {
			switch {
			case vs.Kind == coord.KindArbiter:
				empty = vs.Status == coord.StatusAbsent || vs.Status == coord.StatusUninitialized
			case vs.Status != coord.StatusPresent:
				equal = false
			case seed == nil:
				seed = &view.Voters[i]
			default:
				equal = equal && samePayload(seed.State, vs.State)
			}
		}

		return []check.Check{
			blocking(check.CoordNoOperation, true), blocking(check.ArbiterEmpty, empty), blocking(check.ArbiterStatesEqual, equal),
		}, nil
	}

	return nil, nil
}

func blocking(id check.ID, pass bool) check.Check {
	c := check.Check{ID: id, Result: check.Pass, Effect: check.Block}
	if !pass {
		c.Result = check.Fail
	}

	return c
}

// primaryOnlyIn reports whether dc runs the one primary, every cluster read: in stretch
// the leader of the cluster runs in dc.
func (f fakePreflight) primaryOnlyIn(dc string) bool {
	if f.stretch {
		p, err := fakePatroni{f.w}.Place(context.Background(), pgctl.Cluster{Datacenter: dc}, f.w.names())

		return err == nil && p.Leader != nil && p.Leader.Datacenter == dc
	}

	for _, name := range f.w.names() {
		d := f.w.dc(name)
		if d.unread || (d.role == patroni.RolePrimary) != (name == dc) {
			return false
		}
	}

	return true
}

// Create creates the record of key when v holds none, as an arbiter does on its
// initialization; createErr fails it.
func (v *fakeVoter) Create(ctx context.Context, key coord.Key, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	switch {
	case v.getErr != nil:
		return v.getErr
	case v.createErr != nil:
		return v.createErr
	case v.entries[key] != nil:
		return coord.ErrVersionMismatch
	}

	v.store(key, value)

	return nil
}

// samePayload compares the fields of two records that decisions are based on.
func samePayload(a, b kv.State) bool {
	return a.Generation == b.Generation && a.Revision == b.Revision && a.ActiveDatacenter == b.ActiveDatacenter &&
		a.OpID == b.OpID && a.Forced == b.Forced && a.PatroniTimeline == b.PatroniTimeline &&
		slices.Equal(a.NeedsRecovery, b.NeedsRecovery)
}
