package preflight

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// passing is the Consul check status of a healthy instance.
const passing = "passing"

// operation returns the operation of [Params.Operation] as the latest updated copy
// shows it; nil when no copy that answered holds it.
func (in *Input) operation() *dcctlv1.Operation {
	name := "operations/" + in.Params.Operation

	var out *dcctlv1.Operation

	for _, c := range status.Operations(in.Snapshot).Copies {
		if c.Status != coord.StatusPresent || c.Operation.GetName() != name {
			continue
		}

		if out == nil || c.Operation.GetUpdateTime().AsTime().After(out.GetUpdateTime().AsTime()) {
			out = c.Operation
		}
	}

	return out
}

// datacenterID returns the datacenter of a name that may be the resource name
// datacenters/{datacenter}.
func datacenterID(name string) string {
	return strings.TrimPrefix(name, "datacenters/")
}

// opInterrupted fails unless the operation is interrupted. Its facts name the operation
// and its state, the enum name; found is false only when every KV answered without it.
func (in *Input) opInterrupted() check.Check {
	c := block(check.OpInterrupted, "")

	ok, unreachable := in.kvAnswers()
	if !ok {
		return c.fail("no Consul KV answers", map[string]any{"unreachable": unreachable})
	}

	name := "operations/" + in.Params.Operation

	for _, cp := range status.Operations(in.Snapshot).Copies {
		if cp.Status == coord.StatusPresent && cp.Operation.GetName() == name && finished(cp.Operation.GetState()) {
			st := cp.Operation.GetState()

			return c.fail(name+" is "+stateName(st),
				map[string]any{"operation": name, "state": st.String(), "kv": cp.Name})
		}
	}

	op := in.operation()
	if op == nil {
		if len(unreachable) > 0 {
			return c.fail(name+" is not found in the Consul KVs that answer",
				map[string]any{"operation": name, "unreachable": unreachable})
		}

		return c.fail(name+" is not found", map[string]any{"operation": name, "found": false})
	}

	facts := map[string]any{"operation": name, "state": op.GetState().String()}

	if op.GetState() != dcctlv1.Operation_INTERRUPTED {
		return c.fail(name+" is "+stateName(op.GetState()), facts)
	}

	return c.pass(name+" is interrupted", facts)
}

func finished(s dcctlv1.Operation_State) bool {
	return s == dcctlv1.Operation_SUCCEEDED || s == dcctlv1.Operation_FAILED || s == dcctlv1.Operation_ABANDONED
}

// pointOfNoReturn warns when the operation may have passed its point of no return:
// abandoning it then leaves the datacenters as the steps left them. Every step that
// started counts, also one that failed or is in a state this dcctl does not know.
func (in *Input) pointOfNoReturn() check.Check {
	c := warnOnly(check.OpPointOfNoReturn, "")

	op := in.operation()
	if op == nil {
		return c.fail("the operation is not found", nil)
	}

	var started []any

	passed := false

	for _, st := range op.GetSteps() {
		if st.GetState() == dcctlv1.Operation_Step_PENDING || st.GetState() == dcctlv1.Operation_Step_SKIPPED {
			continue
		}

		started = append(started, map[string]any{"type": st.GetType().String(), "state": st.GetState().String()})
		passed = passed || st.GetPointOfNoReturn()
	}

	facts := map[string]any{"steps": started}

	if passed {
		return c.fail("the operation may have passed its point of no return", facts)
	}

	return c.pass("the point of no return is not passed", facts)
}

func (in *Input) resolveConflict() check.Check {
	c := block(check.ResolveConflict, "")

	if in.noVoterAnswers() {
		return c.fail("no voter answers", map[string]any{"voters": in.votersFacts()})
	}

	if cf := in.Status.View.Conflict; len(cf) > 0 {
		return c.pass(fmt.Sprintf("generation %d has %d conflicting records", cf[0].State.Generation, len(cf)),
			map[string]any{"records": recordFacts(cf)})
	}

	return c.fail("there is no conflict", nil)
}

func (in *Input) resolveWinner() check.Check {
	w := in.Params.Datacenter
	c := block(check.ResolveWinner, w)
	cf := in.Status.View.Conflict

	roles := make(map[string]any)

	for _, r := range cf {
		dc := r.State.ActiveDatacenter
		if d, ok := in.datacenter(dc); ok && d.Source(status.SystemPatroni).Reachable {
			roles[dc] = string(d.Patroni.Role)
		} else {
			roles[dc] = "unknown"
		}
	}

	facts := map[string]any{"records": recordFacts(cf), "roles": roles}

	if !slices.ContainsFunc(cf, func(r coord.Record) bool { return r.State.ActiveDatacenter == w }) {
		return c.fail(fmt.Sprintf("%q is not the active datacenter of a conflicting record", w), facts)
	}

	return c.pass(w+" is the active datacenter of a conflicting record", facts)
}

