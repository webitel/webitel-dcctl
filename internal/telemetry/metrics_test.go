package telemetry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// collectedAt is when the reader collects: the lease was first seen 12s before.
var collectedAt = observeTime.Add(2 * time.Second)

// maxAge is the stale limit of the default status.interval and status.request_timeout.
const maxAge = 3 * (5*time.Second + 3*time.Second)

func collectStatus(t *testing.T, src Observations) map[string]series {
	t.Helper()

	return collectStatusAt(t, src, collectedAt)
}

func collectStatusAt(t *testing.T, src Observations, at time.Time) map[string]series {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { require.NoError(t, mp.Shutdown(t.Context())) })

	reg, err := registerStatusMetrics(mp, src, maxAge, func() time.Time { return at })
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, reg.Unregister()) })

	return collectSeries(t, reader)
}

const (
	dcA = "webitel.datacenter.name=dc_a"
	dcB = "webitel.datacenter.name=dc_b"
)

func TestStatusMetrics_representative(t *testing.T) {
	got := collectStatus(t, observing(representative()))

	// Metrics whose every point is listed.
	full := map[string]series{
		"webitel.dcctl.generation": {kind: kindGauge, unit: "{generation}", points: map[string]float64{"": 7}},
		"webitel.dcctl.voter.generation": {kind: kindGauge, unit: "{generation}", points: map[string]float64{
			"webitel.dcctl.voter.name=dc_a": 7,
			"webitel.dcctl.voter.name=dc_b": 7,
		}},
		"webitel.dcctl.quorum.status": {kind: kindUpDownCounter, unit: "1", points: map[string]float64{
			"webitel.dcctl.quorum.mode=quorum":      1,
			"webitel.dcctl.quorum.mode=forced_only": 0,
		}},
		"webitel.dcctl.lease.age": {kind: kindGauge, unit: "s", points: map[string]float64{dcA: 12}},
		"webitel.dcctl.patroni.member.count": {kind: kindUpDownCounter, unit: "{member}", points: map[string]float64{
			dcA + ",webitel.dcctl.patroni.role=primary":        1,
			dcA + ",webitel.dcctl.patroni.role=standby_leader": 0,
			dcA + ",webitel.dcctl.patroni.role=replica":        1,
			dcA + ",webitel.dcctl.patroni.role=other":          0,
			dcA + ",webitel.dcctl.patroni.role=unknown":        0,
			dcB + ",webitel.dcctl.patroni.role=primary":        0,
			dcB + ",webitel.dcctl.patroni.role=standby_leader": 1,
			dcB + ",webitel.dcctl.patroni.role=replica":        0,
			dcB + ",webitel.dcctl.patroni.role=other":          0,
			dcB + ",webitel.dcctl.patroni.role=unknown":        1,
		}},
		"webitel.dcctl.patroni.timeline": {kind: kindGauge, unit: "{timeline}", points: map[string]float64{dcA: 3, dcB: 3}},
		"webitel.dcctl.patroni.wal.position": {kind: kindGauge, unit: "By", points: map[string]float64{
			dcA: 100000,
			dcB: 95904,
		}},
		"webitel.dcctl.patroni.lag":        {kind: kindGauge, unit: "By", points: map[string]float64{dcB: 4096}},
		"webitel.dcctl.patroni.divergence": {kind: kindGauge, unit: "By", points: map[string]float64{dcB: 512}},
		"webitel.dcctl.collect.duration":   {kind: kindGauge, unit: "s", points: map[string]float64{"": 1.5}},
	}

	for name, want := range full {
		assert.Equal(t, want, got[name], name)
	}

	// Metrics with a point for every value of an enum: the count of points and those
	// that are not 0.
	oneHot := map[string]struct {
		kind, unit string
		count      int
		nonZero    map[string]float64
	}{
		"webitel.dcctl.voter.status": {kind: kindUpDownCounter, unit: "1", count: 9, nonZero: map[string]float64{
			"webitel.dcctl.voter.name=dc_a,webitel.dcctl.voter.state=ok":             1,
			"webitel.dcctl.voter.name=dc_b,webitel.dcctl.voter.state=ok":             1,
			"webitel.dcctl.voter.name=arbiter,webitel.dcctl.voter.state=unreachable": 1,
		}},
		"webitel.dcctl.datacenter.status": {kind: kindUpDownCounter, unit: "1", count: 2 * 8, nonZero: map[string]float64{
			dcA + ",webitel.dcctl.datacenter.state=active":  1,
			dcB + ",webitel.dcctl.datacenter.state=standby": 1,
		}},
		"webitel.dcctl.alarm.count": {kind: kindUpDownCounter, unit: "{alarm}", count: 7 + 8*2 + 3 + 1, nonZero: map[string]float64{
			dcB + ",webitel.dcctl.alarm.name=db_degraded":                                 1,
			"webitel.dcctl.alarm.name=voter_unreachable,webitel.dcctl.voter.name=arbiter": 1,
		}},
		"webitel.dcctl.job.count": {kind: kindUpDownCounter, unit: "{job}", count: 4 * 4, nonZero: map[string]float64{
			dcA + ",webitel.dcctl.job.stage=1,webitel.dcctl.job.state=healthy": 2,
			dcA + ",webitel.dcctl.job.stage=2,webitel.dcctl.job.state=healthy": 1,
			dcB + ",webitel.dcctl.job.stage=1,webitel.dcctl.job.state=stopped": 2,
			dcB + ",webitel.dcctl.job.stage=2,webitel.dcctl.job.state=stopped": 1,
		}},
		"webitel.dcctl.source.status": {kind: kindUpDownCounter, unit: "1", count: 7 * 3, nonZero: map[string]float64{
			dcA + ",webitel.dcctl.source.state=ok,webitel.dcctl.source.system=consul":       1,
			dcA + ",webitel.dcctl.source.state=ok,webitel.dcctl.source.system=nomad":        1,
			dcA + ",webitel.dcctl.source.state=ok,webitel.dcctl.source.system=patroni":      1,
			dcB + ",webitel.dcctl.source.state=ok,webitel.dcctl.source.system=consul":       1,
			dcB + ",webitel.dcctl.source.state=ok,webitel.dcctl.source.system=nomad":        1,
			dcB + ",webitel.dcctl.source.state=partial,webitel.dcctl.source.system=patroni": 1,
			"webitel.dcctl.source.state=unreachable,webitel.dcctl.source.system=arbiter":    1,
		}},
	}

	for name, want := range oneHot {
		s := got[name]
		assert.Equal(t, want.kind, s.kind, name)
		assert.Equal(t, want.unit, s.unit, name)
		assert.Len(t, s.points, want.count, name)
		assert.Equal(t, want.nonZero, nonZero(s.points), name)
	}

	assert.Len(t, got, len(full)+len(oneHot), "every metric is checked")
}

