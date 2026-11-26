package api

import (
	"time"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// The fixtures are the model behind the swagger examples.

const observerAddress = "10.1.0.81:10023"

var observeTime = time.Date(2026, 11, 23, 10, 14, 55, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func reachableSources() []status.Source {
	return []status.Source{
		{System: status.SystemConsul, Reachable: true, ObserveTime: observeTime},
		{System: status.SystemNomad, Reachable: true, ObserveTime: observeTime},
		{System: status.SystemPatroni, Reachable: true, ObserveTime: observeTime},
	}
}

func exampleDCA() status.Datacenter {
	return status.Datacenter{
		Name:    "dc_a",
		State:   status.StateActive,
		Health:  status.HealthOK,
		Sources: reachableSources(),
		Patroni: status.Patroni{
			Leader: "pg-a1", Role: status.RolePrimary, Timeline: ptr[int64](5),
			Divergence: status.Divergence{Known: true},
		},
		Nomad: status.Nomad{ReadyNodeCount: 3, JobCounts: status.JobCounts{Healthy: 12}},
	}
}

func exampleMember(name, host string, role status.Role, lag *int64) status.Member {
	return status.Member{
		Name: name, Host: host, APIURL: "https://" + host + ":8008", Datacenter: "dc_b",
		Reachable: true, Role: role, PatroniState: "streaming", Timeline: ptr[int64](5),
		ReplicationState: "streaming",
		Xlog: status.Xlog{
			ReceivedLocation: ptr[int64](83886176), ReplayedLocation: ptr[int64](83886176),
			ReplayedTimestamp: "2026-11-23 10:14:54.000000+00:00",
		},
		LagBytes: lag, Version: "4.1.0", Usable: true,
	}
}

func exampleDCB() status.Datacenter {
	return status.Datacenter{
		Name:    "dc_b",
		State:   status.StateStandby,
		Health:  status.HealthOK,
		Sources: reachableSources(),
		Patroni: status.Patroni{
			Leader: "pg-b1", Role: status.RoleStandbyLeader, Timeline: ptr[int64](5),
			StandbyClusterHost: "10.1.0.95", UpstreamDatacenter: "dc_a", LagBytes: ptr[int64](0),
			Divergence: status.Divergence{Known: true},
			Members: []status.Member{
				exampleMember("pg-b1", "10.2.0.95", status.RoleStandbyLeader, nil),
				exampleMember("pg-b2", "10.2.0.96", status.RoleReplica, ptr[int64](0)),
			},
		},
		Nomad: status.Nomad{
			ReadyNodeCount: 3,
			JobCounts:      status.JobCounts{Stopped: 12},
			Stages: []status.Stage{
				{Stage: 1, JobCounts: status.JobCounts{Stopped: 1}, Jobs: []status.Job{{
					Name: "freeswitch", Stage: 1, Type: "system", Stop: true, State: status.JobStopped,
					AllocationCounts: map[string]int{"complete": 3}, ModifyIndex: 4182, ExpectedNodeCount: 3,
				}}},
				{Stage: 3, JobCounts: status.JobCounts{Stopped: 1}, Jobs: []status.Job{{
					Name: "webitel-core-api", Stage: 3, Type: "service", Stop: true, State: status.JobStopped,
					AllocationCounts: map[string]int{"complete": 2}, ModifyIndex: 4190,
				}}},
			},
			UnmanagedJobs: []status.UnmanagedJob{{Name: "node-exporter", NomadStatus: "running"}},
		},
		InfoChecks: []check.Check{{
			ID: check.InfoRabbitMQ, Result: check.Pass, Datacenter: "dc_b",
			Detail: "rabbitmq: 3 of 3 passing",
			Facts:  map[string]any{"service": "rabbitmq", "passing": 3, "total": 3},
		}},
	}
}

func exampleStatus() status.Status {
	return status.Status{
		Observer:                 collect.Instance{Datacenter: "dc_a", Node: "dcctl-a1"},
		ObserveTime:              observeTime,
		CycleDuration:            420 * time.Millisecond,
		Interval:                 5 * time.Second,
		Profile:                  config.ProfileWarmStandby,
		Initialized:              true,
		DetectedActiveDatacenter: "dc_a",
		Effective: &kv.State{
			Generation: 7, ActiveDatacenter: "dc_a", PatroniTimeline: 5,
			UpdatedAt: time.Date(2026, 11, 20, 16, 2, 11, 0, time.UTC),
		},
		Voters: []status.Voter{
			{
				Name: "dc_a", Type: status.VoterDatacenter, Datacenter: "dc_a", Reachable: true,
				Generation: 7, ActiveDatacenter: "dc_a", Valid: true,
			},
			{
				Name: "dc_b", Type: status.VoterDatacenter, Datacenter: "dc_b", Reachable: true,
				Generation: 7, ActiveDatacenter: "dc_a", Valid: true,
			},
			{Name: "arbiter", Type: status.VoterArbiter, Reason: source.ReasonTimeout},
		},
		Quorum: status.Quorum{
			Available: true, Mode: status.QuorumQuorum, ReachableCount: 2, RequiredCount: 2, TotalCount: 3,
		},
		Datacenters: []status.Datacenter{exampleDCA(), exampleDCB()},
	}
}

// statusExampleSkips are the paths of the GetStatus example the model does
// not give: the lease and the alarms come from elsewhere.
func statusExampleSkips() unproducible {
	skip := prefixed("datacenters.*.", skipEndpoint)
	skip["lease"] = "the lease is not part of the status model"
	skip["alarms"] = "alarms come from the alarm provider"

	return skip
}
