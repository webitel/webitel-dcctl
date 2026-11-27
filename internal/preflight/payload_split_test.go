package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/collect"
)

// splitInput is the input of action on s whose record the action starts from is split,
// as the voters of warm_standby leave it in payloadSplit. Stretch has a single voter,
// which cannot split, so its view is split by hand.
func splitInput(t *testing.T, s *collect.Snapshot, action Action, p Params) *Input {
	t.Helper()

	if s.Profile != warm().Profile {
		in := newInput(s, action, p)
		require.NotNil(t, in.Status.View.Effective)
		in.Status.View.Effective.Settled = false

		return in
	}

	op := s.Voters[0].KV.Data.Op

	payloadSplit(s)

	s.Voters[0].KV.Data.Op = op

	return newInput(s, action, p)
}

// A switch and a failover of both profiles are refused on a payload split by name,
// before anything stops: the generation they write cannot start from such a record.
func TestPayloadSplitRefusesSwitchAndFailover(t *testing.T) {
	for _, profile := range []struct {
		name string
		snap func() *collect.Snapshot
	}{{name: "warm_standby", snap: warm}, {name: "stretch", snap: stretch}} {
		for _, action := range []Action{Switch, Failover} {
			t.Run(profile.name+"/"+string(action), func(t *testing.T) {
				cs, err := Checks(newInput(profile.snap(), action, Params{Datacenter: dcB}))
				require.NoError(t, err)
				assert.Equal(t, check.Pass, find(t, cs, check.CoordPayloadSettled, "").Result)

				cs, err = Checks(splitInput(t, profile.snap(), action, Params{Datacenter: dcB}))
				require.NoError(t, err)

				c := find(t, cs, check.CoordPayloadSettled, "")
				assert.Equal(t, check.Fail, c.Result)
				assert.Contains(t, c.Detail, "chosen explicitly")

				v := check.Decide(cs, check.Confirmations{Isolation: true, NotMaxLSN: true, MaxDiscardBytes: 1 << 40})
				assert.Equal(t, check.Blocked, v.Outcome)
				assert.Contains(t, v.Failed, c)
			})
		}
	}
}

// A switch or a failover resumed before its generation is written sees the split too.
func TestPayloadSplitRefusesResumeBeforeGeneration(t *testing.T) {
	for _, profile := range []struct {
		name string
		snap func() *collect.Snapshot
	}{{name: "warm_standby", snap: warm}, {name: "stretch", snap: stretch}} {
		for _, typ := range []dcctlv1.Operation_Type{dcctlv1.Operation_SWITCH, dcctlv1.Operation_FAILOVER} {
			t.Run(profile.name+"/"+typ.String(), func(t *testing.T) {
				op := recoveryOp(typ,
					stepIn(dcctlv1.Operation_Step_GENERATION, dcctlv1.Operation_Step_FAILED),
					stepIn(dcctlv1.Operation_Step_APPS_START, dcctlv1.Operation_Step_PENDING),
				)

				s := profile.snap()
				setOp(t, s, op)

				cs, err := Checks(splitInput(t, s, Resume, Params{Operation: "op-2"}))
				require.NoError(t, err)
				assert.Equal(t, check.Fail, find(t, cs, check.CoordPayloadSettled, "").Result)

				op.Steps[0].State = dcctlv1.Operation_Step_SUCCEEDED
				s = profile.snap()
				setOp(t, s, op)

				cs, err = Checks(splitInput(t, s, Resume, Params{Operation: "op-2"}))
				require.NoError(t, err)

				for _, c := range cs {
					assert.NotEqual(t, check.CoordPayloadSettled, c.ID, "the generation is written, nothing starts from the record")
				}
			})
		}
	}
}

// A switch interrupted in its generation step: generation 6 of dc_b reached the KV of
// dc_b only, the KV of dc_a holds 5. The voters do not tell the record the operation
// starts from, so the resume does not fail on a split it cannot read: the write of the
// step finishes the record.
func TestPayloadSplitSkipsUndeterminedRecordOnResume(t *testing.T) {
	for _, arbiterDown := range []bool{true, false} {
		s := warm()
		old, next := state(5, dcA), state(6, dcB)
		next.OpID = "op-2"
		s.Voters[0] = kvVoter(dcA, old)
		s.Voters[1] = kvVoter(dcB, next)

		if arbiterDown {
			s.Arbiter = new(down[collect.Arbiter]())
		} else {
			s.Arbiter = arbiterWith(old)
		}

		op := operation("op-2", dcctlv1.Operation_INTERRUPTED, dcB, dcB)
		op.PreviousGeneration, op.NewGeneration = 5, 6
		op.Steps = []*dcctlv1.Operation_Step{
			{Type: dcctlv1.Operation_Step_APPS_STOP, State: dcctlv1.Operation_Step_SUCCEEDED},
			{Type: dcctlv1.Operation_Step_GENERATION, State: dcctlv1.Operation_Step_FAILED},
			{Type: dcctlv1.Operation_Step_PATRONI_SWITCHOVER, State: dcctlv1.Operation_Step_PENDING, PointOfNoReturn: true},
		}
		setOp(t, s, op)

		cs, err := Checks(newInput(s, Resume, Params{Operation: "op-2"}))
		require.NoError(t, err)

		c := find(t, cs, check.CoordPayloadSettled, "")
		assert.NotEqual(t, check.Fail, c.Result, "arbiter down %v", arbiterDown)

		v := check.Decide(cs, check.Confirmations{Isolation: true})
		assert.NotContains(t, v.Failed, c)
	}
}
