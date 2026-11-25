package status_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/nomad"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func editMember(name string, fn func(m *collect.Member)) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		for i := range s.Clusters {
			for _, m := range s.Clusters[i].Patroni.Data.Members {
				if m.Name == name {
					editPatroni(s, s.Clusters[i].Datacenters[0], func(p *collect.Patroni) { fn(memberNamed(p, name)) })

					return
				}
			}
		}

		panic("no member " + name)
	}
}

func editProbe(name string, fn func(p *patroni.State)) func(s *collect.Snapshot) {
	return editMember(name, func(m *collect.Member) {
		p := *m.Probe
		fn(&p)
		m.Probe = &p
	})
}

func dropLeader(dc, name string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		editPatroni(s, dc, func(p *collect.Patroni) {
			m := memberNamed(p, name)
			m.Role = patroni.ClusterRoleReplica
			m.State = "stopped"
			pr := *m.Probe
			pr.Role, pr.State = patroni.RoleReplica, "stopped"
			m.Probe = &pr
			p.Leader = ""
		})
	}
}

func editNomad(dc string, fn func(n *collect.Nomad)) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		d := dcOf(s, dc)
		n := d.Nomad.Data
		n.Managed = append([]nomad.Job(nil), n.Managed...)
		fn(&n)
		d.Nomad.Data = n
	}
}

func setStandbyHost(host string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		editPatroni(s, dcB, func(p *collect.Patroni) {
			p.Config = up(map[string]any{"standby_cluster": map[string]any{"host": host}})
		})
	}
}

func all(fns ...func(s *collect.Snapshot)) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		for _, fn := range fns {
			fn(s)
		}
	}
}

