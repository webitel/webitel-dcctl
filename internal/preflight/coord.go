package preflight

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func (in *Input) votersFacts() []any {
	return list(in.Status.Voters, func(v status.Voter) any {
		f := map[string]any{"name": v.Name, "reachable": v.Reachable}
		if v.Generation > 0 {
			f["generation"] = v.Generation
			f["active_datacenter"] = v.ActiveDatacenter
		}

		return f
	})
}

// noVoterAnswers reports whether no voter can tell anything about the state.
func (in *Input) noVoterAnswers() bool {
	return !slices.ContainsFunc(in.Status.Voters, func(v status.Voter) bool { return v.Reachable })
}

func recordFacts(rs []coord.Record) []any {
	return list(rs, func(r coord.Record) any {
		return map[string]any{
			"generation": r.State.Generation, "active_datacenter": r.State.ActiveDatacenter,
			"forced": r.State.Forced, "voters": strs(r.Voters),
		}
	})
}

func (in *Input) statePresent() check.Check {
	s := block(check.CoordStatePresent, "")
	facts := map[string]any{"voters": in.votersFacts()}

	if !in.Status.Initialized {
		return s.fail("no voter that answers holds dcctl/state", facts)
	}

	return s.pass("dcctl/state exists", facts)
}

// kvAnswers reports whether some Consul KV answered, and lists those that did not.
func (in *Input) kvAnswers() (bool, []any) {
	var (
		answered    bool
		unreachable []any
	)

	for _, v := range in.Snapshot.Voters {
		if v.KV.Reachable {
			answered = true
		} else {
			unreachable = append(unreachable, v.Datacenter)
		}
	}

	return answered, unreachable
}

func (in *Input) noOperation() check.Check {
	s := block(check.CoordNoOperation, "")

	ok, unreachable := in.kvAnswers()
	if !ok {
		return s.fail("no Consul KV answers, an operation may be running",
			map[string]any{"reachable": false, "unreachable": unreachable})
	}

	c := status.Operations(in.Snapshot).Blocking()
	if c == nil {
		return s.pass("no operation is running or interrupted", map[string]any{"unreachable": unreachable})
	}

	if c.Status == coord.StatusInvalid {
		return s.fail(fmt.Sprintf("%s in %s cannot be read: %s", kv.OpKey, c.Name, c.Reason),
			map[string]any{"kv": c.Name, "reason": string(c.Reason)})
	}

	op := c.Operation

	return s.fail(fmt.Sprintf("operation %s is %s", op.GetName(), stateName(op.GetState())), map[string]any{
		"op_id": op.GetName(), "status": stateName(op.GetState()), "leader": op.GetLeader().GetNode(),
		"leader_datacenter": op.GetLeader().GetDatacenter(), "kv": c.Name,
	})
}

func stateName(s dcctlv1.Operation_State) string {
	return strings.ToLower(s.String())
}

func (in *Input) lockFree() check.Check {
	s := block(check.CoordLockFree, "")

	ok, unreachable := in.kvAnswers()
	if !ok {
		return s.fail("no Consul KV answers, the lock may be held",
			map[string]any{"reachable": false, "unreachable": unreachable})
	}

	for _, v := range in.Snapshot.Voters {
		e := v.KV.Data.Lock
		if !v.KV.Reachable || e == nil || e.Session == "" {
			continue
		}

		var holder kv.Lock
		// A value that cannot be decoded still holds the lock; the holder is unknown.
		_ = json.Unmarshal(e.Value, &holder)

		return s.fail(fmt.Sprintf("%s in %s is held by %q of %s", kv.LockKey, v.Datacenter, holder.Instance, holder.Datacenter),
			map[string]any{
				"kv": v.Datacenter, "holder": holder.Instance, "datacenter": holder.Datacenter,
				"operation": holder.Operation,
			})
	}

	return s.pass("the lock is free", map[string]any{"unreachable": unreachable})
}

