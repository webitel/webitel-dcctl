package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/patroni"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func stretchSnap(fn func(s *collect.Snapshot)) func(t *testing.T) *collect.Snapshot {
	return func(*testing.T) *collect.Snapshot {
		s := stretch()
		if fn != nil {
			fn(s)
		}

		return s
	}
}

func withC3(recv int64, fn func(s *collect.Snapshot)) func(t *testing.T) *collect.Snapshot {
	return snap(func(s *collect.Snapshot) {
		withC(s, recv)

		if fn != nil {
			fn(s)
		}
	})
}

// cutPatroni makes the Patroni cluster of dc unreachable in the fresh collection.
func cutPatroni(s *collect.Snapshot, dc string) {
	cl, _ := s.ClusterOf(dc)
	cl.Patroni = down[collect.Patroni]()
	cl.Patroni.Data.Config = down[map[string]any]()
	cl.Patroni.Data.History = down[[]patroni.HistoryEntry]()
}

// memberDown makes the member name of dc not answer GET /patroni.
func memberDown(s *collect.Snapshot, dc, name string) {
	editPatroni(s, dc, func(p *collect.Patroni) {
		m := memberNamed(p, name)
		*m = member(m.Name, m.Host, m.Datacenter, m.Role, m.State, nil)
	})
}

func onB(eval func(in *Input, s pgScope) check.Check) func(in *Input) check.Check {
	return func(in *Input) check.Check { return eval(in, in.pgDatacenter(dcB)) }
}

func TestMembersFound(t *testing.T) {
	runCases(t, onB((*Input).membersFound), []caseOf{
		{name: "found", snap: snap(nil), want: check.Pass},
		{name: "none", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcB, func(p *collect.Patroni) { p.Members = nil })
		}), want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Fail},
	})

	runCases(t, func(in *Input) check.Check { return in.membersFound(in.pgCluster()) }, []caseOf{
		{name: "stretch_cluster", snap: stretchSnap(nil), want: check.Pass},
		{name: "stretch_unreachable", snap: stretchSnap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), want: check.Fail},
	})
}

func TestPatroniReachable(t *testing.T) {
	runCases(t, onB((*Input).pgReachable), []caseOf{
		{name: "all_answer", snap: snap(nil), want: check.Pass},
		{name: "replica_down_warns", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-2") }), want: check.Warn},
		{name: "leader_down", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-1") }), want: check.Fail},
		{name: "leader_tls", snap: snap(func(s *collect.Snapshot) {
			memberDown(s, dcB, "pg-dc_b-1")
			editPatroni(s, dcB, func(p *collect.Patroni) { memberNamed(p, "pg-dc_b-1").UnusableReason = patroni.UnusableTLS })
		}), want: check.Fail},
		{name: "cluster_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Fail},
	})
}

func TestPatroniVersion(t *testing.T) {
	runCases(t, onB((*Input).pgVersion), []caseOf{
		{name: "supported", snap: snap(nil), want: check.Pass},
		{name: "old_replica", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcB, func(p *collect.Patroni) {
				m := memberNamed(p, "pg-dc_b-2")
				m.Usable, m.UnusableReason = false, patroni.UnusableVersion
			})
		}), want: check.Fail},
		{name: "leader_version_unknown", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-1") }), want: check.Fail},
		{name: "unreachable_replica_is_not_judged", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-2") }), want: check.Pass},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Fail},
	})
}

func TestNotPaused(t *testing.T) {
	runCases(t, onB((*Input).notPaused), []caseOf{
		{name: "running", snap: snap(nil), want: check.Pass},
		{name: "paused", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcB, func(p *collect.Patroni) {
				m := memberNamed(p, "pg-dc_b-2")
				pr := *m.Probe
				pr.Pause = true
				m.Probe = &pr
			})
		}), want: check.Fail},
		{name: "no_member_answers", snap: snap(func(s *collect.Snapshot) {
			memberDown(s, dcB, "pg-dc_b-1")
			memberDown(s, dcB, "pg-dc_b-2")
		}), want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Fail},
	})
}