func TestBuild_health(t *testing.T) {
	tests := []struct {
		name    string
		snap    func() *collect.Snapshot
		dc      string
		health  status.Health
		reasons []status.HealthReason
	}{
		// warm_standby, active datacenter
		{name: "warm_active_ok", snap: warm, dc: dcA, health: status.HealthOK},
		{
			name: "warm_active_no_primary",
			snap: with(warm, dropLeader(dcA, "pg-a1")),
			dc:   dcA, health: status.HealthDown,
			reasons: []status.HealthReason{status.ReasonNoPrimary, status.ReasonMemberNotRunning},
		},
		{
			name: "warm_active_leader_is_standby_leader",
			snap: with(warm, editMember("pg-a1", func(m *collect.Member) { m.Role = patroni.ClusterRoleStandbyLeader })),
			dc:   dcA, health: status.HealthDown, reasons: []status.HealthReason{status.ReasonNoPrimary},
		},
		{
			name: "warm_active_job_not_healthy",
			snap: with(warm, editNomad(dcA, func(n *collect.Nomad) {
				n.Allocs = map[string][]nomad.Alloc{"engine": nil, "freeswitch": n.Allocs["freeswitch"]}
			})),
			dc: dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonJobsNotHealthy},
		},
		{
			name: "warm_active_no_job_healthy",
			snap: with(warm, editNomad(dcA, func(n *collect.Nomad) { n.Allocs = nil })),
			dc:   dcA, health: status.HealthDown, reasons: []status.HealthReason{status.ReasonNoHealthyJobs},
		},
		{
			name: "warm_active_without_managed_jobs",
			snap: with(warm, editNomad(dcA, func(n *collect.Nomad) { n.Managed, n.Allocs = nil, nil })),
			dc:   dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonNoManagedJobs},
		},
		{
			name: "warm_active_member_not_running",
			snap: with(warm, editProbe("pg-a2", func(p *patroni.State) { p.State = "starting" })),
			dc:   dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonMemberNotRunning},
		},
		{
			name: "warm_active_member_unreachable",
			snap: with(warm, editMember("pg-a2", func(m *collect.Member) {
				*m = member(m.Name, m.Host, m.Datacenter, m.Role, m.State, nil)
			})),
			dc: dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonMemberUnreachable},
		},
		{
			name: "warm_active_member_unusable",
			snap: with(warm, editMember("pg-a2", func(m *collect.Member) {
				m.Usable, m.UnusableReason = false, patroni.UnusableVersion
			})),
			dc: dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonMemberUnusable},
		},
		{
			name: "warm_active_paused",
			snap: with(warm, editProbe("pg-a1", func(p *patroni.State) { p.Pause = true })),
			dc:   dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonPaused},
		},
		{
			name: "warm_active_patroni_unreachable",
			snap: with(warm, func(s *collect.Snapshot) { s.Clusters[0].Patroni = unreachable(s.Clusters[0].Patroni) }),
			dc:   dcA, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonPatroniUnreachable},
		},
		{
			name: "warm_active_nomad_unreachable",
			snap: with(warm, func(s *collect.Snapshot) { dcOf(s, dcA).Nomad = unreachable(dcOf(s, dcA).Nomad) }),
			dc:   dcA, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonNomadUnreachable},
		},
		{
			name: "warm_active_down_known_without_nomad",
			snap: with(warm, all(
				dropLeader(dcA, "pg-a1"),
				func(s *collect.Snapshot) { dcOf(s, dcA).Nomad = unreachable(dcOf(s, dcA).Nomad) },
			)),
			dc: dcA, health: status.HealthDown,
			reasons: []status.HealthReason{
				status.ReasonNoPrimary, status.ReasonMemberNotRunning, status.ReasonNomadUnreachable,
			},
		},
		{
			name: "warm_active_unreachable",
			snap: with(warm, func(s *collect.Snapshot) { allSourcesDown(s, dcA) }),
			dc:   dcA, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonDatacenterUnreachable},
		},
		{
			name: "uninitialized",
			snap: with(warm, uninitialized),
			dc:   dcA, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonUninitialized},
		},

		// stretch, active datacenter
		{name: "stretch_active_ok", snap: stretch, dc: dcA, health: status.HealthOK},
		{
			name: "stretch_active_no_leader",
			snap: with(stretch, dropLeader(dcA, "pg-1")),
			dc:   dcA, health: status.HealthDown,
			reasons: []status.HealthReason{status.ReasonNoPatroniLeader, status.ReasonMemberNotRunning},
		},
		{
			name: "stretch_active_leader_elsewhere",
			snap: with(stretch, func(s *collect.Snapshot) {
				editPatroni(s, dcA, func(p *collect.Patroni) {
					memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
					memberNamed(p, "pg-1").State = "streaming"
					memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
					memberNamed(p, "pg-3").State = "running"
					p.Leader = "pg-3"
				})
			}),
			dc: dcA, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonLeaderNotInDatacenter},
		},

		// warm_standby, inactive datacenter
		{name: "warm_standby_ok", snap: warm, dc: dcB, health: status.HealthOK},
		{
			name: "warm_standby_no_leader",
			snap: with(warm, dropLeader(dcB, "pg-b1")),
			dc:   dcB, health: status.HealthDown,
			reasons: []status.HealthReason{status.ReasonNoPatroniLeader, status.ReasonMemberNotRunning},
		},
		{
			name: "warm_standby_leader_is_primary",
			snap: with(warm, func(s *collect.Snapshot) { promote(s, dcB, "pg-b1") }),
			dc:   dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonLeaderNotStandbyLeader},
		},
		{
			name: "warm_standby_not_streaming",
			snap: with(warm, editProbe("pg-b1", func(p *patroni.State) { p.ReplicationState = "stopped" })),
			dc:   dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonNotStreaming},
		},
		{
			name: "warm_standby_follows_another_datacenter",
			snap: with(warm, setStandbyHost("10.0.1.2")),
			dc:   dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonNotFollowingActive},
		},
		{
			name: "warm_standby_upstream_unknown",
			snap: with(warm, setStandbyHost("10.9.9.9")),
			dc:   dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonUpstreamUnknown},
		},
		{
			name: "warm_standby_config_unreachable",
			snap: with(warm, func(s *collect.Snapshot) {
				editPatroni(s, dcB, func(p *collect.Patroni) { p.Config = unreachable(p.Config) })
			}),
			dc: dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonPatroniConfigUnreachable},
		},
		{
			name: "warm_standby_other_timeline",
			snap: with(warm, editProbe("pg-b1", func(p *patroni.State) { p.Timeline = i64(2) })),
			dc:   dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonTimelineMismatch},
		},
		{
			name: "warm_standby_lag_exceeded",
			snap: with(warm, editProbe("pg-a1", func(p *patroni.State) { p.Xlog.Location = i64(4900 + maxLag + 1) })),
			dc:   dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonLagExceeded},
		},
		{
			name: "warm_standby_lag_at_the_limit",
			snap: with(warm, editProbe("pg-a1", func(p *patroni.State) { p.Xlog.Location = i64(4900 + maxLag) })),
			dc:   dcB, health: status.HealthOK,
		},
		{
			name: "warm_standby_lag_unknown",
			snap: with(warm, editProbe("pg-b1", func(p *patroni.State) { p.Xlog.ReceivedLocation = nil })),
			dc:   dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonLagUnknown},
		},
		{
			name: "warm_standby_active_patroni_unreachable",
			snap: with(warm, func(s *collect.Snapshot) { s.Clusters[0].Patroni = unreachable(s.Clusters[0].Patroni) }),
			dc:   dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonActivePatroniUnreachable},
		},
		{
			name: "warm_standby_leader_unreachable",
			snap: with(warm, editMember("pg-b1", func(m *collect.Member) {
				*m = member(m.Name, m.Host, m.Datacenter, m.Role, m.State, nil)
			})),
			dc: dcB, health: status.HealthUnknown,
			reasons: []status.HealthReason{status.ReasonMemberUnreachable, status.ReasonLagUnknown},
		},
		{
			name: "warm_standby_needs_recovery",
			snap: with(warm, func(s *collect.Snapshot) {
				setVoterState(s, dcA, stateEntry(5, dcA, dcB))
				setVoterState(s, dcB, stateEntry(5, dcA, dcB))
			}),
			dc: dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonNeedsRecovery},
		},
		{
			name: "warm_standby_job_running",
			snap: with(warm, func(s *collect.Snapshot) { startEngine(s, dcB) }),
			dc:   dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonJobsNotStopped},
		},
		{
			name: "warm_standby_nomad_unreachable",
			snap: with(warm, func(s *collect.Snapshot) { dcOf(s, dcB).Nomad = unreachable(dcOf(s, dcB).Nomad) }),
			dc:   dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonNomadUnreachable},
		},
		{
			name: "warm_standby_patroni_unreachable",
			snap: with(warm, func(s *collect.Snapshot) { s.Clusters[1].Patroni = unreachable(s.Clusters[1].Patroni) }),
			dc:   dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonPatroniUnreachable},
		},

		// stretch, inactive datacenter
		{name: "stretch_standby_ok", snap: stretch, dc: dcB, health: status.HealthOK},
		{
			name: "stretch_standby_member_not_streaming",
			snap: with(stretch, editMember("pg-4", func(m *collect.Member) {
				m.State = "running"
				m.Probe.ReplicationState = ""
			})),
			dc: dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonNotStreaming},
		},
		{
			name: "stretch_standby_no_leader",
			snap: with(stretch, dropLeader(dcA, "pg-1")),
			dc:   dcB, health: status.HealthDown, reasons: []status.HealthReason{status.ReasonNoPatroniLeader},
		},
		{
			name: "stretch_standby_holds_the_leader",
			snap: with(stretch, func(s *collect.Snapshot) {
				editPatroni(s, dcA, func(p *collect.Patroni) {
					memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
					memberNamed(p, "pg-1").State = "streaming"
					memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
					memberNamed(p, "pg-3").State = "running"
					p.Leader = "pg-3"
				})
			}),
			dc: dcB, health: status.HealthDegraded, reasons: []status.HealthReason{status.ReasonLeaderInDatacenter},
		},
		{
			name: "stretch_standby_without_members",
			snap: with(stretch, func(s *collect.Snapshot) {
				editPatroni(s, dcA, func(p *collect.Patroni) { p.Members = p.Members[:2] })
			}),
			dc: dcB, health: status.HealthDown, reasons: []status.HealthReason{status.ReasonNoMembers},
		},
		{
			name: "stretch_member_without_datacenter_may_be_any",
			snap: with(stretch, editMember("pg-4", func(m *collect.Member) { m.Datacenter = "" })),
			dc:   dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonMemberDatacenterUnknown},
		},
		{
			name: "stretch_member_without_datacenter_in_the_active",
			snap: with(stretch, editMember("pg-4", func(m *collect.Member) { m.Datacenter = "" })),
			dc:   dcA, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonMemberDatacenterUnknown},
		},
		{
			name: "stretch_members_without_datacenter_are_not_missing",
			snap: with(stretch, all(
				editMember("pg-3", func(m *collect.Member) { m.Datacenter = "" }),
				editMember("pg-4", func(m *collect.Member) { m.Datacenter = "" }),
			)),
			dc: dcB, health: status.HealthUnknown, reasons: []status.HealthReason{status.ReasonMemberDatacenterUnknown},
		},
		{
			name: "stretch_leader_without_datacenter",
			snap: with(stretch, editMember("pg-1", func(m *collect.Member) { m.Datacenter = "" })),
			dc:   dcA, health: status.HealthUnknown,
			reasons: []status.HealthReason{status.ReasonLeaderDatacenterUnknown, status.ReasonMemberDatacenterUnknown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := datacenter(t, status.Build(tt.snap(), testConfig(), now), tt.dc)

			assert.Equal(t, tt.health, d.Health)
			assert.ElementsMatch(t, tt.reasons, d.HealthReasons)
		})
	}
}

