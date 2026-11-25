package api

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// datacenter maps d. Without detail it gives the summary fields that
// ListDatacenters and GetStatus carry: no members, nodes, jobs, info checks
// or observer.
func (m *mapper) datacenter(d *status.Datacenter, detail bool) *dcctlv1.Datacenter {
	out := &dcctlv1.Datacenter{
		Name:          datacenterName(d.Name),
		State:         stateOf(d.State),
		Health:        healthOf(d.Health),
		NeedsRecovery: d.NeedsRecovery,
		Patroni:       m.patroni(d.Name, &d.Patroni, detail),
		Nomad:         m.nomad(&d.Nomad, detail),
	}

	for _, r := range d.HealthReasons {
		out.HealthReasons = append(out.HealthReasons, string(r))
	}

	for _, sys := range []status.System{status.SystemConsul, status.SystemNomad, status.SystemPatroni} {
		s := d.Source(sys)
		out.Sources = append(out.Sources, &dcctlv1.Source{
			System:               systemOf(s.System),
			Reachable:            s.Reachable,
			Error:                sourceErrorOf(s.Reason),
			UnreachableStartTime: timestamp(s.UnreachableSince),
			ObserveTime:          timestamp(s.ObserveTime),
		})
	}

	if !detail {
		return out
	}

	out.Observer = m.observer()
	out.ObserveTime = timestamp(m.st.ObserveTime)

	for _, c := range d.InfoChecks {
		out.InfoChecks = append(out.InfoChecks, m.checkOf(c))
	}

	return out
}

func (m *mapper) patroni(dc string, p *status.Patroni, detail bool) *dcctlv1.Datacenter_Patroni {
	out := &dcctlv1.Datacenter_Patroni{
		Leader:             p.Leader,
		Role:               roleOf(p.Role),
		Timeline:           deref(p.Timeline),
		Paused:             p.Paused,
		StandbyClusterHost: p.StandbyClusterHost,
		UpstreamDatacenter: datacenterName(p.UpstreamDatacenter),
		LagBytes:           deref(p.LagBytes),
		DivergenceState:    m.divergence(dc, p.Divergence),
	}

	if out.GetDivergenceState() == dcctlv1.Datacenter_Patroni_DIVERGED {
		out.DivergenceLsn = formatLSN(p.Divergence.LSN)
		out.DivergenceBytes = p.Divergence.Bytes
	}

	if !detail {
		return out
	}

	for i := range p.Members {
		out.Members = append(out.Members, m.member(&p.Members[i]))
	}

	return out
}

// divergence is UNSPECIFIED where there is nothing to compare: for the
// active datacenter and in stretch. A divergence the model could not tell is
// UNKNOWN, never NOT_DIVERGED.
func (m *mapper) divergence(dc string, d status.Divergence) dcctlv1.Datacenter_Patroni_DivergenceState {
	switch {
	case !m.warmStandby(), dc == m.activeDatacenter():
		return dcctlv1.Datacenter_Patroni_DIVERGENCE_STATE_UNSPECIFIED
	case !d.Known:
		return dcctlv1.Datacenter_Patroni_UNKNOWN
	case d.Diverged:
		return dcctlv1.Datacenter_Patroni_DIVERGED
	}

	return dcctlv1.Datacenter_Patroni_NOT_DIVERGED
}

func (m *mapper) member(p *status.Member) *dcctlv1.Datacenter_Patroni_Member {
	return &dcctlv1.Datacenter_Patroni_Member{
		Member:           p.Name,
		Host:             p.Host,
		ApiUri:           p.APIURL,
		Reachable:        p.Reachable,
		Error:            sourceErrorOf(p.Reason),
		Role:             roleOf(p.Role),
		PatroniState:     p.PatroniState,
		Timeline:         deref(p.Timeline),
		ReplicationState: p.ReplicationState,
		Xlog: &dcctlv1.Datacenter_Patroni_Xlog{
			Location:         deref(p.Xlog.Location),
			ReceivedLocation: deref(p.Xlog.ReceivedLocation),
			ReplayedLocation: deref(p.Xlog.ReplayedLocation),
			ReplayTime:       replayTime(p.Xlog.ReplayedTimestamp),
		},
		LagBytes:         deref(p.LagBytes),
		Version:          p.Version,
		FailoverPriority: m.i32("failover_priority", p.FailoverPriority),
		NoFailover:       p.NoFailover,
		Usable:           p.Usable,
		UnusableReason:   unusableOf(p.UnusableReason),
	}
}

// replayTimeLayout is how Patroni writes replayed_timestamp, the PostgreSQL
// timestamptz text.
const replayTimeLayout = "2006-01-02 15:04:05.999999999Z07:00"