func TestStatusMetrics_noCycle(t *testing.T) {
	assert.Empty(t, collectStatus(t, observing(nil)))
}

func TestStatusMetrics_staleCycle(t *testing.T) {
	src := observing(representative())

	assert.NotEmpty(t, collectStatusAt(t, src, observeTime.Add(maxAge)), "at the limit")
	assert.Empty(t, collectStatusAt(t, src, observeTime.Add(maxAge+time.Second)), "past the limit")
}

func TestStatusMetrics_alarmZeros(t *testing.T) {
	obs := representative()
	obs.Alarms = nil

	points := collectStatus(t, observing(obs))["webitel.dcctl.alarm.count"].points

	want := make(map[string]float64)

	for _, id := range []status.AlarmID{
		status.AlarmConflict, status.AlarmUncommitted, status.AlarmMultiplePrimaries, status.AlarmDBLeaderMismatch,
		status.AlarmLeaseLost, status.AlarmQuorumLost, status.AlarmOperationInterrupted,
	} {
		want["webitel.dcctl.alarm.name="+string(id)] = 0
	}

	for _, id := range []status.AlarmID{
		status.AlarmStale, status.AlarmSplitBrain, status.AlarmDBSplit, status.AlarmDBPrimaryMissing,
		status.AlarmDatacenterUnreachable, status.AlarmAppsDegraded, status.AlarmDBDegraded, status.AlarmStandbyNotFollowing,
	} {
		want[dcA+",webitel.dcctl.alarm.name="+string(id)] = 0
		want[dcB+",webitel.dcctl.alarm.name="+string(id)] = 0
	}

	for _, v := range []string{"dc_a", "dc_b", "arbiter"} {
		want["webitel.dcctl.alarm.name=voter_unreachable,webitel.dcctl.voter.name="+v] = 0
	}

	want["webitel.dcctl.alarm.name=arbiter_uninitialized,webitel.dcctl.voter.name=arbiter"] = 0

	assert.Equal(t, want, points)
	assert.Len(t, want, len(status.AlarmIDs())+8+2, "every alarm has a point")
}