func TestHasLeader(t *testing.T) {
	runCases(t, onB((*Input).hasLeader), []caseOf{
		{name: "streaming_standby_leader", snap: snap(nil), want: check.Pass},
		{name: "no_leader", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcB, func(p *collect.Patroni) { memberNamed(p, "pg-dc_b-1").Role = patroni.ClusterRoleReplica })
		}), want: check.Fail},
		{name: "leader_stopped", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcB, func(p *collect.Patroni) {
				m := memberNamed(p, "pg-dc_b-1")
				pr := *m.Probe
				pr.State = "stopped"
				m.Probe = &pr
			})
		}), want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Fail},
	})

	runCases(t, func(in *Input) check.Check { return in.hasLeader(in.pgDatacenter(dcA)) }, []caseOf{
		{name: "running_primary", snap: snap(nil), want: check.Pass},
	})
}

func promoteB(s *collect.Snapshot) {
	editPatroni(s, dcB, func(p *collect.Patroni) { memberNamed(p, "pg-dc_b-1").Role = patroni.ClusterRoleLeader })
}

func TestSinglePrimary(t *testing.T) {
	runCases(t, (*Input).singlePrimary, []caseOf{
		{name: "one", snap: snap(nil), want: check.Pass},
		{name: "two", snap: snap(promoteB), want: check.Fail},
		{name: "unreachable_one_is_not_counted", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Pass},
		{name: "none_answers", snap: snap(func(s *collect.Snapshot) {
			cutPatroni(s, dcA)
			cutPatroni(s, dcB)
		}), want: check.Fail},
	})
}

func TestLeaderRoles(t *testing.T) {
	runCases(t, (*Input).sourcePrimary, []caseOf{
		{name: "source_primary", snap: snap(nil), want: check.Pass},
		{name: "source_demoted", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-a1").Role = patroni.ClusterRoleStandbyLeader })
		}), want: check.Fail},
		{name: "source_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), want: check.Fail},
		{name: "active_not_known", snap: snap(conflict), want: check.Fail},
	})

	runCases(t, (*Input).targetStandbyLeader, []caseOf{
		{name: "target_standby_leader", snap: snap(nil), want: check.Pass},
		{name: "target_primary", snap: snap(promoteB), want: check.Fail},
		{name: "target_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), want: check.Fail},
	})

	runCases(t, (*Input).dcPrimary, []caseOf{
		{name: "dc_primary", snap: snap(promoteB), action: Demote, want: check.Pass},
		{name: "dc_standby_is_not_primary", snap: snap(nil), action: Demote, want: check.Fail},
	})

	runCases(t, (*Input).dcStandby, []caseOf{
		{name: "dc_standby", snap: snap(nil), action: Repoint, want: check.Pass},
		{name: "dc_primary_is_not_standby", snap: snap(promoteB), action: Repoint, want: check.Fail},
		{name: "dc_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), action: Repoint, want: check.Fail},
	})
}

func editProbe(s *collect.Snapshot, dc, name string, fn func(p *patroni.State)) {
	editPatroni(s, dc, func(p *collect.Patroni) {
		m := memberNamed(p, name)
		pr := *m.Probe
		fn(&pr)
		m.Probe = &pr
	})
}

func TestTargetFollowsSource(t *testing.T) {
	runCases(t, (*Input).targetFollowsSource, []caseOf{
		{name: "follows", snap: snap(nil), want: check.Pass},
		{name: "not_streaming", snap: snap(func(s *collect.Snapshot) {
			editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) { p.ReplicationState = "stopped" })
		}), want: check.Fail},
		{name: "upstream_elsewhere", snap: snap(func(s *collect.Snapshot) {
			cl, _ := s.ClusterOf(dcB)
			cl.Patroni.Data.Config = up(map[string]any{"standby_cluster": map[string]any{"host": "10.0.9.9"}})
		}), want: check.Fail},
		{name: "other_timeline", snap: snap(func(s *collect.Snapshot) {
			editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) { p.Timeline = i64(2) })
		}), want: check.Fail},
		{name: "other_port", snap: snap(func(s *collect.Snapshot) {
			cl, _ := s.ClusterOf(dcB)
			cl.Patroni.Data.Config = up(map[string]any{"standby_cluster": map[string]any{"host": "10.0.0.1", "port": float64(6432)}})
		}), want: check.Fail},
		{name: "default_port", snap: snap(func(s *collect.Snapshot) {
			cl, _ := s.ClusterOf(dcB)
			cl.Patroni.Data.Config = up(map[string]any{"standby_cluster": map[string]any{"host": "10.0.0.1"}})
		}), want: check.Pass},
		{name: "primary_does_not_see_client", snap: snap(func(s *collect.Snapshot) {
			editProbe(s, dcA, "pg-a1", func(p *patroni.State) {
				p.Replication = []patroni.Replica{{ClientAddr: "10.0.0.2", State: "streaming"}}
			})
		}), want: check.Fail},
		{name: "config_unreachable", snap: snap(func(s *collect.Snapshot) {
			cl, _ := s.ClusterOf(dcB)
			cl.Patroni.Data.Config = down[map[string]any]()
		}), want: check.Fail},
		{name: "source_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), want: check.Fail},
	})
}

