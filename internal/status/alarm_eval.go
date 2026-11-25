package status

import (
	"fmt"
	"slices"
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

// Signal is what one cycle tells about the condition of an alarm.
type Signal string

const (
	// SignalRaise: the condition holds.
	SignalRaise Signal = "raise"
	// SignalClear: the condition does not hold, and every source it needs answered.
	SignalClear Signal = "clear"
	// SignalUnknown: what answered does not show the condition, and a source that may
	// show it did not answer, or the role it depends on is not known.
	SignalUnknown Signal = "unknown"
)

// Finding is the condition of one alarm on one cycle.
type Finding struct {
	Key    AlarmKey
	Signal Signal
	// Severity, VoterType, Detail and Facts are those of the alarm when raised.
	Severity  Severity
	VoterType VoterType
	Detail    string
	Facts     map[string]any

	// involved are the datacenters the condition is about: a running operation of one
	// of them holds back a suppressible alarm.
	involved []string
}

// AlarmInput is what the alarms of one cycle are evaluated from.
type AlarmInput struct {
	Snapshot *collect.Snapshot
	// Status is built from Snapshot.
	Status Status
	Lease  coord.LeaseStatus
	// LastActive is the last active datacenter the observer knew. It sets the severity
	// of datacenter_unreachable while the voters do not tell the active datacenter.
	LastActive string
}

// EvaluateAlarms evaluates the condition of every alarm for each of its instances: an
// alarm of the installation once, of a datacenter for each datacenter, of a voter for
// each voter. Alarms that do not apply to the profile are left out. Data of a source
// that does not answer never makes a condition false.
func EvaluateAlarms(in AlarmInput) []Finding {
	e := evaluator{in: in, st: &in.Status, snap: in.Snapshot}
	if in.Status.Effective != nil {
		e.active, e.known = in.Status.Effective.ActiveDatacenter, true
	}

	out := []Finding{e.conflict(), e.uncommitted()}
	out = append(out, e.voters()...)
	out = append(out, e.splitBrainGlobal(), e.multiplePrimaries(), e.leaseLost(), e.quorumLost(), e.operationInterrupted())

	if e.snap.Profile == config.ProfileStretch {
		out = append(out, e.dbLeaderMismatch())
	}

	for i := range e.st.Datacenters {
		out = append(out, e.datacenter(&e.st.Datacenters[i])...)
	}

	return out
}

type evaluator struct {
	in   AlarmInput
	st   *Status
	snap *collect.Snapshot
	// active is the active datacenter when known is set.
	active string
	known  bool
}

func finding(id AlarmID, dc string, sig Signal) Finding {
	f := Finding{Key: AlarmKey{ID: id, Datacenter: dc}, Signal: sig, Severity: alarmDefs[id].severity}
	if dc != "" {
		f.involved = []string{dc}
	}

	return f
}

func raised(id AlarmID, dc string, facts map[string]any, format string, args ...any) Finding {
	f := finding(id, dc, SignalRaise)
	f.Detail = fmt.Sprintf(format, args...)
	f.Facts = facts

	return f
}

// clearIf clears when every source the condition needs answered.
func clearIf(answered bool, id AlarmID, dc string) Finding {
	if answered {
		return finding(id, dc, SignalClear)
	}

	return finding(id, dc, SignalUnknown)
}

func list[T any](items []T, fn func(T) any) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, fn(it))
	}

	return out
}

func joined(items []any) string {
	s := make([]string, 0, len(items))
	for _, it := range items {
		s = append(s, fmt.Sprint(it))
	}

	return strings.Join(s, ", ")
}

// votersAnswered reports whether every voter answered with a record it can be judged
// by: one that does not answer or holds a record that cannot be decoded may hold any.
func (e *evaluator) votersAnswered() bool {
	return !slices.ContainsFunc(e.st.Voters, func(v Voter) bool {
		return v.Status == coord.StatusUnreachable || v.Status == coord.StatusInvalid
	})
}

func (e *evaluator) conflict() Finding {
	if c := e.st.View.Conflict; len(c) > 0 {
		dcs := list(c, func(r coord.Record) any { return r.State.ActiveDatacenter })

		return raised(AlarmConflict, "", map[string]any{"generation": c[0].State.Generation, "active_datacenters": dcs},
			"generation %d is valid with the active datacenters %s", c[0].State.Generation, joined(dcs))
	}

	return clearIf(e.votersAnswered(), AlarmConflict, "")
}

