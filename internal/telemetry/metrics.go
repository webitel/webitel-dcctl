package telemetry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/status"
)

const scope = "github.com/webitel/webitel-dcctl/internal/telemetry"

// Observations gives what the last collection cycle observed; [*status.Tracker] is
// one.
type Observations interface {
	// Current returns nil before the first cycle.
	Current() *status.Observed
}

// The states a datacenter is reported in, those of webitel.dcctl.datacenter.state. The
// registry has no undetermined yet: such a datacenter is 0 in every state until the
// value is added there and here.
var datacenterStates = []status.State{
	status.StateUnreachable, status.StateUninitialized, status.StateActive, status.StateSplitBrain,
	status.StateDBSplit, status.StateStale, status.StateNeedsRecovery, status.StateStandby,
}

var (
	voterStates  = []string{voterOK, voterUnreachable, voterUninitialized}
	quorumModes  = []status.QuorumMode{status.QuorumQuorum, status.QuorumForcedOnly}
	patroniRoles = []string{rolePrimary, roleStandbyLeader, roleReplica, roleOther, roleUnknown}
	jobStates    = []status.JobState{status.JobHealthy, status.JobUnhealthy, status.JobStopping, status.JobStopped}
	sourceStates = []string{sourceOK, sourcePartial, sourceUnreachable}
)

// statusMetrics are the instruments observed from the status of the last cycle.
type statusMetrics struct {
	src Observations
	now func() time.Time
	// maxAge is how old the status may be to be observed.
	maxAge time.Duration

	generation      metric.Int64ObservableGauge
	voterGeneration metric.Int64ObservableGauge
	voterStatus     metric.Int64ObservableUpDownCounter
	quorumStatus    metric.Int64ObservableUpDownCounter
	leaseAge        metric.Float64ObservableGauge
	dcStatus        metric.Int64ObservableUpDownCounter
	alarmCount      metric.Int64ObservableUpDownCounter
	memberCount     metric.Int64ObservableUpDownCounter
	timeline        metric.Int64ObservableGauge
	walPosition     metric.Int64ObservableGauge
	lag             metric.Int64ObservableGauge
	divergence      metric.Int64ObservableGauge
	jobCount        metric.Int64ObservableUpDownCounter
	sourceStatus    metric.Int64ObservableUpDownCounter
	collectDuration metric.Float64ObservableGauge
}

// RegisterMetrics registers the metrics of the installation as this instance sees it,
// observed from src on every collection. Nothing is observed before the first cycle or
// while the last one is older than maxAge, see [collect.StaleAfter]; a value that is not
// known is not observed at all. Unregister the result to stop.
func RegisterMetrics(mp metric.MeterProvider, src Observations, maxAge time.Duration) (metric.Registration, error) {
	return registerStatusMetrics(mp, src, maxAge, time.Now)
}

