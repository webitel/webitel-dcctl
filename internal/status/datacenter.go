package status

import (
	"slices"
	"time"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/source"
)

// State is the role of a datacenter in coordination: the first rule that holds.
type State string

const (
	// StateUnreachable is a datacenter none of whose sources answers.
	StateUnreachable State = "unreachable"
	// StateUninitialized is an installation where every voter answers and none holds
	// dcctl/state.
	StateUninitialized State = "uninitialized"
	StateActive        State = "active"
	// StateSplitBrain is an inactive datacenter with a managed job not stopped.
	StateSplitBrain State = "split_brain"
	// StateDBSplit is an inactive datacenter of warm_standby whose Patroni leader is a
	// primary.
	StateDBSplit State = "db_split"
	// StateStale is a datacenter whose voter holds an older generation, or answered
	// without dcctl/state while it exists elsewhere: alignment has yet to write it.
	StateStale         State = "stale"
	StateNeedsRecovery State = "needs_recovery"
	StateStandby       State = "standby"
	// StateUndetermined is a datacenter whose role cannot be told: the voters give no
	// active datacenter, as in a conflict, without a majority that answers, when a
	// record that unreachable voters may hold names another one, or when they may hold
	// a state that no voter shows.
	StateUndetermined State = "undetermined"
)

// System is the system a source belongs to.
type System string

const (
	SystemConsul  System = "consul"
	SystemNomad   System = "nomad"
	SystemPatroni System = "patroni"
)

// Source is whether a source of the datacenter answers.
type Source struct {
	System    System
	Reachable bool
	// Reason is why the source does not answer.
	Reason source.Reason
	// UnreachableSince is when the source stopped answering; zero when it answers or it
	// is not known.
	UnreachableSince time.Time
	// ObserveTime is when the data of the source was read.
	ObserveTime time.Time
}

func sourceOf[T any](sys System, s collect.Source[T]) Source {
	return Source{
		System: sys, Reachable: s.Reachable, Reason: s.Reason,
		UnreachableSince: s.UnreachableSince, ObserveTime: s.ObserveTime,
	}
}

// Datacenter is one datacenter as the observer sees it.
type Datacenter struct {
	Name  string
	State State
	// Skipped are the rules of a higher priority than State that were not checked for
	// lack of a source, in the order they are checked.
	Skipped       []State
	Health        Health
	HealthReasons []HealthReason
	// NeedsRecovery is set when the datacenter is in needs_recovery of the effective
	// state, even when State shows a rule of a higher priority.
	NeedsRecovery bool
	// Sources are Consul, Nomad and Patroni, in this order.
	Sources []Source
	Patroni Patroni
	// Nomad is built from the last data read, see the Nomad source for its age.
	Nomad      Nomad
	InfoChecks []check.Check
}

// dcFacts is what the state and health of a datacenter are derived from.
type dcFacts struct {
	name     string
	consul   Source
	nomad    Source
	patroni  dcPatroni
	jobs     JobCounts
	recovery bool
}

func (b *builder) datacenter(d collect.Datacenter) Datacenter {
	f := dcFacts{
		name:    d.Name,
		consul:  sourceOf(SystemConsul, d.Services),
		nomad:   sourceOf(SystemNomad, d.Nomad),
		patroni: b.patroniIn(d.Name),
	}

	if b.effective != nil {
		f.recovery = slices.Contains(b.effective.NeedsRecovery, d.Name)
	}

	out := Datacenter{
		Name:          d.Name,
		NeedsRecovery: f.recovery,
		Sources:       []Source{f.consul, f.nomad, f.patroni.src},
		Patroni:       b.patroni(d.Name, f.patroni),
		Nomad:         nomadOf(d.Nomad.Data, b.now, b.cfg.MinHealthyTime),
		InfoChecks:    b.infoChecks(d),
	}

	f.jobs = out.Nomad.JobCounts
	out.State, out.Skipped = b.state(f)
	out.Health, out.HealthReasons = b.health(f, out.State, out.Patroni)

	return out
}

// state applies the rules in their order. A rule whose source does not answer is
// skipped and listed; a rule that does not apply to the profile is not.
func (b *builder) state(f dcFacts) (State, []State) {
	if !f.consul.Reachable && !f.nomad.Reachable && !f.patroni.src.Reachable {
		return StateUnreachable, nil
	}

	var skipped []State

	if b.effective == nil {
		if !b.initialized {
			if !slices.ContainsFunc(b.view.Voters, func(vs coord.VoterState) bool {
				return vs.Status == coord.StatusUnreachable
			}) {
				return StateUninitialized, nil
			}

			skipped = append(skipped, StateUninitialized)
		}

		return StateUndetermined, skipped
	}

	if f.name == b.effective.ActiveDatacenter {
		return StateActive, nil
	}

	switch {
	case !f.nomad.Reachable:
		skipped = append(skipped, StateSplitBrain)
	case f.jobs.Stopped < f.jobs.Total():
		return StateSplitBrain, skipped
	}

	if b.warm() {
		switch {
		case !f.patroni.src.Reachable:
			skipped = append(skipped, StateDBSplit)
		case f.patroni.leader != nil && roleOf(f.patroni.leader.Role) == RolePrimary:
			return StateDBSplit, skipped
		}

		if i := slices.IndexFunc(b.view.Voters, func(vs coord.VoterState) bool {
			return vs.Kind == coord.KindKV && vs.Name == f.name
		}); i >= 0 {
			switch vs := b.view.Voters[i]; {
			case vs.Status == coord.StatusUnreachable, vs.Status == coord.StatusInvalid:
				skipped = append(skipped, StateStale)
			case vs.Stale:
				return StateStale, skipped
			}
		}
	}

	if f.recovery {
		return StateNeedsRecovery, skipped
	}

	return StateStandby, skipped
}