// kvVoters returns the voters that are a Consul KV.
func (in *Input) kvVoters() []status.Voter {
	var out []status.Voter

	for _, v := range in.Status.Voters {
		if v.Type == status.VoterDatacenter {
			out = append(out, v)
		}
	}

	return out
}

func (in *Input) initStateAbsent() check.Check {
	c := block(check.InitStateAbsent, "")
	facts := map[string]any{"voters": in.votersFacts()}

	for _, v := range in.kvVoters() {
		switch v.Status {
		case coord.StatusAbsent:
		case coord.StatusUnreachable, coord.StatusStale:
			return c.fail("Consul KV of "+v.Name+" does not answer, it may hold dcctl/state", facts)
		case coord.StatusPresent, coord.StatusInvalid, coord.StatusUninitialized:
			return c.fail("Consul KV of "+v.Name+" holds dcctl/state", facts)
		}
	}

	return c.pass("no Consul KV holds dcctl/state", facts)
}

func (in *Input) initAllReachable() check.Check {
	c := block(check.InitAllReachable, "")

	var down []string

	for _, v := range in.kvVoters() {
		if !v.Reachable {
			down = append(down, v.Name)
		}
	}

	if len(down) > 0 {
		return c.fail("Consul KV does not answer: "+strings.Join(down, ", "), map[string]any{"unreachable": strs(down)})
	}

	return c.pass("every Consul KV answers", nil)
}

func (in *Input) initActiveValid() check.Check {
	a := in.Params.Datacenter
	c := block(check.InitActiveValid, a)

	if !in.configured(a) {
		return c.fail(fmt.Sprintf("%q is not a configured datacenter", a), nil)
	}

	return c.pass(a+" is a configured datacenter", nil)
}

// initActiveHasPrimary needs the primary in the datacenter init makes active. The
// primary is known only when every cluster answers, as another may hold one too.
func (in *Input) initActiveHasPrimary() check.Check {
	a := in.Params.Datacenter
	c := block(check.InitActiveHasPrimary, a)
	detected := in.Status.DetectedActiveDatacenter
	facts := map[string]any{"detected_active": detected}

	switch {
	case detected == "":
		return c.fail("where the primary runs is not known: a cluster does not answer, or there is not exactly one primary", facts)
	case detected != a:
		return c.fail("the primary runs in "+detected, facts)
	}

	return c.pass("the primary runs in "+a, facts)
}

func (in *Input) arbiterReachable() check.Check {
	c := block(check.ArbiterReachable, "")

	a := in.Snapshot.Arbiter
	switch {
	case a == nil:
		return c.fail("no arbiter is configured", nil)
	case !a.Reachable:
		return c.fail("the arbiter does not answer: "+string(a.Reason), map[string]any{"reason": string(a.Reason)})
	}

	return c.pass("the arbiter answers", nil)
}

func (in *Input) arbiterEmpty() check.Check {
	c := block(check.ArbiterEmpty, "")

	a := in.Snapshot.Arbiter
	switch {
	case a == nil:
		return c.fail("no arbiter is configured", nil)
	case !a.Reachable:
		return c.fail("the arbiter does not answer: "+string(a.Reason), nil)
	case a.Data.Initialized:
		return c.fail("the arbiter is initialized", nil)
	}

	return c.pass("the arbiter is empty", nil)
}

// arbiterStatesEqual needs the same dcctl/state in every Consul KV: the arbiter gets
// it, and must not take a side.
func (in *Input) arbiterStatesEqual() check.Check {
	c := block(check.ArbiterStatesEqual, "")

	var (
		first  *kv.State
		states = make(map[string]any)
		why    string
	)

	for _, v := range in.Status.View.Voters {
		if v.Kind != coord.KindKV {
			continue
		}

		if v.Status != coord.StatusPresent {
			states[v.Name] = string(v.Status)
			why = cmp.Or(why, "dcctl/state of "+v.Name+" is "+string(v.Status))

			continue
		}

		states[v.Name] = map[string]any{"generation": v.State.Generation, "active_datacenter": v.State.ActiveDatacenter}

		switch {
		case first == nil:
			first = &v.State
		case !samePayload(*first, v.State):
			why = cmp.Or(why, "dcctl/state of "+v.Name+" differs")
		}
	}

	facts := map[string]any{"states": states}

	if why != "" {
		return c.fail(why, facts)
	}

	if first == nil {
		return c.fail("no Consul KV holds dcctl/state", facts)
	}

	return c.pass("every Consul KV holds the same dcctl/state", facts)
}

func samePayload(a, b kv.State) bool {
	return a.Generation == b.Generation && a.ActiveDatacenter == b.ActiveDatacenter && a.OpID == b.OpID &&
		a.Forced == b.Forced && a.PatroniTimeline == b.PatroniTimeline && slices.Equal(a.NeedsRecovery, b.NeedsRecovery)
}