func registerStatusMetrics(
	mp metric.MeterProvider, src Observations, maxAge time.Duration, now func() time.Time,
) (metric.Registration, error) {
	m := mp.Meter(scope)
	sm := &statusMetrics{src: src, now: now, maxAge: maxAge}

	var errs []error

	gauge := func(name, unit, desc string) metric.Int64ObservableGauge {
		g, err := m.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)

		return g
	}
	seconds := func(name, desc string) metric.Float64ObservableGauge {
		g, err := m.Float64ObservableGauge(name, metric.WithUnit("s"), metric.WithDescription(desc))
		errs = append(errs, err)

		return g
	}
	upDown := func(name, unit, desc string) metric.Int64ObservableUpDownCounter {
		c, err := m.Int64ObservableUpDownCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)

		return c
	}

	sm.generation = gauge("webitel.dcctl.generation", "{generation}",
		"The effective generation of the coordination state.")
	sm.voterGeneration = gauge("webitel.dcctl.voter.generation", "{generation}", "The generation held by a voter.")
	sm.voterStatus = upDown("webitel.dcctl.voter.status", "1", "The current status of the voter.")
	sm.quorumStatus = upDown("webitel.dcctl.quorum.status", "1", "The current status of the quorum.")
	sm.leaseAge = seconds("webitel.dcctl.lease.age", "Time since the counter of the lease last changed.")
	sm.dcStatus = upDown("webitel.dcctl.datacenter.status", "1", "The current status of the datacenter.")
	sm.alarmCount = upDown("webitel.dcctl.alarm.count", "{alarm}", "Number of alarms that are currently raised.")
	sm.memberCount = upDown("webitel.dcctl.patroni.member.count", "{member}",
		"Number of Patroni members of the datacenter that currently have the role.")
	sm.timeline = gauge("webitel.dcctl.patroni.timeline", "{timeline}",
		"The timeline of the Patroni leader of the datacenter.")
	sm.walPosition = gauge("webitel.dcctl.patroni.wal.position", "By",
		"The WAL position of the Patroni leader of the datacenter.")
	sm.lag = gauge("webitel.dcctl.patroni.lag", "By", "The replication lag of the datacenter that is not active.")
	sm.divergence = gauge("webitel.dcctl.patroni.divergence", "By",
		"The size of the data of the datacenter that diverged from the active datacenter.")
	sm.jobCount = upDown("webitel.dcctl.job.count", "{job}",
		"Number of managed jobs that are currently in the state.")
	sm.sourceStatus = upDown("webitel.dcctl.source.status", "1", "The current status of the source.")
	sm.collectDuration = seconds("webitel.dcctl.collect.duration", "Duration of the last completed collection cycle.")

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("create status metrics: %w", err)
	}

	reg, err := m.RegisterCallback(sm.observe,
		sm.generation, sm.voterGeneration, sm.voterStatus, sm.quorumStatus, sm.leaseAge, sm.dcStatus, sm.alarmCount,
		sm.memberCount, sm.timeline, sm.walPosition, sm.lag, sm.divergence, sm.jobCount, sm.sourceStatus,
		sm.collectDuration,
	)
	if err != nil {
		return nil, fmt.Errorf("register status metrics: %w", err)
	}

	return reg, nil
}

func (sm *statusMetrics) observe(_ context.Context, o metric.Observer) error {
	obs := sm.src.Current()
	if obs == nil {
		return nil
	}

	st := &obs.Status

	// A loop that stopped finishing cycles leaves a status that tells nothing about
	// now; its liveness check reports it.
	if sm.now().Sub(st.ObserveTime) > sm.maxAge {
		return nil
	}

	o.ObserveFloat64(sm.collectDuration, st.CycleDuration.Seconds())

	if st.Effective != nil {
		o.ObserveInt64(sm.generation, st.Effective.Generation)
	}

	for _, mode := range quorumModes {
		o.ObserveInt64(sm.quorumStatus, oneIf(st.Quorum.Mode == mode), metric.WithAttributes(quorumModeKey.String(string(mode))))
	}

	if l := obs.Lease; l.Lease.Holder != "" && !l.Since.IsZero() {
		o.ObserveFloat64(sm.leaseAge, sm.now().Sub(l.Since).Seconds(),
			metric.WithAttributes(datacenterNameKey.String(l.Lease.Holder)))
	}

	sm.observeVoters(o, st)
	sm.observeAlarms(o, obs)

	for i := range st.Datacenters {
		sm.observeDatacenter(o, st, &st.Datacenters[i])
	}

	return nil
}

func (sm *statusMetrics) observeVoters(o metric.Observer, st *status.Status) {
	for _, v := range st.Voters {
		name := voterNameKey.String(v.Name)

		if v.Reachable && v.Status == coord.StatusPresent {
			o.ObserveInt64(sm.voterGeneration, v.Generation, metric.WithAttributes(name))
		}

		state, known := voterState(v)
		for _, s := range voterStates {
			o.ObserveInt64(sm.voterStatus, oneIf(known && s == state), metric.WithAttributes(name, voterStateKey.String(s)))
		}

		if v.Type == status.VoterArbiter {
			observeSource(o, sm.sourceStatus, nil, systemArbiter, reachability(v.Reachable))
		}
	}
}

