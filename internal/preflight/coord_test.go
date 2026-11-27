package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

// caseOf is one situation of a check: the snapshot, the action and the result.
type caseOf struct {
	name   string
	snap   func(t *testing.T) *collect.Snapshot
	action Action
	params Params
	want   check.Result
}

func runCases(t *testing.T, eval func(in *Input) check.Check, cases []caseOf) {
	t.Helper()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			action := tt.action
			if action == "" {
				action = Switch
			}

			params := tt.params
			if params == (Params{}) {
				params = Params{Datacenter: dcB}
			}

			c := eval(newInput(tt.snap(t), action, params))

			assert.Equal(t, tt.want, c.Result, "%s: %s", c.ID, c.Detail)
			assert.NotEmpty(t, c.Detail)
			requireStruct(t, c)
		})
	}
}

// withOp is warm with op in every Consul KV, changed by fn.
func withOp(op *dcctlv1.Operation, fn func(s *collect.Snapshot)) func(t *testing.T) *collect.Snapshot {
	return func(t *testing.T) *collect.Snapshot {
		t.Helper()

		s := warm()
		setOp(t, s, op)

		if fn != nil {
			fn(s)
		}

		return s
	}
}

func snap(fn func(s *collect.Snapshot)) func(t *testing.T) *collect.Snapshot {
	return func(*testing.T) *collect.Snapshot {
		s := warm()
		if fn != nil {
			fn(s)
		}

		return s
	}
}

func TestStatePresent(t *testing.T) {
	runCases(t, (*Input).statePresent, []caseOf{
		{name: "present", snap: snap(nil), want: check.Pass},
		{name: "absent_everywhere", snap: snap(func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV.Data.State = nil
			}

			s.Arbiter = new(up(collect.Arbiter{}))
		}), want: check.Fail},
		{name: "voters_unreachable", snap: snap(func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV = down[collect.KV]()
			}

			s.Arbiter = new(down[collect.Arbiter]())
		}), want: check.Fail},
	})
}

func TestNoOperation(t *testing.T) {
	runCases(t, (*Input).noOperation, []caseOf{
		{name: "none", snap: snap(nil), want: check.Pass},
		{name: "running", snap: withOp(operation("op-2", dcctlv1.Operation_RUNNING, dcB, dcB), nil), want: check.Fail},
		{name: "interrupted", snap: withOp(operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB), nil), want: check.Fail},
		{name: "finished", snap: withOp(operation("op-2", dcctlv1.Operation_SUCCEEDED, dcB, dcB), nil), want: check.Pass},
		{name: "unreadable_copy", snap: snap(func(s *collect.Snapshot) {
			s.Voters[0].KV.Data.Op = &consul.KVEntry{Key: "dcctl/op", Value: []byte("{")}
		}), want: check.Fail},
		{name: "unreachable_kv_is_not_checked", snap: withOp(operation("op-2", dcctlv1.Operation_SUCCEEDED, dcB, dcB), func(s *collect.Snapshot) { s.Voters[0].KV = down[collect.KV]() }), want: check.Pass},
		{name: "no_kv_answers", snap: snap(func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV = down[collect.KV]()
			}
		}), want: check.Fail},
	})
}

func lockEntry(session string) *consul.KVEntry {
	e := entry("dcctl/lock", map[string]string{"instance": "dcctl-dc_a", "datacenter": dcA, "operation": "op-9"})
	e.Session = session

	return e
}

func TestLockFree(t *testing.T) {
	runCases(t, (*Input).lockFree, []caseOf{
		{name: "free", snap: snap(nil), want: check.Pass},
		{name: "released_key", snap: snap(func(s *collect.Snapshot) { s.Voters[1].KV.Data.Lock = lockEntry("") }), want: check.Pass},
		{name: "held", snap: snap(func(s *collect.Snapshot) { s.Voters[1].KV.Data.Lock = lockEntry("s-1") }), want: check.Fail},
		{name: "no_kv_answers", snap: snap(func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV = down[collect.KV]()
			}
		}), want: check.Fail},
	})

	c := newInput(snap(func(s *collect.Snapshot) { s.Voters[1].KV.Data.Lock = lockEntry("s-1") })(t), Switch, Params{Datacenter: dcB}).lockFree()
	assert.Equal(t, map[string]any{"kv": dcB, "holder": "dcctl-dc_a", "datacenter": dcA, "operation": "op-9"}, c.Facts)
}