func (e *evaluator) uncommitted() Finding {
	if u := e.st.View.Uncommitted; len(u) > 0 {
		r := u[0]
		voters := list(r.Voters, func(v string) any { return v })

		return raised(AlarmUncommitted, "", map[string]any{
			"generation": r.State.Generation, "active_datacenter": r.State.ActiveDatacenter, "voters": voters,
		}, "generation %d with the active datacenter %s is on %s only, without a majority and not forced",
			r.State.Generation, r.State.ActiveDatacenter, joined(voters))
	}

	return clearIf(len(e.st.View.Undetermined) == 0 && e.votersAnswered(), AlarmUncommitted, "")
}

func (e *evaluator) voters() []Finding {
	out := make([]Finding, 0, 2*len(e.st.Voters))

	for i := range e.st.Voters {
		v := &e.st.Voters[i]

		out = append(out, e.voterUnreachable(v))

		switch v.Type {
		case VoterDatacenter:
			out = append(out, e.stale(v))
		case VoterArbiter:
			out = append(out, e.arbiterUninitialized(v))
		}
	}

	return out
}

func voterFinding(id AlarmID, v *Voter, sig Signal) Finding {
	f := finding(id, v.Datacenter, sig)
	f.Key.Voter, f.VoterType = v.Name, v.Type

	return f
}

func (e *evaluator) voterUnreachable(v *Voter) Finding {
	if v.Reachable {
		return voterFinding(AlarmVoterUnreachable, v, SignalClear)
	}

	f := voterFinding(AlarmVoterUnreachable, v, SignalRaise)
	f.Detail = fmt.Sprintf("voter %s does not answer: %s", v.Name, v.Reason)
	f.Facts = map[string]any{"voter": v.Name, "reason": string(v.Reason)}

	return f
}

func (e *evaluator) arbiterUninitialized(v *Voter) Finding {
	switch {
	case !v.Reachable:
		return voterFinding(AlarmArbiterUninitialized, v, SignalUnknown)
	case v.Status != coord.StatusUninitialized:
		return voterFinding(AlarmArbiterUninitialized, v, SignalClear)
	}

	f := voterFinding(AlarmArbiterUninitialized, v, SignalRaise)
	f.Detail = "the arbiter answers without the state"
	f.Facts = map[string]any{"voter": v.Name}

	return f
}

// stale is an alarm of the datacenter of the voter. A voter that stays stale for
// status.degraded_after was not aligned by the cycles in between. It is judged against
// the decided effective state only: the voters that do not answer may hold a newer one.
func (e *evaluator) stale(v *Voter) Finding {
	eff := e.st.Effective

	switch {
	case v.Status == coord.StatusUnreachable, v.Status == coord.StatusInvalid, eff == nil:
		return finding(AlarmStale, v.Datacenter, SignalUnknown)
	case !v.Stale:
		return finding(AlarmStale, v.Datacenter, SignalClear)
	}

	facts := map[string]any{
		"voter": v.Name, "generation": v.Generation, "effective_generation": eff.Generation, "key_missing": v.KeyMissing,
	}

	if v.KeyMissing {
		return raised(AlarmStale, v.Datacenter, facts, "voter %s has no dcctl/state, the effective generation is %d",
			v.Name, eff.Generation)
	}

	return raised(AlarmStale, v.Datacenter, facts, "voter %s holds generation %d, the effective generation is %d",
		v.Name, v.Generation, eff.Generation)
}

func (e *evaluator) quorumLost() Finding {
	q := e.st.Quorum
	if q.Available {
		return finding(AlarmQuorumLost, "", SignalClear)
	}

	return raised(AlarmQuorumLost, "", map[string]any{
		"reachable": q.ReachableCount, "required": q.RequiredCount, "total": q.TotalCount,
	}, "%d of %d voters can take a write, %d are needed", q.ReachableCount, q.TotalCount, q.RequiredCount)
}

func (e *evaluator) operationInterrupted() Finding {
	if op := e.st.Operation; op.GetState() == dcctlv1.Operation_INTERRUPTED {
		f := raised(AlarmOperationInterrupted, "", map[string]any{
			"operation": op.GetName(), "type": op.GetType().String(), "target": op.GetTarget(),
		}, "operation %s (%s) is interrupted", op.GetName(), op.GetType())

		if dc := datacenterName(op.GetTarget()); dc != "" {
			f.involved = []string{dc}
		}

		return f
	}

	// Every KV voter holds a copy of the operation: one that does not answer, answers
	// from a Consul without a leader or cannot be decoded may hold it interrupted.
	unread := slices.ContainsFunc(Operations(e.snap).Copies, func(c coord.OpCopy) bool {
		return c.Status == coord.StatusUnreachable || c.Status == coord.StatusInvalid
	}) || slices.ContainsFunc(e.st.Voters, func(v Voter) bool { return v.Type == VoterDatacenter && !v.Reachable })

	return clearIf(!unread, AlarmOperationInterrupted, "")
}