// voterState is the state of v; a record that cannot be decoded has none of the
// states, and every point of the voter is 0.
func voterState(v status.Voter) (string, bool) {
	switch {
	case !v.Reachable:
		return voterUnreachable, true
	case v.Status == coord.StatusPresent:
		return voterOK, true
	case v.Status == coord.StatusAbsent, v.Status == coord.StatusUninitialized:
		return voterUninitialized, true
	}

	return "", false
}

// alarmSeries is the attribute set of an alarm.
type alarmSeries struct {
	id         status.AlarmID
	datacenter string
	voter      string
}

func (s alarmSeries) attributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{alarmNameKey.String(string(s.id))}

	switch {
	case s.voter != "":
		attrs = append(attrs, voterNameKey.String(s.voter))
	case s.datacenter != "":
		attrs = append(attrs, datacenterNameKey.String(s.datacenter))
	}

	return attrs
}

// observeAlarms observes every alarm for what it is raised for, 0 when it is not
// raised. An alarm raised for something else than usual, such as split_brain of the
// whole installation, gets a point of its own.
func (sm *statusMetrics) observeAlarms(o metric.Observer, obs *status.Observed) {
	st := &obs.Status
	counts := make(map[alarmSeries]int64)

	for _, id := range status.AlarmIDs() {
		switch id.Scope() {
		case status.AlarmScopeInstallation:
			counts[alarmSeries{id: id}] = 0
		case status.AlarmScopeDatacenter:
			for _, d := range st.Datacenters {
				counts[alarmSeries{id: id, datacenter: d.Name}] = 0
			}
		case status.AlarmScopeVoter, status.AlarmScopeArbiter:
			for _, v := range st.Voters {
				if id.Scope() == status.AlarmScopeVoter || v.Type == status.VoterArbiter {
					counts[alarmSeries{id: id, voter: v.Name}] = 0
				}
			}
		}
	}

	for _, a := range obs.Alarms {
		s := alarmSeries{id: a.ID, voter: a.Voter}
		if a.Voter == "" {
			s.datacenter = a.Datacenter
		}

		counts[s]++
	}

	for s, n := range counts {
		o.ObserveInt64(sm.alarmCount, n, metric.WithAttributes(s.attributes()...))
	}
}

func (sm *statusMetrics) observeDatacenter(o metric.Observer, st *status.Status, d *status.Datacenter) {
	dc := datacenterNameKey.String(d.Name)
	at := metric.WithAttributes(dc)

	for _, s := range datacenterStates {
		o.ObserveInt64(sm.dcStatus, oneIf(d.State == s), metric.WithAttributes(dc, datacenterStateKey.String(string(s))))
	}

	observeSource(o, sm.sourceStatus, &dc, string(status.SystemConsul), consulState(st, d))
	observeSource(o, sm.sourceStatus, &dc, string(status.SystemNomad), reachability(d.Source(status.SystemNomad).Reachable))
	observeSource(o, sm.sourceStatus, &dc, string(status.SystemPatroni), patroniState(d))

	if d.Source(status.SystemNomad).Reachable {
		for _, stage := range d.Nomad.Stages {
			for _, s := range jobStates {
				o.ObserveInt64(sm.jobCount, int64(jobCount(stage.JobCounts, s)),
					metric.WithAttributes(dc, jobStageKey.Int(stage.Stage), jobStateKey.String(string(s))))
			}
		}
	}

	// The members of a Patroni that does not answer are only last known: not even
	// how many there are is known now.
	if !d.Source(status.SystemPatroni).Reachable {
		return
	}

	p := &d.Patroni

	roles := make(map[string]int64, len(patroniRoles))
	for _, m := range p.Members {
		roles[memberRole(m)]++
	}

	for _, r := range patroniRoles {
		o.ObserveInt64(sm.memberCount, roles[r], metric.WithAttributes(dc, patroniRoleKey.String(r)))
	}

	if p.Timeline != nil {
		o.ObserveInt64(sm.timeline, *p.Timeline, at)
	}

	if pos := leaderWALPosition(p); pos != nil {
		o.ObserveInt64(sm.walPosition, *pos, at)
	}

	if p.LagBytes != nil && (st.Effective == nil || st.Effective.ActiveDatacenter != d.Name) {
		o.ObserveInt64(sm.lag, *p.LagBytes, at)
	}

	if p.Divergence.Known && p.Divergence.Diverged {
		o.ObserveInt64(sm.divergence, p.Divergence.Bytes, at)
	}
}