func TestLeaderReachable(t *testing.T) {
	runCases(t, (*Input).leaderReachable, []caseOf{
		{name: "leading_instance", snap: snap(nil), want: check.Pass},
		{name: "not_the_leading_datacenter", snap: snap(nil), params: Params{Datacenter: dcA}, action: Demote, want: check.Fail},
		{name: "critical", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Services.Data.Dcctl[0].Status = "critical"
		}), want: check.Fail},
		{name: "not_registered", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Services.Data.Dcctl = nil
		}), want: check.Fail},
		{name: "catalog_unreachable", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Services = down[collect.Services]()
		}), want: check.Fail},
		{name: "apps_sync_leads_from_active", snap: snap(func(s *collect.Snapshot) {
			s.Observer = collect.Instance{Datacenter: dcA, Node: "dcctl-" + dcA}
		}), action: AppsSync, want: check.Pass},
		{name: "resume_from_target_of_operation", snap: withOp(operation("op-2", dcctlv1.Operation_INTERRUPTED, dcA, dcB), nil), action: Resume, params: Params{Operation: "op-2"}, want: check.Pass},
		{name: "resume_of_missing_operation", snap: snap(nil), action: Resume, params: Params{Operation: "op-2"}, want: check.Fail},
	})
}

// conflict has two forced records of generation 6 with different active datacenters.
func conflict(s *collect.Snapshot) {
	a, b := state(6, dcA), state(6, dcB)
	a.Forced, b.Forced = true, true
	s.Voters[0] = kvVoter(dcA, a)
	s.Voters[1] = kvVoter(dcB, b)
}

// partial is a partial advance: generation 6 reached dc_a only, dc_b does not answer
// and the arbiter holds 5, so 6 may be on a majority.
func partial(s *collect.Snapshot) {
	s.Voters[0] = kvVoter(dcA, state(6, dcB))
	s.Voters[1].KV = down[collect.KV]()
}

func TestNoConflict(t *testing.T) {
	runCases(t, (*Input).noConflict, []caseOf{
		{name: "none", snap: snap(nil), want: check.Pass},
		{name: "conflict", snap: snap(conflict), want: check.Fail},
		{name: "no_voter_answers", snap: snap(func(s *collect.Snapshot) {
			for i := range s.Voters {
				s.Voters[i].KV = down[collect.KV]()
			}

			s.Arbiter = new(down[collect.Arbiter]())
		}), want: check.Fail},
	})
}

func TestNoUncommitted(t *testing.T) {
	runCases(t, (*Input).noUncommitted, []caseOf{
		{name: "none", snap: snap(nil), want: check.Pass},
		{name: "uncommitted", snap: snap(func(s *collect.Snapshot) {
			s.Voters[0] = kvVoter(dcA, state(6, dcB))
		}), want: check.Fail},
		{name: "partial_advance_blocks_switch", snap: snap(partial), want: check.Fail},
		{name: "partial_advance_blocks_demote", snap: snap(partial), action: Demote, params: Params{Datacenter: dcA}, want: check.Fail},
		{name: "partial_advance_allows_failover", snap: snap(partial), action: Failover, want: check.Pass},
	})

	c := newInput(snap(partial)(t), Failover, Params{Datacenter: dcB}).noUncommitted()
	assert.Equal(t, int64(6), c.Facts["base_generation"])
}

// cutC adds dc_c to s and makes it unreachable.
func cutC(s *collect.Snapshot) {
	withC(s, 4900)
	cut(s, dcC)
}