func datacenterName(name string) string {
	return strings.TrimPrefix(name, "datacenters/")
}

func (e *evaluator) leaseLost() Finding {
	l := e.in.Lease

	var f Finding

	switch l.State {
	case coord.LeaseHeld:
		f = finding(AlarmLeaseLost, "", SignalClear)
	case coord.LeaseUnknown:
		f = finding(AlarmLeaseLost, "", SignalUnknown)
	case coord.LeaseLost:
		f = raised(AlarmLeaseLost, "", map[string]any{
			"holder": l.Lease.Holder, "generation": l.Lease.Generation, "counter": l.Lease.Counter, "reason": string(l.Reason),
		}, "the lease of %s at generation %d is lost: %s", l.Lease.Holder, l.Lease.Generation, l.Reason)
	}

	switch {
	case e.known:
		f.involved = []string{e.active}
	case l.Lease.Holder != "":
		f.involved = []string{l.Lease.Holder}
	}

	return f
}

// runningJobs returns the datacenters whose Nomad answers with a managed job not
// stopped.
func (e *evaluator) runningJobs() ([]string, bool) {
	var (
		running  []string
		answered = true
	)

	for _, d := range e.st.Datacenters {
		switch {
		case !sourceIn(d, SystemNomad).Reachable:
			answered = false
		case d.Nomad.JobCounts.Stopped < d.Nomad.JobCounts.Total():
			running = append(running, d.Name)
		}
	}

	return running, answered
}

// splitBrainGlobal is split_brain while the active datacenter is not known: managed
// jobs that run in two datacenters run in an inactive one, whichever is active.
func (e *evaluator) splitBrainGlobal() Finding {
	running, answered := e.runningJobs()

	if !e.known && len(running) > 1 {
		dcs := list(running, func(dc string) any { return dc })
		f := raised(AlarmSplitBrain, "", map[string]any{"datacenters": dcs},
			"managed jobs run in %s while the active datacenter is not known", joined(dcs))
		f.involved = running

		return f
	}

	return clearIf(answered && (e.known || len(running) <= 1), AlarmSplitBrain, "")
}

func (e *evaluator) multiplePrimaries() Finding {
	var (
		primaries []any
		dcs       []string
		answered  = true
		inConsul  int
		consulDCs []string
	)

	for _, cl := range e.snap.Clusters {
		if cl.Patroni.Reachable {
			for _, m := range cl.Patroni.Data.Members {
				switch {
				case !memberReachable(m):
					// Its REST API may be down while PostgreSQL still takes writes.
					answered = false
				case m.Probe.Role == patroni.RolePrimary:
					primaries = append(primaries, m.Name)
					dcs = append(dcs, m.Datacenter)
				}
			}
		} else {
			answered = false
		}

		if !cl.Scope.Reachable {
			answered = false

			continue
		}

		if n := passingPrimaries(cl.Scope.Data.Nodes); n > inConsul {
			inConsul, consulDCs = n, cl.Datacenters
		}
	}

	facts := map[string]any{"primaries": primaries, "consul_primaries": inConsul}

	var f Finding

	switch {
	case len(primaries) > 1:
		f = raised(AlarmMultiplePrimaries, "", facts, "Patroni members %s report themselves primary", joined(primaries))
		f.involved = dcs
	case inConsul > 1:
		f = raised(AlarmMultiplePrimaries, "", facts, "Consul of %s has %d passing primary instances of the Patroni scope",
			strings.Join(consulDCs, ", "), inConsul)
		f.involved = consulDCs
	default:
		return clearIf(answered, AlarmMultiplePrimaries, "")
	}

	return f
}

func passingPrimaries(nodes []consul.ServiceNode) int {
	var n int

	for _, s := range nodes {
		if s.Status == "passing" && slices.Contains(s.Tags, "primary") {
			n++
		}
	}

	return n
}

// clusterLeader returns the cluster that spans dc and its leader by GET /cluster, or
// false when the cluster does not answer.
func (e *evaluator) clusterLeader(dc string) (*collect.Member, bool) {
	cl, ok := e.snap.ClusterOf(dc)
	if !ok || !cl.Patroni.Reachable {
		return nil, false
	}

	for i := range cl.Patroni.Data.Members {
		if m := &cl.Patroni.Data.Members[i]; m.Role == patroni.ClusterRoleLeader {
			return m, true
		}
	}

	return nil, true
}