func TestTargetLag(t *testing.T) {
	lagged := func(lag int64) func(t *testing.T) *collect.Snapshot {
		return snap(func(s *collect.Snapshot) {
			editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) { p.Xlog.ReceivedLocation = i64(5000 - lag) })
		})
	}

	runCases(t, (*Input).targetLag, []caseOf{
		{name: "small", snap: snap(nil), want: check.Pass},
		{name: "at_limit", snap: lagged(maxLag), want: check.Pass},
		{name: "over_limit", snap: lagged(maxLag + 1), want: check.Fail},
		{name: "unknown", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), want: check.Fail},
	})

	c := newInput(warm(), Switch, Params{Datacenter: dcB}).targetLag()
	assert.Equal(t, map[string]any{"lag_bytes": int64(100), "limit_bytes": int64(maxLag)}, c.Facts)
}

func TestOthersFollowSource(t *testing.T) {
	eval := func(in *Input) []check.Check { return in.othersFollowSource() }

	cs := eval(newInput(warm(), Switch, Params{Datacenter: dcB}))
	assert.Equal(t, []check.Check{{
		ID: check.PatroniOthersFollowSource, Result: check.Skip, Effect: check.WarnOnly,
		Detail: "there is no other standby datacenter",
	}}, cs)

	runCases(t, func(in *Input) check.Check { return find(t, eval(in), check.PatroniOthersFollowSource, dcC) }, []caseOf{
		{name: "c_follows", snap: withC3(4900, nil), want: check.Pass},
		{name: "c_unreachable_warns", snap: withC3(4900, func(s *collect.Snapshot) { cutPatroni(s, dcC) }), want: check.Warn},
		{name: "c_not_streaming_warns", snap: withC3(4900, func(s *collect.Snapshot) {
			editProbe(s, dcC, "pg-dc_c-1", func(p *patroni.State) { p.ReplicationState = "stopped" })
		}), want: check.Warn},
	})
}

func TestTargetNotStreaming(t *testing.T) {
	isolated := snap(func(s *collect.Snapshot) {
		cutPatroni(s, dcA)
		editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) { p.ReplicationState = "stopped" })
	})

	runCases(t, (*Input).targetNotStreaming, []caseOf{
		{name: "stopped_streaming", snap: isolated, action: Failover, want: check.Pass},
		{name: "still_streams", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), action: Failover, want: check.Fail},
		{name: "leader_down", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-1") }), action: Failover, want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), action: Failover, want: check.Fail},
	})
}

