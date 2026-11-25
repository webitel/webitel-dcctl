package preflight

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
)

func cutNomad(s *collect.Snapshot, dc string) {
	d, _ := s.Datacenter(dc)
	d.Nomad = down[collect.Nomad]()
}

func invalidStage(s *collect.Snapshot, dc string) {
	setNomad(s, dc, func(n *collect.Nomad) {
		n.Managed[0].Stage, n.Managed[0].StageErr = 0, nomad.ErrInvalidStage
	})
}

func TestNomadReachable(t *testing.T) {
	runCases(t, func(in *Input) check.Check { return in.nomadReachable(dcB) }, []caseOf{
		{name: "answers", snap: snap(nil), want: check.Pass},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcB) }), want: check.Fail},
	})

	c := newInput(warm(), Switch, Params{Datacenter: dcB}).nomadReachable("")
	assert.Equal(t, check.Fail, c.Result, "the active datacenter is not known")
}

func TestStagesValid(t *testing.T) {
	runCases(t, func(in *Input) check.Check { return in.stagesValid(dcA) }, []caseOf{
		{name: "valid", snap: snap(nil), want: check.Pass},
		{name: "invalid", snap: snap(func(s *collect.Snapshot) { invalidStage(s, dcA) }), want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcA) }), want: check.Fail},
	})
}

func TestTargetJobsPresent(t *testing.T) {
	runCases(t, (*Input).targetJobsPresent, []caseOf{
		{name: "present", snap: snap(nil), want: check.Pass},
		{name: "missing", snap: snap(func(s *collect.Snapshot) {
			setNomad(s, dcB, func(n *collect.Nomad) { n.Managed = n.Managed[:1] })
		}), want: check.Fail},
		{name: "source_unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcA) }), want: check.Fail},
		{name: "target_unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcB) }), want: check.Fail},
		{name: "active_not_known", snap: snap(conflict), want: check.Fail},
	})
}

func TestTargetJobsStopped(t *testing.T) {
	runCases(t, (*Input).targetJobsStopped, []caseOf{
		{name: "stopped", snap: snap(nil), want: check.Pass},
		{name: "running", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Nomad = up(runningNomad())
		}), want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcB) }), want: check.Fail},
	})
}

func TestDCAppsStopped(t *testing.T) {
	runCases(t, (*Input).dcAppsStopped, []caseOf{
		{name: "stopped", snap: snap(nil), action: Demote, want: check.Pass},
		{name: "running_warns", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Nomad = up(runningNomad())
		}), action: Demote, want: check.Warn},
		{name: "unreachable_warns", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcB) }), action: Demote, want: check.Warn},
	})
}

func TestTargetPlan(t *testing.T) {
	for _, tt := range []struct {
		name  string
		plans func(p map[string]Plan)
		snap  func(s *collect.Snapshot)
		want  check.Result
	}{
		{name: "placed", want: check.Pass},
		{name: "failed_task_group", plans: func(p map[string]Plan) {
			p["engine"] = Plan{Result: nomad.PlanResult{FailedTGAllocs: []string{"api"}}}
		}, want: check.Fail},
		{name: "plan_error", plans: func(p map[string]Plan) { p["engine"] = Plan{Err: errors.New("timeout")} }, want: check.Fail},
		{name: "not_planned", plans: func(p map[string]Plan) { delete(p, "engine") }, want: check.Fail},
		{name: "unreachable", snap: func(s *collect.Snapshot) { cutNomad(s, dcB) }, want: check.Fail},
		{name: "no_managed_jobs", snap: func(s *collect.Snapshot) {
			setNomad(s, dcB, func(n *collect.Nomad) { n.Managed, n.Allocs = nil, nil })
		}, want: check.Fail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := warm()
			if tt.snap != nil {
				tt.snap(s)
			}

			in := newInput(s, Switch, Params{Datacenter: dcB})
			if tt.plans != nil {
				tt.plans(in.Plans[dcB])
			}

			c := in.targetPlan()
			assert.Equal(t, tt.want, c.Result, c.Detail)
			requireStruct(t, c)
		})
	}
}

func TestActivePlan(t *testing.T) {
	stoppedA := func(s *collect.Snapshot) {
		s.Observer.Datacenter = dcA
		d, _ := s.Datacenter(dcA)
		d.Nomad = up(stoppedNomad())
	}

	runCases(t, (*Input).activePlan, []caseOf{
		{name: "nothing_stopped", snap: snap(nil), action: AppsSync, want: check.Pass},
		{name: "stopped_placed", snap: snap(stoppedA), action: AppsSync, want: check.Pass},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcA) }), action: AppsSync, want: check.Fail},
		{name: "active_not_known", snap: snap(conflict), action: AppsSync, want: check.Fail},
	})

	in := newInput(snap(stoppedA)(t), AppsSync, Params{})
	in.Plans[dcA]["freeswitch"] = Plan{Result: nomad.PlanResult{FailedTGAllocs: []string{"fs"}}}
	assert.Equal(t, check.Fail, in.activePlan().Result)
}

func TestTargetNodes(t *testing.T) {
	runCases(t, (*Input).targetNodes, []caseOf{
		{name: "ready", snap: snap(nil), want: check.Pass},
		{name: "draining_node_of_system_job", snap: snap(func(s *collect.Snapshot) {
			setNomad(s, dcB, func(n *collect.Nomad) {
				node := readyNode("n2", "freeswitch")
				node.Drain = true
				n.Nodes = append(n.Nodes, node)
			})
		}), want: check.Warn},
		{name: "down_node_without_system_job", snap: snap(func(s *collect.Snapshot) {
			setNomad(s, dcB, func(n *collect.Nomad) {
				node := readyNode("n2", "storage")
				node.Status = "down"
				n.Nodes = append(n.Nodes, node)
			})
		}), want: check.Pass},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutNomad(s, dcB) }), want: check.Warn},
	})
}

func TestTargetRabbitMQ(t *testing.T) {
	noRabbit := func(s *collect.Snapshot) {
		d, _ := s.Datacenter(dcB)
		d.Services.Data.RabbitMQ[0].Status = "critical"
	}

	runCases(t, func(in *Input) check.Check { return in.targetRabbitMQ(check.Block) }, []caseOf{
		{name: "passing", snap: snap(nil), want: check.Pass},
		{name: "none_passing", snap: snap(noRabbit), want: check.Fail},
		{name: "catalog_unreachable", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Services = down[collect.Services]()
		}), want: check.Fail},
	})

	runCases(t, func(in *Input) check.Check { return in.targetRabbitMQ(check.WarnOnly) }, []caseOf{
		{name: "warn_none_passing", snap: snap(noRabbit), action: Failover, want: check.Warn},
	})
}
