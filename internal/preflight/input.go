// Package preflight evaluates the checks dcctl runs before an action (switch, failover,
// demote and the others) and the checks of verify. Every check is a function of a
// snapshot collected for the action alone and of the parameters of the action, so the
// dry-run, the confirmation dialog and the start of the operation see the same checks.
//
// The checks fail closed. A condition whose source does not answer is not met, and
// the last known data of a source never makes a check pass; only the facts of
// coord.isolation show it, marked as such. A check is skipped only when it does not
// apply to the situation.
package preflight

import (
	"slices"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// Action is what the checks are run for.
type Action string

const (
	Switch        Action = "switch"
	Failover      Action = "failover"
	Demote        Action = "demote"
	Repoint       Action = "repoint"
	MarkRecovered Action = "mark_recovered"
	Resume        Action = "resume"
	Abandon       Action = "abandon"
	Resolve       Action = "resolve"
	Init          Action = "init"
	ArbiterInit   Action = "arbiter_init"
	AppsSync      Action = "apps_sync"
	// Verify is GET /verify and the last step of a switch: it checks the result, not a
	// precondition.
	Verify Action = "verify"
)

// Params are the parameters of an action the checks depend on.
type Params struct {
	// Datacenter is the target of switch and failover, the datacenter demote, repoint
	// and mark-recovered act on, the winner of resolve and the active datacenter of init.
	Datacenter string
	// Operation is the ID of the operation resume and abandon act on.
	Operation string
	// ToForced is set for a resume that writes the generation forced, without a
	// majority of voters.
	ToForced bool
}

// Plan is the dry-run of registering a managed job with Stop=false.
type Plan struct {
	Result nomad.PlanResult
	Err    error
}

// Input is what the checks of one action are evaluated on.
type Input struct {
	Action Action
	Params Params
	// Snapshot is collected for the action; Status is built from it.
	Snapshot *collect.Snapshot
	Status   status.Status
	Config   status.Config
	// Plans are the plans of the managed jobs in the datacenters the action starts
	// apps in, by datacenter and job ID. A job without a plan fails its check.
	Plans map[string]map[string]Plan
	// LastKnown is the last snapshot of the collection loop, nil when there is none. It
	// only gives the facts of coord.isolation about the old primary: what dcctl saw of
	// it before it stopped answering.
	LastKnown *collect.Snapshot
}

func (in *Input) warm() bool {
	return in.Snapshot.Profile == config.ProfileWarmStandby
}

// reference is the state the action starts from: the effective state of the voters
// when it tells the active datacenter. A failover starts from the highest record that
// unreachable voters may have committed, as after a generation that reached only some
// of them, when there is one: it is above the effective record. Records of that
// generation that name different datacenters give no state to start from.
func (in *Input) reference() *kv.State {
	if in.Action == Failover {
		if top, ok := in.undeterminedTop(); top != nil {
			if !ok {
				return nil
			}

			return &top.State
		}
	}

	return in.Status.Effective
}

// undeterminedTop returns the highest record that unreachable voters may have
// committed, nil when there is none, and false when the records of its generation name
// different datacenters.
func (in *Input) undeterminedTop() (*coord.Record, bool) {
	u := in.Status.View.Undetermined
	if len(u) == 0 {
		return nil, true
	}

	for _, r := range u[1:] {
		if r.State.Generation == u[0].State.Generation && r.State.ActiveDatacenter != u[0].State.ActiveDatacenter {
			return &u[0], false
		}
	}

	return &u[0], true
}

// active returns the active datacenter of [Input.reference]; false also when that is
// not a configured datacenter, so nothing is judged against a datacenter dcctl does not
// read.
func (in *Input) active() (string, bool) {
	ref := in.reference()
	if ref == nil || !in.configured(ref.ActiveDatacenter) {
		return "", false
	}

	return ref.ActiveDatacenter, true
}

func (in *Input) configured(dc string) bool {
	return dc != "" && slices.ContainsFunc(in.Snapshot.Datacenters, func(d collect.Datacenter) bool { return d.Name == dc })
}

func (in *Input) datacenter(dc string) (*status.Datacenter, bool) {
	i := slices.IndexFunc(in.Status.Datacenters, func(d status.Datacenter) bool { return d.Name == dc })
	if i < 0 {
		return nil, false
	}

	return &in.Status.Datacenters[i], true
}