func TestTargetMaxLSN(t *testing.T) {
	runCases(t, (*Input).targetMaxLSN, []caseOf{
		{name: "no_other_standby", snap: snap(nil), action: Failover, want: check.Skip},
		{name: "target_has_most", snap: withC3(4800, nil), action: Failover, want: check.Pass},
		{name: "equal", snap: withC3(4900, nil), action: Failover, want: check.Pass},
		{name: "c_has_more", snap: withC3(4950, nil), action: Failover, want: check.Fail},
		{name: "c_unreachable_is_not_compared", snap: withC3(4950, func(s *collect.Snapshot) { cutPatroni(s, dcC) }), action: Failover, want: check.Pass},
		{name: "target_unknown", snap: withC3(4800, func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-1") }), action: Failover, want: check.Fail},
	})

	c := newInput(withC3(4950, nil)(t), Failover, Params{Datacenter: dcB}).targetMaxLSN()
	assert.Equal(t, check.Confirm, c.Effect)
	assert.Equal(t, check.ConfirmNotMaxLSN, c.Confirmation)
}

func TestSourceDiscard(t *testing.T) {
	eval := func(in *Input) check.Check { return in.sourceDiscard(in.activeName(), dcB) }

	runCases(t, eval, []caseOf{
		{name: "source_primary", snap: snap(nil), action: Failover, params: Params{Datacenter: dcB}, want: check.Fail},
		{name: "target_received_all", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcB, func(p *collect.Patroni) {
				m := memberNamed(p, "pg-dc_b-1")
				pr := *m.Probe
				pr.Xlog.ReceivedLocation = i64(5000)
				m.Probe = &pr
			})
		}), action: Failover, params: Params{Datacenter: dcB}, want: check.Pass},
		{name: "source_not_read", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), action: Failover, params: Params{Datacenter: dcB}, want: check.Skip},
		{name: "source_not_primary", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) {
				m := memberNamed(p, "pg-a1")
				pr := *m.Probe
				pr.Role, m.Role = patroni.RoleStandbyLeader, patroni.ClusterRoleStandbyLeader
				m.Probe = &pr
			})
		}), action: Failover, params: Params{Datacenter: dcB}, want: check.Skip},
		{name: "target_received_unknown", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-1") }), action: Failover, params: Params{Datacenter: dcB}, want: check.Fail},
	})

	c := eval(newInput(snap(nil)(t), Failover, Params{Datacenter: dcB}))
	assert.Equal(t, check.ConfirmMaxDiscardBytes, c.Confirmation)
	require.NotNil(t, c.DiscardBytes)
	assert.Equal(t, int64(100), *c.DiscardBytes)
	assert.Equal(t, int64(100), c.Facts["divergence_bytes"])
	assert.Equal(t, "0/1388", c.Facts["xlog_location"])
	requireStruct(t, c)

	unknown := eval(newInput(snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-1") })(t), Failover, Params{Datacenter: dcB}))
	assert.Nil(t, unknown.DiscardBytes, "an amount that is not known has no bound")

	// dc_a left timeline 2 at 3000 and wrote up to 5000 on timeline 3; dc_b stayed on
	// timeline 2 with 4900 received: only 3000 is shared.
	bOnTimeline2 := func(s *collect.Snapshot) {
		editPatroni(s, dcB, func(p *collect.Patroni) {
			m := memberNamed(p, "pg-dc_b-1")
			pr := *m.Probe
			pr.Timeline, m.Timeline = i64(2), i64(2)
			m.Probe = &pr
		})
	}

	timelines := eval(newInput(snap(bOnTimeline2)(t), Failover, Params{Datacenter: dcB}))
	assert.Equal(t, check.Fail, timelines.Result)
	require.NotNil(t, timelines.DiscardBytes)
	assert.Equal(t, int64(2000), *timelines.DiscardBytes)

	noHistory := eval(newInput(snap(func(s *collect.Snapshot) {
		bOnTimeline2(s)

		cl, _ := s.ClusterOf(dcA)
		cl.Patroni.Data.History = down[[]patroni.HistoryEntry]()
	})(t), Failover, Params{Datacenter: dcB}))
	assert.Equal(t, check.Fail, noHistory.Result)
	assert.Nil(t, noHistory.DiscardBytes, "without the history of dc_a the amount is not known")
}

func TestTargetCandidate(t *testing.T) {
	runCases(t, (*Input).targetCandidate, []caseOf{
		{name: "candidate", snap: stretchSnap(nil), want: check.Pass},
		{name: "leader_already_in_target", snap: stretchSnap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
				memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
			})
		}), want: check.Skip},
		{name: "lag_over_limit", snap: stretchSnap(func(s *collect.Snapshot) {
			s.Clusters[0].Patroni.Data.Config = up(map[string]any{"maximum_lag_on_failover": float64(5)})
		}), want: check.Fail},
		{name: "nofailover", snap: stretchSnap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-3").NoFailover = true
				memberNamed(p, "pg-4").FailoverPriority = 0
			})
		}), want: check.Fail},
		{name: "config_unreachable", snap: stretchSnap(func(s *collect.Snapshot) {
			s.Clusters[0].Patroni.Data.Config = down[map[string]any]()
		}), want: check.Fail},
		{name: "unreachable", snap: stretchSnap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), want: check.Fail},
	})

	c := newInput(stretch(), Switch, Params{Datacenter: dcB}).targetCandidate()
	assert.Equal(t, "pg-3", c.Facts["candidate"], "the most WAL wins over a higher failover_priority")
}

func noLeader(s *collect.Snapshot) {
	editPatroni(s, dcA, func(p *collect.Patroni) {
		*memberNamed(p, "pg-1") = member("pg-1", "10.0.0.1", dcA, patroni.ClusterRoleReplica, "stopped", nil)
	})
}