// leaderDatacenters returns the datacenters whose instance leads the action. Preflight
// runs on the leading instance, the requests are forwarded to it.
func (in *Input) leaderDatacenters() []string {
	switch in.Action {
	case AppsSync:
		if a, ok := in.active(); ok {
			return []string{a}
		}

		return nil
	case Resume:
		// The instance that led the operation, or one of its target when that one is
		// unavailable.
		op := in.operation()
		if op == nil {
			return nil
		}

		return []string{datacenterID(op.GetLeader().GetDatacenter()), datacenterID(op.GetTarget())}
	case Switch, Failover, Demote, Repoint, MarkRecovered, Resolve, Abandon, Init, ArbiterInit, Verify:
	}

	return []string{in.Params.Datacenter}
}

func (in *Input) leaderReachable() check.Check {
	s := block(check.CoordLeaderReachable, "")
	self := in.Snapshot.Observer
	facts := map[string]any{"instance": self.Node, "datacenter": self.Datacenter}

	leaders := in.leaderDatacenters()
	if !slices.Contains(leaders, self.Datacenter) {
		facts["leader_datacenters"] = strs(leaders)

		return s.fail("this instance does not lead the action, its datacenter is "+self.Datacenter, facts)
	}

	d, ok := in.Snapshot.Datacenter(self.Datacenter)
	if !ok || !d.Services.Reachable {
		reason := "unknown"
		if ok {
			reason = string(d.Services.Reason)
		}

		facts["error"] = reason

		return s.fail("the Consul catalog of the leading instance does not answer: "+reason, facts)
	}

	i := slices.IndexFunc(d.Services.Data.Dcctl, func(n consul.ServiceNode) bool { return n.Node == self.Node })
	if i < 0 {
		return s.fail("the leading instance is not registered in Consul", facts)
	}

	n := d.Services.Data.Dcctl[i]
	facts["address"] = n.HostPort()

	if n.Status != passing {
		facts["error"] = n.Status

		return s.fail("the leading instance is "+n.Status+" in Consul", facts)
	}

	return s.pass("the leading instance is passing in Consul", facts)
}

func (in *Input) noConflict() check.Check {
	s := block(check.CoordNoConflict, "")

	if in.noVoterAnswers() {
		return s.fail("no voter answers", map[string]any{"voters": in.votersFacts()})
	}

	if c := in.Status.View.Conflict; len(c) > 0 {
		return s.fail(fmt.Sprintf("generation %d has %d conflicting records", c[0].State.Generation, len(c)),
			map[string]any{"records": recordFacts(c)})
	}

	return s.pass("no conflicting records", nil)
}

// noUncommitted fails on a record above the effective state that is not on a
// majority. A record that the unreachable voters may hold too is not known to be
// uncommitted either: only a failover proceeds then, writing over the highest of them.
func (in *Input) noUncommitted() check.Check {
	s := block(check.CoordNoUncommitted, "")
	view := in.Status.View

	if in.noVoterAnswers() {
		return s.fail("no voter answers", map[string]any{"voters": in.votersFacts()})
	}

	if u := view.Uncommitted; len(u) > 0 {
		return s.fail(fmt.Sprintf("generation %d is not on a majority of voters", u[0].State.Generation),
			map[string]any{"generation": u[0].State.Generation, "records": recordFacts(u)})
	}

	if u := view.Undetermined; len(u) > 0 {
		facts := map[string]any{"generation": u[0].State.Generation, "records": recordFacts(u)}

		if _, ok := coord.UndeterminedTop(view); !ok {
			return s.fail(fmt.Sprintf("records of generation %d that voters that do not answer may hold name different datacenters",
				u[0].State.Generation), facts)
		}

		if in.Action == Failover {
			facts["base_generation"] = u[0].State.Generation

			return s.pass(fmt.Sprintf("the failover writes over generation %d, which voters that do not answer may hold",
				u[0].State.Generation), facts)
		}

		return s.fail(fmt.Sprintf("generation %d may be committed on voters that do not answer", u[0].State.Generation), facts)
	}

	return s.pass("no uncommitted generation", nil)
}