func TestStatusMetrics_alarmOfNoDatacenter(t *testing.T) {
	obs := representative()
	obs.Alarms = []status.Alarm{{ID: status.AlarmSplitBrain, Severity: status.SeverityCritical}}

	points := collectStatus(t, observing(obs))["webitel.dcctl.alarm.count"].points

	assert.Equal(t, map[string]float64{"webitel.dcctl.alarm.name=split_brain": 1}, nonZero(points))
	assert.Contains(t, points, dcA+",webitel.dcctl.alarm.name=split_brain")
}

func TestStatusMetrics_unknownValuesNotObserved(t *testing.T) {
	obs := representative()
	obs.Status.Effective = nil
	obs.Status.Datacenters[0].State = status.StateUndetermined
	obs.Status.Datacenters[1].State = status.StateUndetermined
	obs.Status.Datacenters[1].Patroni.LagBytes = nil
	obs.Status.Datacenters[1].Patroni.Divergence = status.Divergence{}
	obs.Status.Datacenters[1].Patroni.Timeline = nil
	obs.Status.Voters[1].Reachable = false
	obs.Status.Voters[1].Status = coord.StatusUnreachable
	obs.Lease = coord.LeaseStatus{State: coord.LeaseUnknown, Reason: coord.LeaseReasonStateUndetermined}

	got := collectStatus(t, observing(obs))

	for _, name := range []string{"webitel.dcctl.generation", "webitel.dcctl.lease.age", "webitel.dcctl.patroni.lag", "webitel.dcctl.patroni.divergence"} {
		assert.Empty(t, got[name].points, name)
	}

	assert.Equal(t, map[string]float64{"webitel.dcctl.voter.name=dc_a": 7}, got["webitel.dcctl.voter.generation"].points)
	assert.Equal(t, map[string]float64{dcA: 3}, got["webitel.dcctl.patroni.timeline"].points)
}

// The registry has no undetermined yet: such a datacenter is in none of the states.
func TestStatusMetrics_undeterminedDatacenter(t *testing.T) {
	obs := representative()
	obs.Status.Datacenters[1].State = status.StateUndetermined

	points := collectStatus(t, observing(obs))["webitel.dcctl.datacenter.status"].points

	assert.Equal(t, map[string]float64{dcA + ",webitel.dcctl.datacenter.state=active": 1}, nonZero(points))
	assert.Len(t, points, 2*8)
	assert.Contains(t, points, dcB+",webitel.dcctl.datacenter.state=standby")
	assert.NotContains(t, points, dcB+",webitel.dcctl.datacenter.state=undetermined")
}

func TestStatusMetrics_activeHasNoLag(t *testing.T) {
	obs := representative()
	obs.Status.Datacenters[0].Patroni.LagBytes = ptr[int64](10)

	got := collectStatus(t, observing(obs))

	assert.Equal(t, map[string]float64{dcB: 4096}, got["webitel.dcctl.patroni.lag"].points)
}