func TestTargetLeader(t *testing.T) {
	runCases(t, (*Input).targetLeader, []caseOf{
		{name: "no_leader_with_candidate", snap: stretchSnap(noLeader), action: Failover, want: check.Pass},
		{name: "leader_in_target", snap: stretchSnap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
				memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
			})
		}), action: Failover, want: check.Pass},
		{name: "leader_elsewhere", snap: stretchSnap(nil), action: Failover, want: check.Fail},
		{name: "every_member_over_the_lag_limit", snap: stretchSnap(func(s *collect.Snapshot) {
			noLeader(s)
			s.Clusters[0].Patroni.Data.Config = up(map[string]any{"maximum_lag_on_failover": float64(5)})
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-3").Lag = i64(1 << 30)
				memberNamed(p, "pg-4").Lag = i64(1 << 30)
			})
		}), action: Failover, want: check.Pass},
		{name: "no_candidate", snap: stretchSnap(func(s *collect.Snapshot) {
			noLeader(s)
			memberDown(s, dcA, "pg-3")
			memberDown(s, dcA, "pg-4")
		}), action: Failover, want: check.Fail},
		{name: "leader_datacenter_unknown", snap: stretchSnap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-1").Datacenter = "" })
		}), action: Failover, want: check.Fail},
		{name: "unreachable", snap: stretchSnap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), action: Failover, want: check.Fail},
	})
}

func TestCandidateLoss(t *testing.T) {
	lag := func(n int64) func(t *testing.T) *collect.Snapshot {
		return stretchSnap(func(s *collect.Snapshot) {
			noLeader(s)
			editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-3").Lag = i64(n) })
		})
	}

	runCases(t, (*Input).candidateLoss, []caseOf{
		{name: "leader_exists", snap: stretchSnap(nil), action: Failover, want: check.Skip},
		{name: "no_lag", snap: stretchSnap(func(s *collect.Snapshot) {
			noLeader(s)
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-3").Lag = i64(0)
				memberNamed(p, "pg-3").Probe.Xlog.ReceivedLocation = i64(5000)
			})
		}), action: Failover, want: check.Pass},
		{name: "survivor_ahead", snap: lag(0), action: Failover, want: check.Fail},
		{name: "timeline_unknown", snap: stretchSnap(func(s *collect.Snapshot) {
			noLeader(s)
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-2").Probe.Timeline = nil
				memberNamed(p, "pg-2").Timeline = nil
			})
		}), action: Failover, want: check.Fail},
		{name: "lag", snap: lag(300), action: Failover, want: check.Fail},
		{name: "lag_unknown", snap: stretchSnap(func(s *collect.Snapshot) {
			noLeader(s)
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-3").Lag = nil
				memberNamed(p, "pg-4").Lag = nil
			})
		}), action: Failover, want: check.Fail},
		{name: "unreachable", snap: stretchSnap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), action: Failover, want: check.Fail},
	})

	c := newInput(lag(300)(t), Failover, Params{Datacenter: dcB}).candidateLoss()
	require.NotNil(t, c.DiscardBytes)
	assert.Equal(t, int64(300), *c.DiscardBytes)
	assert.Equal(t, int64(300), c.Facts["lag_bytes"])
	assert.Equal(t, check.ConfirmMaxDiscardBytes, c.Confirmation)

	// pg-2 of dc_a holds 10 bytes more than pg-3, whose lag reads 0.
	c = newInput(lag(0)(t), Failover, Params{Datacenter: dcB}).candidateLoss()
	require.NotNil(t, c.DiscardBytes)
	assert.Equal(t, int64(10), *c.DiscardBytes)

	c = newInput(stretchSnap(func(s *collect.Snapshot) {
		noLeader(s)
		editPatroni(s, dcA, func(p *collect.Patroni) {
			memberNamed(p, "pg-2").Probe.Timeline = nil
			memberNamed(p, "pg-2").Timeline = nil
		})
	})(t), Failover, Params{Datacenter: dcB}).candidateLoss()
	assert.Nil(t, c.DiscardBytes, "a timeline that is not known leaves the amount not known")

	c = newInput(stretchSnap(func(s *collect.Snapshot) {
		noLeader(s)
		editPatroni(s, dcA, func(p *collect.Patroni) {
			memberNamed(p, "pg-3").Lag = nil
			memberNamed(p, "pg-4").Lag = nil
		})
	})(t), Failover, Params{Datacenter: dcB}).candidateLoss()
	assert.Nil(t, c.DiscardBytes, "an unknown lag has no amount to confirm")
}