// noSplit fails on an inactive datacenter in split_brain (a managed job not stopped)
// or, in warm_standby, db_split (its Patroni leader a primary), except, for demote, the
// one demoted. A datacenter whose source does not answer may be in either and fails it
// too. The rules are those of the status, judged against the active datacenter of
// [Input.reference], so a failover from a partial advance is judged too.
func (in *Input) noSplit(except string) check.Check {
	s := block(check.CoordNoSplit, "")

	active, ok := in.active()
	if !ok {
		return s.fail("the active datacenter is not known, so neither is which datacenters must be stopped", nil)
	}

	var split, unknown []any

	for i := range in.Status.Datacenters {
		d := &in.Status.Datacenters[i]
		if d.Name == except || d.Name == active {
			continue
		}

		if n := d.Source(status.SystemNomad); !n.Reachable {
			unknown = append(unknown, map[string]any{"datacenter": d.Name, "source": string(n.System)})
		} else if d.Nomad.JobCounts.Stopped < d.Nomad.JobCounts.Total() {
			split = append(split, map[string]any{"datacenter": d.Name, "state": string(status.StateSplitBrain)})
		}

		if !in.warm() {
			continue
		}

		if p := d.Source(status.SystemPatroni); !p.Reachable {
			unknown = append(unknown, map[string]any{"datacenter": d.Name, "source": string(p.System)})
		} else if d.Patroni.Role == status.RolePrimary {
			split = append(split, map[string]any{"datacenter": d.Name, "state": string(status.StateDBSplit)})
		}
	}

	facts := map[string]any{"active_datacenter": active, "split": split, "unknown": unknown}

	switch {
	case len(split) > 0:
		return s.fail(fmt.Sprintf("%d datacenters are split", len(split)), facts)
	case len(unknown) > 0:
		return s.fail(fmt.Sprintf("it is not known whether %d datacenters are split", len(unknown)), facts)
	}

	return s.pass("no datacenter is split", facts)
}

func (in *Input) quorum(effect check.Effect) check.Check {
	s := spec{id: check.CoordQuorum, effect: effect}
	q := in.Status.Quorum
	// mode is the quorum mode of the voters that answer, quorum or forced_only; how the
	// generation is written is decided by the operation from the voters of the effective
	// record.
	facts := map[string]any{
		"voters": in.votersFacts(), "required_count": q.RequiredCount, "reachable_count": q.ReachableCount,
		"mode": string(q.Mode),
	}

	if !q.Available {
		return s.fail(fmt.Sprintf("%d of %d voters answer, %d required", q.ReachableCount, q.TotalCount, q.RequiredCount), facts)
	}

	return s.pass(fmt.Sprintf("%d of %d voters answer", q.ReachableCount, q.TotalCount), facts)
}

// isolation always needs the operator to state that the old primary is isolated; the
// facts are what the operator judges it by.
func (in *Input) isolation() check.Check {
	s := confirm(check.CoordIsolation, check.ConfirmIsolation, "")
	facts := map[string]any{"mode": string(in.Status.Quorum.Mode), "voters": in.votersFacts()}

	facts["datacenters"] = list(in.isolated(), func(dc string) any { return in.isolationFacts(dc) })

	return s.fail("the old primary must be isolated", facts)
}

// isolated returns the datacenters whose primary the action takes over.
func (in *Input) isolated() []string {
	if in.Action == Resolve {
		var out []string

		for _, r := range in.Status.View.Conflict {
			if dc := r.State.ActiveDatacenter; dc != in.Params.Datacenter && !slices.Contains(out, dc) {
				out = append(out, dc)
			}
		}

		return out
	}

	if a, ok := in.active(); ok && a != in.Params.Datacenter {
		return []string{a}
	}

	return nil
}

func (in *Input) isolationFacts(dc string) map[string]any {
	f := map[string]any{"datacenter": dc, "patroni_reachable": false}

	if d, ok := in.datacenter(dc); ok && d.Source(status.SystemPatroni).Reachable {
		f["patroni_reachable"] = true
		f["role"] = string(d.Patroni.Role)
	}

	if seen := in.lastSeen(dc); seen != nil {
		f["last_seen"] = seen
	}

	return f
}