// The last data of a source that stopped answering is shown, yet never makes a
// datacenter healthy, however good it looks.
func TestBuild_lastKnownDataIsNeverOK(t *testing.T) {
	downs := map[string]func(s *collect.Snapshot, dc string){
		"nomad":   func(s *collect.Snapshot, dc string) { dcOf(s, dc).Nomad = unreachable(dcOf(s, dc).Nomad) },
		"patroni": func(s *collect.Snapshot, dc string) { cl, _ := s.ClusterOf(dc); cl.Patroni = unreachable(cl.Patroni) },
		"all":     allSourcesDown,
	}

	for name, down := range downs {
		for _, dc := range []string{dcA, dcB} {
			t.Run(name+"/"+dc, func(t *testing.T) {
				s := warm()
				down(s, dc)

				d := datacenter(t, status.Build(s, testConfig(), now), dc)

				assert.NotEqual(t, status.HealthOK, d.Health)
				assert.NotEmpty(t, d.HealthReasons)
			})
		}
	}
}

// The catalog is not needed for health, only for the info checks, which fail on its
// last data.
func TestBuild_lastKnownCatalog(t *testing.T) {
	for _, dc := range []string{dcA, dcB} {
		t.Run(dc, func(t *testing.T) {
			s := warm()
			dcOf(s, dc).Services = unreachable(dcOf(s, dc).Services)

			d := datacenter(t, status.Build(s, testConfig(), now), dc)

			assert.Equal(t, status.HealthOK, d.Health)
			assert.Equal(t, status.CheckFail, check(t, d, status.CheckInfoServices).Result)
			assert.Equal(t, status.CheckFail, check(t, d, status.CheckInfoRabbitMQ).Result)
		})
	}
}

// Without a role, the reasons that do not depend on it are still given, and the
// health stays unknown.
func TestBuild_healthWithoutRole(t *testing.T) {
	s := warm()
	voterDown(s, dcB)
	s.Arbiter = new(unreachable(*s.Arbiter))
	dcOf(s, dcB).Nomad = unreachable(dcOf(s, dcB).Nomad)
	editProbe("pg-a1", func(p *patroni.State) { p.Pause = true })(s)
	editProbe("pg-a2", func(p *patroni.State) { p.State = "starting" })(s)

	st := status.Build(s, testConfig(), now)

	a := datacenter(t, st, dcA)
	assert.Equal(t, status.HealthUnknown, a.Health)
	assert.ElementsMatch(t, []status.HealthReason{
		status.ReasonRoleUndetermined, status.ReasonPaused, status.ReasonMemberNotRunning,
	}, a.HealthReasons)

	b := datacenter(t, st, dcB)
	assert.Equal(t, status.HealthUnknown, b.Health)
	assert.ElementsMatch(t, []status.HealthReason{status.ReasonRoleUndetermined, status.ReasonNomadUnreachable}, b.HealthReasons)
}