func (e *evaluator) dbLeaderMismatch() Finding {
	if !e.known {
		return finding(AlarmDBLeaderMismatch, "", SignalUnknown)
	}

	l, ok := e.clusterLeader(e.active)
	if !ok || l == nil || l.Datacenter == "" {
		return finding(AlarmDBLeaderMismatch, "", SignalUnknown)
	}

	if l.Datacenter == e.active {
		return finding(AlarmDBLeaderMismatch, "", SignalClear)
	}

	f := raised(AlarmDBLeaderMismatch, "", map[string]any{
		"leader": l.Name, "leader_datacenter": l.Datacenter, "active_datacenter": e.active,
	}, "the Patroni leader %s runs in %s, the active datacenter is %s", l.Name, l.Datacenter, e.active)
	f.involved = []string{e.active, l.Datacenter}

	return f
}

func sourceIn(d Datacenter, sys System) Source {
	i := slices.IndexFunc(d.Sources, func(s Source) bool { return s.System == sys })
	if i < 0 {
		return Source{System: sys}
	}

	return d.Sources[i]
}

func (e *evaluator) datacenter(d *Datacenter) []Finding {
	out := []Finding{
		e.datacenterUnreachable(d), e.splitBrain(d), e.dbPrimaryMissing(d), e.appsDegraded(d), e.dbDegraded(d),
	}

	if e.snap.Profile == config.ProfileWarmStandby {
		out = append(out, e.dbSplit(d), e.standbyNotFollowing(d))
	}

	return out
}

func (e *evaluator) datacenterUnreachable(d *Datacenter) Finding {
	if d.State != StateUnreachable {
		return finding(AlarmDatacenterUnreachable, d.Name, SignalClear)
	}

	reasons := make(map[string]any, len(d.Sources))
	for _, s := range d.Sources {
		reasons[string(s.System)] = string(s.Reason)
	}

	f := raised(AlarmDatacenterUnreachable, d.Name, map[string]any{"sources": reasons}, "no source of %s answers", d.Name)

	// While the voters do not tell the active datacenter, the last one the observer knew
	// stands for it, although a forced record may have moved it without a majority. With
	// none known, the datacenter may be the active one.
	switch {
	case e.known && d.Name == e.active,
		!e.known && d.Name == e.in.LastActive,
		!e.known && e.in.LastActive == "":
		f.Severity = SeverityCritical
	}

	return f
}

func jobNames(n Nomad, keep func(JobState) bool) []any {
	var out []any

	for _, st := range n.Stages {
		for _, j := range st.Jobs {
			if keep(j.State) {
				out = append(out, j.Name)
			}
		}
	}

	return out
}

func (e *evaluator) splitBrain(d *Datacenter) Finding {
	jc := d.Nomad.JobCounts

	switch {
	case !e.known:
		return finding(AlarmSplitBrain, d.Name, SignalUnknown)
	case d.Name == e.active:
		return finding(AlarmSplitBrain, d.Name, SignalClear)
	case !sourceIn(*d, SystemNomad).Reachable:
		return finding(AlarmSplitBrain, d.Name, SignalUnknown)
	case jc.Stopped == jc.Total():
		return finding(AlarmSplitBrain, d.Name, SignalClear)
	}

	jobs := jobNames(d.Nomad, func(s JobState) bool { return s != JobStopped })

	return raised(AlarmSplitBrain, d.Name, map[string]any{"jobs": jobs},
		"inactive %s runs managed jobs: %s", d.Name, joined(jobs))
}

func (e *evaluator) dbSplit(d *Datacenter) Finding {
	switch {
	case !e.known:
		return finding(AlarmDBSplit, d.Name, SignalUnknown)
	case d.Name == e.active:
		return finding(AlarmDBSplit, d.Name, SignalClear)
	case !sourceIn(*d, SystemPatroni).Reachable:
		return finding(AlarmDBSplit, d.Name, SignalUnknown)
	case d.Patroni.Role != RolePrimary:
		return finding(AlarmDBSplit, d.Name, SignalClear)
	}

	facts := map[string]any{"leader": d.Patroni.Leader}
	if d.Patroni.Timeline != nil {
		facts["timeline"] = *d.Patroni.Timeline
	}

	return raised(AlarmDBSplit, d.Name, facts, "the Patroni leader %s of inactive %s is a primary", d.Patroni.Leader, d.Name)
}