// lastSeen is the leader of dc as the collection loop last read it.
func (in *Input) lastSeen(dc string) map[string]any {
	if in.LastKnown == nil {
		return nil
	}

	cl, ok := in.LastKnown.ClusterOf(dc)
	if !ok || cl.Patroni.ObserveTime.IsZero() {
		return nil
	}

	i := slices.IndexFunc(cl.Patroni.Data.Members, func(m collect.Member) bool {
		return m.Datacenter == dc && m.Probe != nil &&
			(m.Role == patroni.ClusterRoleLeader || m.Role == patroni.ClusterRoleStandbyLeader)
	})
	if i < 0 {
		return nil
	}

	m := cl.Patroni.Data.Members[i]
	f := map[string]any{
		"time": cl.Patroni.ObserveTime.UTC().Format(time.RFC3339), "member": m.Name, "role": string(m.Probe.Role),
	}

	if loc := m.Probe.Xlog.Location; loc != nil {
		f["xlog_location"] = *loc
	}

	if host := in.leaderHost(in.Params.Datacenter); host != "" {
		for _, r := range m.Probe.Replication {
			if r.ClientAddr == host {
				f["target_sync_state"] = r.SyncState
			}
		}
	}

	return f
}

// leaderHost is the host of the leader of dc in the fresh snapshot, "" when not known.
func (in *Input) leaderHost(dc string) string {
	d, ok := in.datacenter(dc)
	if !ok || d.Patroni.Leader == "" {
		return ""
	}

	for _, m := range d.Patroni.Members {
		if m.Name == d.Patroni.Leader {
			return m.Host
		}
	}

	return ""
}

// profileSupported rejects an action the profile has no scenario for: demote and
// repoint move a Patroni cluster of a datacenter between primary and standby, which
// stretch, with one cluster over all datacenters, does not have.
func (in *Input) profileSupported() check.Check {
	s := block(check.CoordProfile, "")
	facts := map[string]any{"profile": string(in.Snapshot.Profile)}

	if (in.Action == Demote || in.Action == Repoint) && !in.warm() {
		return s.fail(fmt.Sprintf("%s applies to warm_standby only", in.Action), facts)
	}

	return s.pass(fmt.Sprintf("%s applies to %s", in.Action, in.Snapshot.Profile), facts)
}

func (in *Input) targetValid() check.Check {
	s := block(check.CoordTargetValid, in.Params.Datacenter)
	target := in.Params.Datacenter

	if !in.configured(target) {
		return s.fail(fmt.Sprintf("%q is not a configured datacenter", target), nil)
	}

	a, ok := in.active()
	if !ok {
		return s.fail("the active datacenter is not known", nil)
	}

	if a == target {
		return s.fail(target+" is the active datacenter", map[string]any{"active_datacenter": a})
	}

	return s.pass(target+" is a standby datacenter", map[string]any{"active_datacenter": a})
}

func (in *Input) targetNotNeedsRecovery() check.Check {
	s := block(check.CoordTargetNotNeedsRecovery, in.Params.Datacenter)

	ref := in.reference()
	if ref == nil {
		return s.fail("the state of the voters is not known", nil)
	}

	facts := map[string]any{"needs_recovery": strs(ref.NeedsRecovery)}

	if slices.Contains(ref.NeedsRecovery, in.Params.Datacenter) {
		return s.fail(in.Params.Datacenter+" needs recovery", facts)
	}

	return s.pass(in.Params.Datacenter+" does not need recovery", facts)
}

func (in *Input) dcNotActive() check.Check {
	dc := in.Params.Datacenter
	s := block(check.CoordDCNotActive, dc)

	if !in.configured(dc) {
		return s.fail(fmt.Sprintf("%q is not a configured datacenter", dc), nil)
	}

	a, ok := in.active()
	if !ok {
		return s.fail("the active datacenter is not known", nil)
	}

	if a == dc {
		return s.fail(dc+" is the active datacenter", map[string]any{"active_datacenter": a})
	}

	return s.pass(dc+" is not active", map[string]any{"active_datacenter": a})
}

func (in *Input) dcNeedsRecovery() check.Check {
	dc := in.Params.Datacenter
	s := block(check.CoordDCNeedsRecovery, dc)

	ref := in.reference()
	if ref == nil {
		return s.fail("the state of the voters is not known", nil)
	}

	facts := map[string]any{"needs_recovery": strs(ref.NeedsRecovery)}

	if !slices.Contains(ref.NeedsRecovery, dc) {
		return s.fail(dc+" does not need recovery", facts)
	}

	return s.pass(dc+" needs recovery", facts)
}
