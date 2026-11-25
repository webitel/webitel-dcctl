package telemetry

import (
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/status"
)

var observeTime = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

// observations is a tracker that returns what the test set.
type observations struct {
	current atomic.Pointer[status.Observed]
}

func observing(o *status.Observed) *observations {
	src := new(observations)
	src.current.Store(o)

	return src
}

func (o *observations) Current() *status.Observed {
	return o.current.Load()
}

func ptr[T any](v T) *T {
	return &v
}

func reachable(sys status.System) status.Source {
	return status.Source{System: sys, Reachable: true, ObserveTime: observeTime}
}

func unreachable(sys status.System) status.Source {
	return status.Source{System: sys, Reason: "timeout", UnreachableSince: observeTime.Add(-time.Minute)}
}

// representative is warm_standby with dc_a active and dc_b a standby that lags and
// diverged, one of its Patroni members not answering; the arbiter does not answer;
// dc_a holds the lease.
func representative() *status.Observed {
	return &status.Observed{
		Status: status.Status{
			Observer:      collect.Instance{Datacenter: "dc_a", Node: "node-a"},
			ObserveTime:   observeTime,
			CycleDuration: 1500 * time.Millisecond,
			Profile:       config.ProfileWarmStandby,
			Initialized:   true,
			Effective:     &kv.State{Generation: 7, ActiveDatacenter: "dc_a"},
			Voters: []status.Voter{
				{
					Name: "dc_a", Type: status.VoterDatacenter, Datacenter: "dc_a", Reachable: true,
					Status: coord.StatusPresent, Generation: 7, ActiveDatacenter: "dc_a", Valid: true,
				},
				{
					Name: "dc_b", Type: status.VoterDatacenter, Datacenter: "dc_b", Reachable: true,
					Status: coord.StatusPresent, Generation: 7, ActiveDatacenter: "dc_a", Valid: true,
				},
				{Name: coord.ArbiterName, Type: status.VoterArbiter, Status: coord.StatusUnreachable, Reason: "timeout"},
			},
			Quorum: status.Quorum{Available: true, Mode: status.QuorumQuorum, ReachableCount: 2, RequiredCount: 2, TotalCount: 3},
			Datacenters: []status.Datacenter{
				{
					Name:  "dc_a",
					State: status.StateActive,
					Sources: []status.Source{
						reachable(status.SystemConsul), reachable(status.SystemNomad), reachable(status.SystemPatroni),
					},
					Patroni: status.Patroni{
						Leader: "pg-a1", Role: status.RolePrimary, Timeline: ptr[int64](3),
						Divergence: status.Divergence{Known: true},
						Members: []status.Member{
							{
								Name: "pg-a1", Datacenter: "dc_a", Reachable: true, Role: status.RolePrimary,
								Xlog: status.Xlog{Location: ptr[int64](100000)},
							},
							{
								Name: "pg-a2", Datacenter: "dc_a", Reachable: true, Role: status.RoleSyncStandby,
								Xlog: status.Xlog{ReceivedLocation: ptr[int64](100000)},
							},
						},
					},
					Nomad: status.Nomad{Stages: []status.Stage{
						{Stage: 1, JobCounts: status.JobCounts{Healthy: 2}},
						{Stage: 2, JobCounts: status.JobCounts{Healthy: 1}},
					}},
				},
				{
					Name:  "dc_b",
					State: status.StateStandby,
					Sources: []status.Source{
						reachable(status.SystemConsul), reachable(status.SystemNomad), reachable(status.SystemPatroni),
					},
					Patroni: status.Patroni{
						Leader: "pg-b1", Role: status.RoleStandbyLeader, Timeline: ptr[int64](3),
						UpstreamDatacenter: "dc_a", LagBytes: ptr[int64](4096),
						Divergence: status.Divergence{Known: true, Diverged: true, LSN: 95392, Bytes: 512},
						Members: []status.Member{
							{
								Name: "pg-b1", Datacenter: "dc_b", Reachable: true, Role: status.RoleStandbyLeader,
								Xlog: status.Xlog{Location: ptr[int64](1), ReceivedLocation: ptr[int64](95904)},
							},
							{Name: "pg-b2", Datacenter: "dc_b", Reason: "timeout", Role: status.RoleReplica},
						},
					},
					Nomad: status.Nomad{Stages: []status.Stage{
						{Stage: 1, JobCounts: status.JobCounts{Stopped: 2}},
						{Stage: 2, JobCounts: status.JobCounts{Stopped: 1}},
					}},
				},
			},
		},
		Lease: coord.LeaseStatus{
			State:  coord.LeaseHeld,
			Reason: coord.LeaseReasonRenewed,
			Lease:  kv.Lease{Holder: "dc_a", Generation: 7, Counter: 40},
			Since:  observeTime.Add(-10 * time.Second),
		},
		Alarms: []status.Alarm{
			{
				ID: status.AlarmDBDegraded, Severity: status.SeverityWarning, Datacenter: "dc_b",
				StartTime: observeTime.Add(-time.Minute), Detail: "Patroni member pg-b2 of dc_b is not in order",
			},
			{
				ID: status.AlarmVoterUnreachable, Severity: status.SeverityWarning, Voter: coord.ArbiterName,
				VoterType: status.VoterArbiter, StartTime: observeTime.Add(-time.Minute),
				Detail: "voter arbiter does not answer: timeout",
			},
		},
	}
}

// series is one metric as collected: its instrument kind, unit and the value of every
// attribute set, keyed by the encoded set.
type series struct {
	kind   string
	unit   string
	points map[string]float64
}

const (
	kindGauge         = "gauge"
	kindUpDownCounter = "updowncounter"
	kindHistogram     = "histogram"
)

// collectSeries collects reader and returns every metric by name; a histogram point
// holds its sum.
func collectSeries(t *testing.T, reader *sdkmetric.ManualReader) map[string]series {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	out := make(map[string]series)

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			s := series{unit: m.Unit, points: make(map[string]float64)}

			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				s.kind = kindGauge
				for _, p := range d.DataPoints {
					s.points[encode(p.Attributes)] = float64(p.Value)
				}
			case metricdata.Gauge[float64]:
				s.kind = kindGauge
				for _, p := range d.DataPoints {
					s.points[encode(p.Attributes)] = p.Value
				}
			case metricdata.Sum[int64]:
				require.False(t, d.IsMonotonic, "%s is monotonic", m.Name)

				s.kind = kindUpDownCounter
				for _, p := range d.DataPoints {
					s.points[encode(p.Attributes)] = float64(p.Value)
				}
			case metricdata.Histogram[float64]:
				s.kind = kindHistogram
				for _, p := range d.DataPoints {
					s.points[encode(p.Attributes)] = p.Sum
				}
			default:
				t.Fatalf("%s has data of type %T", m.Name, m.Data)
			}

			out[m.Name] = s
		}
	}

	return out
}

func encode(s attribute.Set) string {
	return s.Encoded(attribute.DefaultEncoder())
}

// nonZero returns the points with a value other than 0.
func nonZero(points map[string]float64) map[string]float64 {
	out := maps.Clone(points)
	maps.DeleteFunc(out, func(_ string, v float64) bool { return v == 0 })

	return out
}