func TestNoSplit(t *testing.T) {
	dbSplit := func(s *collect.Snapshot) {
		editPatroni(s, dcB, func(p *collect.Patroni) {
			l := memberNamed(p, "pg-dc_b-1")
			l.Role = "leader"
		})
	}

	runCases(t, func(in *Input) check.Check { return in.noSplit("") }, []caseOf{
		{name: "none", snap: snap(nil), want: check.Pass},
		{name: "split_brain", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Nomad = up(runningNomad())
		}), want: check.Fail},
		{name: "db_split", snap: snap(dbSplit), want: check.Fail},
		{name: "standby_not_known", snap: snap(func(s *collect.Snapshot) { cut(s, dcB) }), want: check.Fail},
		{name: "standby_nomad_not_known", snap: snap(func(s *collect.Snapshot) {
			d, _ := s.Datacenter(dcB)
			d.Nomad = down[collect.Nomad]()
		}), want: check.Fail},
		{name: "active_unreachable_is_not_split", snap: snap(func(s *collect.Snapshot) { cut(s, dcA) }), action: Failover, want: check.Pass},
		{name: "active_not_known", snap: snap(conflict), want: check.Fail},
		{name: "stretch_without_db_split", snap: stretchSnap(nil), want: check.Pass},
		{name: "failover_standby_not_known_warns", snap: snap(cutC), action: Failover, params: Params{Datacenter: dcB}, want: check.Warn},
		{name: "switch_standby_not_known", snap: snap(cutC), action: Switch, params: Params{Datacenter: dcB}, want: check.Fail},
		{name: "failover_target_not_known", snap: snap(func(s *collect.Snapshot) { cut(s, dcB) }), action: Failover, params: Params{Datacenter: dcB}, want: check.Fail},
	})

	failover := newInput(snap(cutC)(t), Failover, Params{Datacenter: dcB}).noSplit("")
	v := check.Decide([]check.Check{failover}, check.Confirmations{})
	assert.Equal(t, check.Allowed, v.Outcome, "a failover goes on with a warning")
	assert.Len(t, v.Warnings, 1)
	assert.Equal(t, "dc_c is not reachable; it may be primary or run apps", failover.Detail)

	in := newInput(snap(dbSplit)(t), Demote, Params{Datacenter: dcB})
	assert.Equal(t, check.Pass, in.noSplit(dcB).Result, "demote does not count the datacenter it demotes")
	assert.Equal(t, check.Block, in.noSplit(dcB).Effect)
}

func TestQuorum(t *testing.T) {
	lose := snap(func(s *collect.Snapshot) {
		s.Voters[0].KV = down[collect.KV]()
		s.Arbiter = new(down[collect.Arbiter]())
	})

	runCases(t, func(in *Input) check.Check { return in.quorum(check.Block) }, []caseOf{
		{name: "majority", snap: snap(nil), want: check.Pass},
		{name: "minority", snap: lose, want: check.Fail},
		{name: "consul_without_leader", snap: snap(func(s *collect.Snapshot) {
			s.Voters[0].KV.Data.Meta.KnownLeader = false
			s.Voters[1].KV.Data.Meta.KnownLeader = false
		}), want: check.Fail},
	})

	runCases(t, func(in *Input) check.Check { return in.quorum(check.WarnOnly) }, []caseOf{
		{name: "warn_minority", snap: lose, want: check.Warn},
	})
}

func TestIsolation(t *testing.T) {
	last := warm()
	s := warm()
	cut(s, dcA)

	in := newInput(s, Failover, Params{Datacenter: dcB})
	in.LastKnown = last

	c := in.isolation()
	assert.Equal(t, check.Fail, c.Result, "always needs the confirmation")
	assert.Equal(t, check.Confirm, c.Effect)
	assert.Equal(t, check.ConfirmIsolation, c.Confirmation)
	requireStruct(t, c)

	old := c.Facts["datacenters"].([]any)
	require.Len(t, old, 1)
	assert.Equal(t, map[string]any{
		"datacenter": dcA, "patroni_reachable": false,
		"last_seen": map[string]any{
			"time": observed.Format("2006-01-02T15:04:05Z07:00"), "member": "pg-a1", "role": "primary",
			"xlog_location": int64(5000), "target_sync_state": "async",
		},
	}, old[0])

	t.Run("resolve_names_the_losers", func(t *testing.T) {
		s := warm()
		conflict(s)

		c := newInput(s, Resolve, Params{Datacenter: dcB}).isolation()
		old := c.Facts["datacenters"].([]any)
		require.Len(t, old, 1)
		assert.Equal(t, dcA, old[0].(map[string]any)["datacenter"])
	})
}

func TestTargetValid(t *testing.T) {
	runCases(t, (*Input).targetValid, []caseOf{
		{name: "standby", snap: snap(nil), want: check.Pass},
		{name: "active", snap: snap(nil), params: Params{Datacenter: dcA}, want: check.Fail},
		{name: "unknown_datacenter", snap: snap(nil), params: Params{Datacenter: "dc_x"}, want: check.Fail},
		{name: "active_not_known", snap: snap(conflict), want: check.Fail},
		{name: "partial_advance_switch", snap: snap(partial), want: check.Fail},
		{name: "partial_advance_failover_to_active_of_record", snap: snap(partial), action: Failover, want: check.Fail},
		{name: "partial_advance_failover", snap: snap(partial), action: Failover, params: Params{Datacenter: dcA}, want: check.Pass},
	})
}