func (e *evaluator) dbPrimaryMissing(d *Datacenter) Finding {
	switch {
	case !e.known:
		return finding(AlarmDBPrimaryMissing, d.Name, SignalUnknown)
	case d.Name != e.active:
		return finding(AlarmDBPrimaryMissing, d.Name, SignalClear)
	}

	l, ok := e.clusterLeader(d.Name)

	switch {
	case !ok:
		return finding(AlarmDBPrimaryMissing, d.Name, SignalUnknown)
	case l == nil && e.snap.Profile == config.ProfileStretch:
		return raised(AlarmDBPrimaryMissing, d.Name, nil, "the Patroni cluster has no leader")
	case l == nil:
		return raised(AlarmDBPrimaryMissing, d.Name, nil, "active %s has no Patroni primary", d.Name)
	}

	return finding(AlarmDBPrimaryMissing, d.Name, SignalClear)
}

func (e *evaluator) appsDegraded(d *Datacenter) Finding {
	jc := d.Nomad.JobCounts

	switch {
	case !e.known:
		return finding(AlarmAppsDegraded, d.Name, SignalUnknown)
	case d.Name != e.active:
		return finding(AlarmAppsDegraded, d.Name, SignalClear)
	case !sourceIn(*d, SystemNomad).Reachable:
		return finding(AlarmAppsDegraded, d.Name, SignalUnknown)
	case jc.Healthy == jc.Total():
		return finding(AlarmAppsDegraded, d.Name, SignalClear)
	}

	jobs := jobNames(d.Nomad, func(s JobState) bool { return s != JobHealthy })

	return raised(AlarmAppsDegraded, d.Name, map[string]any{"jobs": jobs},
		"managed jobs of active %s are not healthy: %s", d.Name, joined(jobs))
}

func (e *evaluator) dbDegraded(d *Datacenter) Finding {
	if src := sourceIn(*d, SystemPatroni); !src.Reachable {
		return raised(AlarmDBDegraded, d.Name, map[string]any{"reason": string(src.Reason)},
			"Patroni of %s does not answer: %s", d.Name, src.Reason)
	}

	var bad []any

	for _, m := range d.Patroni.Members {
		switch {
		case !m.Reachable:
			bad = append(bad, m.Name+": unreachable")
		case !m.Usable:
			bad = append(bad, m.Name+": "+string(m.UnusableReason))
		case m.PatroniState != "running" && m.PatroniState != "streaming":
			bad = append(bad, m.Name+": "+m.PatroniState)
		}
	}

	switch {
	case len(bad) > 0:
		return raised(AlarmDBDegraded, d.Name, map[string]any{"members": bad, "paused": d.Patroni.Paused},
			"Patroni members of %s are not fine: %s", d.Name, joined(bad))
	case d.Patroni.Paused:
		return raised(AlarmDBDegraded, d.Name, map[string]any{"paused": true}, "Patroni of %s is paused", d.Name)
	}

	// A member of no known datacenter may be one of this datacenter that is not fine.
	cl, _ := e.snap.ClusterOf(d.Name)
	unplaced := cl != nil && slices.ContainsFunc(cl.Patroni.Data.Members, func(m collect.Member) bool { return m.Datacenter == "" })

	return clearIf(!unplaced, AlarmDBDegraded, d.Name)
}

var (
	// notFollowing are the health reasons of a standby that does not stream from the
	// active datacenter on its timeline.
	notFollowing = []HealthReason{
		ReasonNoPatroniLeader, ReasonLeaderNotStandbyLeader, ReasonNotStreaming, ReasonNotFollowingActive,
		ReasonTimelineMismatch,
	}
	// followingUnknown are the health reasons that leave it untold.
	followingUnknown = []HealthReason{ReasonPatroniConfigUnreachable, ReasonUpstreamUnknown, ReasonActivePatroniUnreachable}
)

func (e *evaluator) standbyNotFollowing(d *Datacenter) Finding {
	switch {
	case !e.known:
		return finding(AlarmStandbyNotFollowing, d.Name, SignalUnknown)
	case d.Name == e.active, d.NeedsRecovery:
		return finding(AlarmStandbyNotFollowing, d.Name, SignalClear)
	case !sourceIn(*d, SystemPatroni).Reachable:
		return finding(AlarmStandbyNotFollowing, d.Name, SignalUnknown)
	}

	var reasons []any

	for _, r := range d.HealthReasons {
		if slices.Contains(notFollowing, r) {
			reasons = append(reasons, string(r))
		}
	}

	if len(reasons) > 0 {
		return raised(AlarmStandbyNotFollowing, d.Name, map[string]any{"reasons": reasons, "leader": d.Patroni.Leader},
			"standby %s does not follow active %s: %s", d.Name, e.active, joined(reasons))
	}

	unknown := slices.ContainsFunc(d.HealthReasons, func(r HealthReason) bool { return slices.Contains(followingUnknown, r) })

	return clearIf(!unknown, AlarmStandbyNotFollowing, d.Name)
}