func TestStatusMetrics_unreachablePatroniAndNomad(t *testing.T) {
	obs := representative()
	dc := &obs.Status.Datacenters[1]
	dc.Sources = []status.Source{reachable(status.SystemConsul), unreachable(status.SystemNomad), unreachable(status.SystemPatroni)}

	for i := range dc.Patroni.Members {
		dc.Patroni.Members[i].Reachable = false
	}

	got := collectStatus(t, observing(obs))

	assert.Equal(t, map[string]float64{dcA: 3}, got["webitel.dcctl.patroni.timeline"].points)
	assert.Equal(t, map[string]float64{dcA: 100000}, got["webitel.dcctl.patroni.wal.position"].points)
	assert.Empty(t, got["webitel.dcctl.patroni.lag"].points)
	assert.Empty(t, got["webitel.dcctl.patroni.divergence"].points)

	members := got["webitel.dcctl.patroni.member.count"].points
	assert.Len(t, members, 5, "only the members of dc_a")
	assert.NotContains(t, members, dcB+",webitel.dcctl.patroni.role=unknown", "the last known members are not counted")

	jobs := got["webitel.dcctl.job.count"].points
	assert.Len(t, jobs, 2*4, "only the jobs of dc_a")
	assert.NotContains(t, jobs, dcB+",webitel.dcctl.job.stage=1,webitel.dcctl.job.state=stopped")

	assert.Equal(t, 1.0, got["webitel.dcctl.source.status"].points[dcB+",webitel.dcctl.source.state=unreachable,webitel.dcctl.source.system=patroni"])
	assert.Equal(t, 1.0, got["webitel.dcctl.source.status"].points[dcB+",webitel.dcctl.source.state=unreachable,webitel.dcctl.source.system=nomad"])
}

func TestStatusMetrics_patroniNeverReached(t *testing.T) {
	obs := representative()
	dc := &obs.Status.Datacenters[1]
	dc.Sources[2] = status.Source{System: status.SystemPatroni, Reason: "no_endpoints"}
	dc.Patroni = status.Patroni{}

	got := collectStatus(t, observing(obs))

	for _, name := range []string{
		"webitel.dcctl.patroni.member.count", "webitel.dcctl.patroni.timeline", "webitel.dcctl.patroni.wal.position",
		"webitel.dcctl.patroni.lag", "webitel.dcctl.patroni.divergence",
	} {
		for attrs := range got[name].points {
			assert.NotContains(t, attrs, dcB, name)
		}
	}

	assert.Equal(t, 1.0, got["webitel.dcctl.source.status"].points[dcB+",webitel.dcctl.source.state=unreachable,webitel.dcctl.source.system=patroni"])
}

func TestStatusMetrics_leaderNotAnswering(t *testing.T) {
	obs := representative()
	obs.Status.Datacenters[1].Patroni.Members[0].Reachable = false

	got := collectStatus(t, observing(obs))

	assert.Equal(t, map[string]float64{dcA: 100000}, got["webitel.dcctl.patroni.wal.position"].points)
}

func TestStatusMetrics_invalidVoter(t *testing.T) {
	obs := representative()
	obs.Status.Voters[1].Status = coord.StatusInvalid
	obs.Status.Voters[1].Generation = 0

	got := collectStatus(t, observing(obs))

	voters := got["webitel.dcctl.voter.status"].points
	for _, state := range []string{"ok", "unreachable", "uninitialized"} {
		assert.Zero(t, voters["webitel.dcctl.voter.name=dc_b,webitel.dcctl.voter.state="+state], state)
	}

	assert.NotContains(t, got["webitel.dcctl.voter.generation"].points, "webitel.dcctl.voter.name=dc_b")
}

func TestStatusMetrics_consulPartial(t *testing.T) {
	tests := []struct {
		name      string
		catalog   bool
		kv        bool
		wantState string
	}{
		{name: "both", catalog: true, kv: true, wantState: "ok"},
		{name: "catalog_only", catalog: true, wantState: "partial"},
		{name: "kv_only", kv: true, wantState: "partial"},
		{name: "none", wantState: "unreachable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := representative()
			obs.Status.Datacenters[1].Sources[0].Reachable = tt.catalog
			obs.Status.Voters[1].Reachable = tt.kv

			points := collectStatus(t, observing(obs))["webitel.dcctl.source.status"].points

			assert.Equal(t, 1.0, points[dcB+",webitel.dcctl.source.state="+tt.wantState+",webitel.dcctl.source.system=consul"])
		})
	}
}