func TestActivePrimary(t *testing.T) {
	runCases(t, (*Input).activePrimary, []caseOf{
		{name: "warm_running_primary", snap: snap(nil), action: Demote, want: check.Pass},
		{name: "warm_demoted", snap: snap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) { memberNamed(p, "pg-a1").Role = patroni.ClusterRoleStandbyLeader })
		}), action: Demote, want: check.Fail},
		{name: "warm_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), action: Demote, want: check.Fail},
		{name: "active_not_known", snap: snap(partial), action: Demote, want: check.Fail},
		{name: "stretch_leader_in_active", snap: stretchSnap(nil), action: AppsSync, want: check.Pass},
		{name: "stretch_leader_elsewhere", snap: stretchSnap(func(s *collect.Snapshot) {
			editPatroni(s, dcA, func(p *collect.Patroni) {
				memberNamed(p, "pg-1").Role = patroni.ClusterRoleReplica
				memberNamed(p, "pg-3").Role = patroni.ClusterRoleLeader
			})
		}), action: AppsSync, want: check.Fail},
	})
}

// diverged puts dc_b on timeline 4 branched from timeline 3 at 4500: it wrote 400 bytes
// the active datacenter, still on timeline 3, does not have.
func diverged(s *collect.Snapshot) {
	editProbe(s, dcB, "pg-dc_b-1", func(p *patroni.State) {
		p.Role, p.Timeline = patroni.RolePrimary, i64(4)
		p.Xlog = patroni.Xlog{Location: i64(4900)}
	})
	editPatroni(s, dcB, func(p *collect.Patroni) { memberNamed(p, "pg-dc_b-1").Role = patroni.ClusterRoleLeader })

	cl, _ := s.ClusterOf(dcB)
	cl.Patroni.Data.History = up([]patroni.HistoryEntry{{Timeline: 1, LSN: 1000}, {Timeline: 2, LSN: 3000}, {Timeline: 3, LSN: 4500}})
}

func TestDivergence(t *testing.T) {
	runCases(t, (*Input).divergence, []caseOf{
		{name: "not_diverged", snap: snap(nil), action: Demote, want: check.Pass},
		{name: "diverged", snap: snap(diverged), action: Demote, want: check.Fail},
		{name: "unknown", snap: snap(func(s *collect.Snapshot) {
			diverged(s)
			cl, _ := s.ClusterOf(dcB)
			cl.Patroni.Data.History = down[[]patroni.HistoryEntry]()
		}), action: Demote, want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), action: Demote, want: check.Fail},
	})

	c := newInput(snap(diverged)(t), Demote, Params{Datacenter: dcB}).divergence()
	require.NotNil(t, c.DiscardBytes)
	assert.Equal(t, int64(400), *c.DiscardBytes)

	verdict := check.Decide([]check.Check{c}, check.Confirmations{MaxDiscardBytes: 400})
	assert.Equal(t, check.Allowed, verdict.Outcome, "a bound equal to the discard covers it")

	verdict = check.Decide([]check.Check{c}, check.Confirmations{MaxDiscardBytes: 399})
	assert.Equal(t, check.NeedsConfirmation, verdict.Outcome)

	unknown := newInput(snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) })(t), Demote, Params{Datacenter: dcB}).divergence()
	assert.Equal(t, check.Confirm, unknown.Effect)
	assert.Nil(t, unknown.DiscardBytes)
	assert.Equal(t, check.Blocked, check.Decide([]check.Check{unknown}, check.Confirmations{MaxDiscardBytes: 1 << 62}).Outcome,
		"no bound covers an amount that is not known")
}