// replayTime returns nil when Patroni did not report the time or wrote it in
// a way it does not usually.
func replayTime(s string) *timestamppb.Timestamp {
	t, err := time.Parse(replayTimeLayout, s)
	if err != nil {
		return nil
	}

	return timestamppb.New(t)
}

func (m *mapper) nomad(n *status.Nomad, detail bool) *dcctlv1.Datacenter_Nomad {
	out := &dcctlv1.Datacenter_Nomad{
		ReadyNodeCount:      m.i32("ready_node_count", n.ReadyNodeCount),
		IneligibleNodeCount: m.i32("ineligible_node_count", n.IneligibleNodeCount),
		DrainingNodeCount:   m.i32("draining_node_count", n.DrainingNodeCount),
		DownNodeCount:       m.i32("down_node_count", n.DownNodeCount),
		JobCounts:           m.jobCounts(n.JobCounts),
	}

	if !detail {
		return out
	}

	for _, node := range n.NotReadyNodes {
		out.NotReadyNodes = append(out.NotReadyNodes, &dcctlv1.Datacenter_Nomad_Node{
			Node: node.Name, Reason: nodeReasonOf(node.Reason),
		})
	}

	for _, st := range n.Stages {
		stage := &dcctlv1.Datacenter_Nomad_Stage{
			Stage:     m.i32("stage", st.Stage),
			JobCounts: m.jobCounts(st.JobCounts),
		}

		for i := range st.Jobs {
			stage.Jobs = append(stage.Jobs, m.job(&st.Jobs[i]))
		}

		out.Stages = append(out.Stages, stage)
	}

	for _, j := range n.UnmanagedJobs {
		out.UnmanagedJobs = append(out.UnmanagedJobs, &dcctlv1.Datacenter_Nomad_UnmanagedJob{
			Job: j.Name, NomadStatus: j.NomadStatus,
		})
	}

	return out
}

func (m *mapper) jobCounts(c status.JobCounts) *dcctlv1.Datacenter_Nomad_JobCounts {
	return &dcctlv1.Datacenter_Nomad_JobCounts{
		StoppedCount:   m.i32("stopped_count", c.Stopped),
		StoppingCount:  m.i32("stopping_count", c.Stopping),
		HealthyCount:   m.i32("healthy_count", c.Healthy),
		UnhealthyCount: m.i32("unhealthy_count", c.Unhealthy),
	}
}

func (m *mapper) job(j *status.Job) *dcctlv1.Datacenter_Nomad_Job {
	out := &dcctlv1.Datacenter_Nomad_Job{
		Job:               j.Name,
		Stage:             m.i32("stage", j.Stage),
		Type:              jobTypeOf(j.Type),
		Stop:              j.Stop,
		State:             jobStateOf(j.State),
		ModifyIndex:       m.i64("modify_index", j.ModifyIndex),
		HealthyNodeCount:  m.i32("healthy_node_count", j.HealthyNodeCount),
		ExpectedNodeCount: m.i32("expected_node_count", j.ExpectedNodeCount),
		MissingNodes:      missingNodes(j.MissingNodes),
	}

	if len(j.AllocationCounts) > 0 {
		out.AllocationCounts = make(map[string]int32, len(j.AllocationCounts))

		for _, k := range slices.Sorted(maps.Keys(j.AllocationCounts)) {
			out.AllocationCounts[k] = m.i32("allocation_counts."+k, j.AllocationCounts[k])
		}
	}

	return out
}

func missingNodes(nodes []jobstate.MissingNode) []*dcctlv1.Datacenter_Nomad_Node {
	out := make([]*dcctlv1.Datacenter_Nomad_Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, &dcctlv1.Datacenter_Nomad_Node{Node: n.Name, Reason: nodeReasonOf(n.Reason)})
	}

	return out
}

// checkOf maps c; DiscardBytes is internal, its amount is in the facts.
func (m *mapper) checkOf(c check.Check) *dcctlv1.Check {
	out := &dcctlv1.Check{
		Id:           string(c.ID),
		Result:       checkResultOf(c.Result),
		Effect:       checkEffectOf(c.Effect),
		Confirmation: string(c.Confirmation),
		Datacenter:   datacenterName(c.Datacenter),
		Detail:       c.Detail,
	}

	if c.Facts != nil {
		facts, err := structpb.NewStruct(c.Facts)
		if err != nil {
			m.fail(fmt.Errorf("facts of %s: %w", c.ID, err))
		}

		out.Facts = facts
	}

	return out
}

// deref returns 0 for nil: the proto has no "not known" for these numbers.
func deref(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}
