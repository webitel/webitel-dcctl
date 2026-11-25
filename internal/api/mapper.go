package api

import (
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// mapper converts one status model into the proto. A value the proto cannot
// hold, such as a count over int32, sets err: the caller then answers with
// an error rather than a wrong number. Only the first failure is kept.
type mapper struct {
	st *status.Status
	// address is listen.peer of this instance, shown in the observer.
	address string
	err     error
}

func newMapper(st *status.Status, address string) *mapper {
	return &mapper{st: st, address: address}
}

func (m *mapper) fail(err error) {
	if m.err == nil {
		m.err = err
	}
}

func (m *mapper) i32(what string, v int) int32 {
	if v < math.MinInt32 || v > math.MaxInt32 {
		m.fail(fmt.Errorf("%s %d does not fit int32", what, v))

		return 0
	}

	return int32(v)
}

func (m *mapper) i64(what string, v uint64) int64 {
	if v > math.MaxInt64 {
		m.fail(fmt.Errorf("%s %d does not fit int64", what, v))

		return 0
	}

	return int64(v)
}

// timestamp returns nil for the zero time, which the model uses for "not
// known".
func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}

	return timestamppb.New(t)
}

func (m *mapper) observer() *dcctlv1.Instance {
	return &dcctlv1.Instance{
		Node:       m.st.Observer.Node,
		Datacenter: datacenterName(m.st.Observer.Datacenter),
		Address:    m.address,
	}
}

// status maps everything but the lease and the alarms, which the model does
// not hold.
func (m *mapper) status() *dcctlv1.Status {
	st := m.st
	out := &dcctlv1.Status{
		Name:                     "status",
		Observer:                 m.observer(),
		ObserveTime:              timestamp(st.ObserveTime),
		CycleDuration:            durationpb.New(st.CycleDuration),
		Interval:                 durationpb.New(st.Interval),
		Profile:                  profileOf(st.Profile),
		Initialized:              st.Initialized,
		DetectedActiveDatacenter: datacenterName(st.DetectedActiveDatacenter),
		Quorum: &dcctlv1.Status_Quorum{
			Available:      st.Quorum.Available,
			Mode:           quorumModeOf(st.Quorum.Mode),
			ReachableCount: m.i32("quorum.reachable_count", st.Quorum.ReachableCount),
			RequiredCount:  m.i32("quorum.required_count", st.Quorum.RequiredCount),
			TotalCount:     m.i32("quorum.total_count", st.Quorum.TotalCount),
		},
		Operation: st.Operation,
	}

	if e := st.Effective; e != nil {
		out.ActiveDatacenter = datacenterName(e.ActiveDatacenter)
		out.Generation = e.Generation
		out.Forced = e.Forced
		out.PatroniTimeline = e.PatroniTimeline
		out.UpdateTime = timestamp(e.UpdatedAt)

		for _, dc := range e.NeedsRecovery {
			out.NeedsRecoveryDatacenters = append(out.NeedsRecoveryDatacenters, datacenterName(dc))
		}
	}

	for _, v := range st.Voters {
		out.Voters = append(out.Voters, &dcctlv1.Status_Voter{
			Type:             voterTypeOf(v.Type),
			Datacenter:       datacenterName(v.Datacenter),
			Reachable:        v.Reachable,
			Error:            sourceErrorOf(v.Reason),
			Generation:       v.Generation,
			ActiveDatacenter: datacenterName(v.ActiveDatacenter),
			Forced:           v.Forced,
			Valid:            v.Valid,
			Initialized:      v.Initialized,
		})
	}

	for i := range st.Datacenters {
		out.Datacenters = append(out.Datacenters, m.datacenter(&st.Datacenters[i], false))
	}

	return out
}

// activeDatacenter is the active datacenter of the effective state, "" when
// the voters do not tell it.
func (m *mapper) activeDatacenter() string {
	if m.st.Effective == nil {
		return ""
	}

	return m.st.Effective.ActiveDatacenter
}

func (m *mapper) warmStandby() bool {
	return m.st.Profile == config.ProfileWarmStandby
}
