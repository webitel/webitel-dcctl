package cli

import (
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

var testTime = time.Date(2026, 11, 23, 10, 15, 0, 0, time.UTC)

func ts(offset time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(testTime.Add(offset))
}

func int64p(v int64) *int64 { return &v }

func testStatus() *dcctlv1.Status {
	return &dcctlv1.Status{
		Name:                     "status",
		Observer:                 &dcctlv1.Instance{Node: "dcctl-a1", Datacenter: "datacenters/dc_a", Address: "10.1.0.81:10023"},
		ObserveTime:              ts(0),
		Profile:                  dcctlv1.Status_WARM_STANDBY,
		Initialized:              true,
		DetectedActiveDatacenter: "datacenters/dc_a",
		ActiveDatacenter:         "datacenters/dc_a",
		Generation:               7,
		Quorum:                   &dcctlv1.Status_Quorum{Available: true, Mode: dcctlv1.Status_Quorum_QUORUM, ReachableCount: 3, RequiredCount: 2, TotalCount: 3},
		Lease:                    &dcctlv1.Status_Lease{Holder: "dc_a", Generation: 7},
		Alarms: []*dcctlv1.Status_Alarm{
			{Type: dcctlv1.Status_Alarm_STALE, Severity: dcctlv1.Status_Alarm_WARNING, Datacenter: "datacenters/dc_b", Detail: "dc_b is stale"},
		},
		Datacenters: []*dcctlv1.Datacenter{testDatacenter("dc_a", dcctlv1.Datacenter_ACTIVE), testDatacenter("dc_b", dcctlv1.Datacenter_STANDBY)},
	}
}

func testDatacenter(name string, state dcctlv1.Datacenter_State) *dcctlv1.Datacenter {
	role := dcctlv1.Datacenter_Patroni_PRIMARY
	if state == dcctlv1.Datacenter_STANDBY {
		role = dcctlv1.Datacenter_Patroni_STANDBY_LEADER
	}

	return &dcctlv1.Datacenter{
		Name:        "datacenters/" + name,
		State:       state,
		Health:      dcctlv1.Datacenter_OK,
		ObserveTime: ts(0),
		Observer:    &dcctlv1.Instance{Node: "dcctl-a1", Datacenter: "datacenters/dc_a"},
		Sources: []*dcctlv1.Source{
			{System: dcctlv1.Source_CONSUL, Endpoint: "10.1.0.89:8500", Reachable: true, ObserveTime: ts(0)},
		},
		Patroni: &dcctlv1.Datacenter_Patroni{
			Leader: "pg-" + name, Role: role, Timeline: int64p(5), LagBytes: int64p(0),
			Members: []*dcctlv1.Datacenter_Patroni_Member{
				{Member: "pg-" + name, Host: "10.1.0.95", Reachable: true, Role: role, PatroniState: "running", Usable: true},
			},
		},
		Nomad: &dcctlv1.Datacenter_Nomad{Leader: "10.1.0.91:4647", ReadyNodeCount: 3},
	}
}

func testOperation(id string, state dcctlv1.Operation_State) *dcctlv1.Operation {
	op := &dcctlv1.Operation{
		Name:               "operations/" + id,
		Type:               dcctlv1.Operation_SWITCH,
		State:              state,
		Target:             "datacenters/dc_b",
		Initiator:          &dcctlv1.Initiator{User: "operator", Address: "10.1.0.15"},
		Leader:             &dcctlv1.Instance{Node: "dcctl-b1", Datacenter: "datacenters/dc_b"},
		Mode:               dcctlv1.Operation_QUORUM,
		PreviousGeneration: 7,
		NewGeneration:      8,
		CreateTime:         ts(0),
		UpdateTime:         ts(time.Minute),
		Steps: []*dcctlv1.Operation_Step{
			{Index: 1, Type: dcctlv1.Operation_Step_APPS_STOP, Stage: 3, State: dcctlv1.Operation_Step_SUCCEEDED, Datacenter: "datacenters/dc_a"},
			{Index: 2, Type: dcctlv1.Operation_Step_GENERATION, State: dcctlv1.Operation_Step_PENDING, PointOfNoReturn: true},
		},
	}
	if state != dcctlv1.Operation_RUNNING {
		op.EndTime = ts(2 * time.Minute)
	}

	return op
}

func testCheck(id string, result dcctlv1.Check_Result, effect dcctlv1.Check_Effect, detail string) *dcctlv1.Check {
	return &dcctlv1.Check{Id: id, Result: result, Effect: effect, Detail: detail}
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}

	return s
}