func TestTargetNotNeedsRecovery(t *testing.T) {
	runCases(t, (*Input).targetNotNeedsRecovery, []caseOf{
		{name: "fine", snap: snap(nil), want: check.Pass},
		{name: "needs_recovery", snap: snap(func(s *collect.Snapshot) {
			st := state(5, dcA, dcB)
			s.Voters[0], s.Voters[1] = kvVoter(dcA, st), kvVoter(dcB, st)
			s.Arbiter = arbiterWith(st)
		}), want: check.Fail},
		{name: "state_not_known", snap: snap(conflict), want: check.Fail},
	})
}

func TestDCNotActive(t *testing.T) {
	runCases(t, (*Input).dcNotActive, []caseOf{
		{name: "standby", snap: snap(nil), action: Demote, want: check.Pass},
		{name: "active", snap: snap(nil), action: Demote, params: Params{Datacenter: dcA}, want: check.Fail},
		{name: "unknown_datacenter", snap: snap(nil), action: Demote, params: Params{Datacenter: "dc_x"}, want: check.Fail},
		{name: "active_not_known", snap: snap(partial), action: Demote, want: check.Fail},
	})
}

func TestDCNeedsRecovery(t *testing.T) {
	runCases(t, (*Input).dcNeedsRecovery, []caseOf{
		{name: "needs_recovery", snap: snap(func(s *collect.Snapshot) {
			st := state(5, dcA, dcB)
			s.Voters[0], s.Voters[1] = kvVoter(dcA, st), kvVoter(dcB, st)
			s.Arbiter = arbiterWith(st)
		}), action: MarkRecovered, want: check.Pass},
		{name: "recovered", snap: snap(nil), action: MarkRecovered, want: check.Fail},
		{name: "state_not_known", snap: snap(conflict), action: MarkRecovered, want: check.Fail},
	})
}

// gone names in dcctl/state an active datacenter the config no longer has.
func gone(s *collect.Snapshot) {
	st := state(5, "dc_gone", dcB)
	for i := range s.Voters {
		s.Voters[i] = kvVoter(s.Voters[i].Datacenter, st)
	}

	s.Arbiter = arbiterWith(st)
}

func TestActiveNotConfigured(t *testing.T) {
	for _, a := range []Action{Switch, Failover, Demote, Repoint, MarkRecovered, AppsSync, Verify} {
		t.Run(string(a), func(t *testing.T) {
			in := newInput(snap(gone)(t), a, Params{Datacenter: dcB})

			cs, err := Checks(in)
			require.NoError(t, err)

			v := check.Decide(cs, check.Confirmations{Isolation: true, NotMaxLSN: true})
			assert.NotEqual(t, check.Allowed, v.Outcome)
		})
	}

	in := newInput(snap(gone)(t), MarkRecovered, Params{Datacenter: dcB})
	assert.Equal(t, check.Fail, in.dcRecovered().Result)
	assert.Equal(t, check.Fail, in.noSplit("").Result)
}

// forcedBelowUndetermined has a forced generation 5 with dc_b in needs_recovery on dc_b
// and generation 6 on dc_c and the arbiter, which dc_a, not answering, may hold too.
func forcedBelowUndetermined(s *collect.Snapshot) {
	withC(s, 4900)

	eff := state(5, dcA, dcB)
	eff.Forced = true
	next := state(6, dcA)

	s.Voters[0].KV = down[collect.KV]()
	s.Voters[1] = kvVoter(dcB, eff)
	s.Voters[2] = kvVoter(dcC, next)
	s.Arbiter = arbiterWith(next)
}

func TestFailoverBase(t *testing.T) {
	in := newInput(snap(forcedBelowUndetermined)(t), Failover, Params{Datacenter: dcB})
	require.Len(t, in.Status.View.Undetermined, 1)

	nu := in.noUncommitted()
	assert.Equal(t, check.Pass, nu.Result)
	assert.Equal(t, int64(6), nu.Facts["base_generation"])

	require.NotNil(t, in.reference())
	assert.Equal(t, int64(6), in.reference().Generation, "every check starts from the base no_uncommitted reports")
	assert.Equal(t, check.Pass, in.targetNotNeedsRecovery().Result, "generation 6 does not list dc_b")

	sw := newInput(snap(forcedBelowUndetermined)(t), Switch, Params{Datacenter: dcB})
	assert.Equal(t, check.Fail, sw.noUncommitted().Result)
}