// observeSource observes a point for every state of the source; dc is nil for the
// arbiter.
func observeSource(o metric.Observer, c metric.Int64ObservableUpDownCounter, dc *attribute.KeyValue, system, state string) {
	for _, s := range sourceStates {
		attrs := []attribute.KeyValue{sourceSystemKey.String(system), sourceStateKey.String(s)}
		if dc != nil {
			attrs = append(attrs, *dc)
		}

		o.ObserveInt64(c, oneIf(s == state), metric.WithAttributes(attrs...))
	}
}

func reachability(reachable bool) string {
	if reachable {
		return sourceOK
	}

	return sourceUnreachable
}

// consulState is the state of the Consul of the datacenter: its catalog, and in
// warm_standby also its KV voter. A KV that answers without a leader counts as one
// that does not answer, as it does in the vote.
func consulState(st *status.Status, d *status.Datacenter) string {
	catalog := d.Source(status.SystemConsul).Reachable

	i := slices.IndexFunc(st.Voters, func(v status.Voter) bool {
		return v.Type == status.VoterDatacenter && v.Datacenter == d.Name
	})
	if i < 0 {
		return reachability(catalog)
	}

	switch kv := st.Voters[i].Reachable; {
	case catalog && kv:
		return sourceOK
	case catalog || kv:
		return sourcePartial
	}

	return sourceUnreachable
}

// patroniState is partial when Patroni of the datacenter answers while some of its
// members do not.
func patroniState(d *status.Datacenter) string {
	switch {
	case !d.Source(status.SystemPatroni).Reachable:
		return sourceUnreachable
	case slices.ContainsFunc(d.Patroni.Members, func(m status.Member) bool { return !m.Reachable }):
		return sourcePartial
	}

	return sourceOK
}

func jobCount(c status.JobCounts, s status.JobState) int {
	switch s {
	case status.JobHealthy:
		return c.Healthy
	case status.JobUnhealthy:
		return c.Unhealthy
	case status.JobStopping:
		return c.Stopping
	case status.JobStopped:
		return c.Stopped
	}

	return 0
}

// memberRole is the role of a member that answers; the synchronous and quorum
// standbys are replicas.
func memberRole(m status.Member) string {
	if !m.Reachable {
		return roleUnknown
	}

	switch m.Role {
	case status.RolePrimary:
		return rolePrimary
	case status.RoleStandbyLeader:
		return roleStandbyLeader
	case status.RoleReplica, status.RoleSyncStandby, status.RoleQuorumStandby:
		return roleReplica
	case status.RoleUnknown:
	}

	return roleOther
}

// leaderWALPosition is the WAL written by the leader of the datacenter when it is a
// primary, received when it is a standby leader; nil when the leader does not answer.
func leaderWALPosition(p *status.Patroni) *int64 {
	i := slices.IndexFunc(p.Members, func(m status.Member) bool { return m.Name == p.Leader })
	if p.Leader == "" || i < 0 || !p.Members[i].Reachable {
		return nil
	}

	switch p.Role {
	case status.RolePrimary:
		return p.Members[i].Xlog.Location
	case status.RoleStandbyLeader:
		return p.Members[i].Xlog.ReceivedLocation
	case status.RoleUnknown, status.RoleReplica, status.RoleSyncStandby, status.RoleQuorumStandby:
	}

	return nil
}

func oneIf(b bool) int64 {
	if b {
		return 1
	}

	return 0
}