func TestDCRecovered(t *testing.T) {
	runCases(t, (*Input).dcRecovered, []caseOf{
		{name: "recovered", snap: snap(nil), action: MarkRecovered, want: check.Pass},
		{name: "replica_not_streaming", snap: snap(func(s *collect.Snapshot) {
			editProbe(s, dcB, "pg-dc_b-2", func(p *patroni.State) { p.ReplicationState = "stopped" })
		}), action: MarkRecovered, want: check.Fail},
		{name: "replica_down", snap: snap(func(s *collect.Snapshot) { memberDown(s, dcB, "pg-dc_b-2") }), action: MarkRecovered, want: check.Fail},
		{name: "old_timeline", snap: snap(func(s *collect.Snapshot) {
			editProbe(s, dcB, "pg-dc_b-2", func(p *patroni.State) { p.Timeline = i64(2) })
		}), action: MarkRecovered, want: check.Fail},
		{name: "unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcB) }), action: MarkRecovered, want: check.Fail},
		{name: "active_unreachable", snap: snap(func(s *collect.Snapshot) { cutPatroni(s, dcA) }), action: MarkRecovered, want: check.Fail},
	})
}

func TestCandidates(t *testing.T) {
	m := func(name string, recv *int64, priority int, lag *int64) status.Member {
		return status.Member{
			Name: name, Datacenter: dcB, Usable: true, FailoverPriority: priority, LagBytes: lag,
			Xlog: status.Xlog{ReceivedLocation: recv},
		}
	}

	names := func(ms []status.Member) []string {
		out := make([]string, len(ms))
		for i, x := range ms {
			out[i] = x.Name
		}

		return out
	}

	unusable := m("unusable", i64(900), 1, i64(0))
	unusable.Usable = false
	noFailover := m("nofailover", i64(900), 1, i64(0))
	noFailover.NoFailover = true
	elsewhere := m("elsewhere", i64(900), 1, i64(0))
	elsewhere.Datacenter = dcA

	members := []status.Member{
		m("low_wal", i64(100), 9, i64(800)),
		m("unknown_wal", nil, 9, i64(0)),
		m("high_wal_low_priority", i64(500), 1, i64(400)),
		m("high_wal_high_priority", i64(500), 2, i64(400)),
		m("priority_zero", i64(900), 0, i64(0)),
		unusable, noFailover, elsewhere,
	}

	assert.Equal(t, []string{"high_wal_high_priority", "high_wal_low_priority", "low_wal", "unknown_wal"},
		names(Candidates(members, dcB, nil)), "WAL first, failover_priority on equal WAL")

	assert.Equal(t, []string{"high_wal_high_priority", "high_wal_low_priority", "unknown_wal"},
		names(Candidates(members, dcB, i64(400))), "lag over the limit is not a candidate")

	members[1].LagBytes = nil
	assert.NotContains(t, names(Candidates(members, dcB, i64(400))), "unknown_wal", "an unknown lag is over any limit")
}

func TestTargetMaxLSN_reachableStandbyWithoutData(t *testing.T) {
	noReceived := func(s *collect.Snapshot) {
		for _, name := range []string{"pg-dc_c-1", "pg-dc_c-2"} {
			editProbe(s, dcC, name, func(p *patroni.State) { p.Xlog.ReceivedLocation = nil })
		}
	}

	runCases(t, (*Input).targetMaxLSN, []caseOf{
		{name: "received_not_reported", snap: withC3(9999, noReceived), action: Failover, want: check.Fail},
		{name: "leader_down", snap: withC3(9999, func(s *collect.Snapshot) { memberDown(s, dcC, "pg-dc_c-1") }), action: Failover, want: check.Fail},
		{name: "no_leader", snap: withC3(9999, func(s *collect.Snapshot) {
			editPatroni(s, dcC, func(p *collect.Patroni) { memberNamed(p, "pg-dc_c-1").Role = patroni.ClusterRoleReplica })
		}), action: Failover, want: check.Fail},
	})

	c := newInput(withC3(4800, nil)(t), Failover, Params{Datacenter: dcB}).targetMaxLSN()
	others := c.Facts["others"].([]any)
	require.Len(t, others, 1)
	assert.Equal(t, true, others[0].(map[string]any)["reachable"], "Patroni of the standby answered")
}

func TestDivergence_facts(t *testing.T) {
	c := newInput(snap(diverged)(t), Demote, Params{Datacenter: dcB}).divergence()

	assert.Equal(t, int64(400), c.Facts["divergence_bytes"])
	assert.Equal(t, "0/1194", c.Facts["divergence_lsn"])
	assert.Equal(t, "0/1324", c.Facts["xlog_location"])
}

func TestFormatLSN(t *testing.T) {
	assert.Equal(t, "0/5000060", formatLSN(0x5000060))
	assert.Equal(t, "1/0", formatLSN(1<<32))
	assert.Empty(t, formatLSN(-1))
}