func TestFailoverBase_ambiguous(t *testing.T) {
	s := withC(warm(), 4900)
	s.Voters[0].KV = down[collect.KV]()
	s.Voters[1] = kvVoter(dcB, state(6, dcB))
	s.Voters[2] = kvVoter(dcC, state(6, dcC))
	s.Arbiter = new(down[collect.Arbiter]())

	in := newInput(s, Failover, Params{Datacenter: dcB})
	require.Len(t, in.Status.View.Undetermined, 2)

	assert.Equal(t, check.Fail, in.noUncommitted().Result)
	assert.Nil(t, in.reference())
	assert.Equal(t, check.Fail, in.targetValid().Result)
}

func TestProfileSupported(t *testing.T) {
	runCases(t, (*Input).profileSupported, []caseOf{
		{name: "demote_warm", snap: snap(nil), action: Demote, want: check.Pass},
		{name: "repoint_warm", snap: snap(nil), action: Repoint, want: check.Pass},
		{name: "demote_stretch", snap: stretchSnap(nil), action: Demote, want: check.Fail},
		{name: "repoint_stretch", snap: stretchSnap(nil), action: Repoint, want: check.Fail},
	})
}

func TestNoKVAnswers_unreachableFacts(t *testing.T) {
	s := warm()
	for i := range s.Voters {
		s.Voters[i].KV = down[collect.KV]()
	}

	in := newInput(s, Switch, Params{Datacenter: dcB})

	for _, c := range []check.Check{in.noOperation(), in.lockFree()} {
		assert.Equal(t, check.Fail, c.Result)
		assert.Equal(t, false, c.Facts["reachable"], "%s: a KV that does not answer is not a busy one", c.ID)
	}
}

// payloadSplit is generation 5 of dc_a at revision 1 with dc_b in needs_recovery on the
// KV of dc_a and without it on the arbiter, while the KV of dc_b does not answer: the
// record is on a majority, neither payload is.
func payloadSplit(s *collect.Snapshot) {
	x, y := state(5, dcA, dcB), state(5, dcA)
	x.Revision, y.Revision = 1, 1

	s.Voters[0] = kvVoter(dcA, x)
	s.Voters[1].KV = down[collect.KV]()
	s.Arbiter = arbiterWith(y)
}

func TestPayloadSettled(t *testing.T) {
	runCases(t, (*Input).payloadSettled, []caseOf{
		{name: "settled", snap: snap(nil), action: MarkRecovered, want: check.Pass},
		{name: "revision_ahead_on_one_voter", snap: snap(func(s *collect.Snapshot) {
			st := state(5, dcA)
			st.Revision = 1
			s.Voters[0] = kvVoter(dcA, st)
		}), action: MarkRecovered, want: check.Pass},
		{name: "split", snap: snap(payloadSplit), action: MarkRecovered, want: check.Fail},
		{name: "state_not_known", snap: snap(conflict), action: MarkRecovered, want: check.Fail},
	})
}

// The split names itself and shows each payload with the voters that hold it.
func TestPayloadSettled_splitFacts(t *testing.T) {
	c := newInput(snap(payloadSplit)(t), MarkRecovered, Params{Datacenter: dcB}).payloadSettled()

	require.Equal(t, check.Fail, c.Result)
	assert.Contains(t, c.Detail, "payload split")
	assert.Equal(t, []any{
		map[string]any{
			"voters": []any{dcA}, "op_id": "op-1", "forced": false, "patroni_timeline": int64(3), "needs_recovery": []any{dcB},
		},
		map[string]any{
			"voters": []any{"arbiter"}, "op_id": "op-1", "forced": false, "patroni_timeline": int64(3), "needs_recovery": []any{},
		},
	}, c.Facts["payloads"])
	requireStruct(t, c)

	cs, err := Checks(newInput(snap(payloadSplit)(t), MarkRecovered, Params{Datacenter: dcB}))
	require.NoError(t, err)
	assert.Equal(t, check.Fail, find(t, cs, check.CoordPayloadSettled, "").Result)
}
