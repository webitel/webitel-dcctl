package status_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/status"
)

func TestBuild_summary(t *testing.T) {
	st := status.Build(warm(), testConfig(), now)

	assert.Equal(t, collect.Instance{Datacenter: dcA, Node: "dcctl-a"}, st.Observer)
	assert.Equal(t, observed, st.ObserveTime)
	assert.Equal(t, 800*time.Millisecond, st.CycleDuration)
	assert.Equal(t, 5*time.Second, st.Interval)
	assert.Equal(t, config.ProfileWarmStandby, st.Profile)
	assert.True(t, st.Initialized)
	assert.Equal(t, dcA, st.DetectedActiveDatacenter)
	require.NotNil(t, st.Effective)
	assert.Equal(t, int64(5), st.Effective.Generation)
	assert.Equal(t, dcA, st.Effective.ActiveDatacenter)
	assert.Equal(t, int64(3), st.Effective.PatroniTimeline)
	assert.Equal(t, []string{dcA, dcB}, []string{st.Datacenters[0].Name, st.Datacenters[1].Name})
}

func TestBuild_voters(t *testing.T) {
	s := warm()
	setVoterState(s, dcB, nil)

	st := status.Build(s, testConfig(), now)

	assert.Equal(t, []status.Voter{
		{
			Name: dcA, Type: status.VoterDatacenter, Datacenter: dcA, Reachable: true, Status: coord.StatusPresent,
			Generation: 5, ActiveDatacenter: dcA, Valid: true,
		},
		{
			Name: dcB, Type: status.VoterDatacenter, Datacenter: dcB, Reachable: true, Status: coord.StatusAbsent,
			Stale: true, KeyMissing: true,
		},
		{
			Name: coord.ArbiterName, Type: status.VoterArbiter, Reachable: true, Status: coord.StatusPresent,
			Generation: 5, ActiveDatacenter: dcA, Valid: true, Initialized: true,
		},
	}, st.Voters)
	assert.Equal(t, status.Quorum{
		Available: true, Mode: status.QuorumQuorum, ReachableCount: 3, RequiredCount: 2, TotalCount: 3,
	}, st.Quorum)
}

func TestBuild_voterStatuses(t *testing.T) {
	tests := []struct {
		name string
		edit func(s *collect.Snapshot)
		want status.Voter
	}{
		{
			name: "unreachable",
			edit: func(s *collect.Snapshot) { voterDown(s, dcB) },
			want: status.Voter{
				Name: dcB, Type: status.VoterDatacenter, Datacenter: dcB, Status: coord.StatusUnreachable,
				Reason: source.ReasonRefused,
			},
		},
		{
			name: "invalid",
			edit: func(s *collect.Snapshot) {
				setVoterState(s, dcB, &consul.KVEntry{Key: kv.StateKey, Value: []byte("{")})
			},
			want: status.Voter{
				Name: dcB, Type: status.VoterDatacenter, Datacenter: dcB, Reachable: true, Status: coord.StatusInvalid,
				Reason: source.ReasonInvalid,
			},
		},
		{
			name: "stale",
			edit: func(s *collect.Snapshot) { setVoterState(s, dcB, stateEntry(4, dcB)) },
			want: status.Voter{
				Name: dcB, Type: status.VoterDatacenter, Datacenter: dcB, Reachable: true, Status: coord.StatusPresent,
				Generation: 4, ActiveDatacenter: dcB, Stale: true,
			},
		},
		{
			name: "consul_without_leader",
			edit: func(s *collect.Snapshot) { s.Voters[1].KV.Data.Meta.KnownLeader = false },
			want: status.Voter{
				Name: dcB, Type: status.VoterDatacenter, Datacenter: dcB, Status: coord.StatusStale,
				Reason: source.ReasonNoLeader, Generation: 5, ActiveDatacenter: dcA,
			},
		},
		{
			name: "consul_without_leader_newer",
			edit: func(s *collect.Snapshot) {
				setVoterState(s, dcB, stateEntry(6, dcB))
				s.Voters[1].KV.Data.Meta.KnownLeader = false
			},
			want: status.Voter{
				Name: dcB, Type: status.VoterDatacenter, Datacenter: dcB, Status: coord.StatusStale,
				Reason: source.ReasonNoLeader, Generation: 6, ActiveDatacenter: dcB,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := warm()
			tt.edit(s)

			assert.Equal(t, tt.want, status.Build(s, testConfig(), now).Voters[1])
		})
	}
}

// A Consul without a leader answers with what it last knew; its record never votes.
func TestBuild_staleReadDoesNotVote(t *testing.T) {
	s := warm()
	setVoterState(s, dcB, stateEntry(6, dcB))
	s.Voters[1].KV.Data.Meta.KnownLeader = false

	st := status.Build(s, testConfig(), now)

	require.NotNil(t, st.Effective)
	assert.Equal(t, int64(5), st.Effective.Generation)
	assert.Equal(t, dcA, st.Effective.ActiveDatacenter)
	assert.Empty(t, st.View.Uncommitted)
	assert.Empty(t, st.View.Undetermined)
	assert.False(t, st.View.Voters[1].Stale, "a stale read is not aligned")
	assert.Equal(t, 2, st.Quorum.ReachableCount)
}

func TestBuild_arbiterVoter(t *testing.T) {
	tests := []struct {
		name    string
		arbiter collect.Source[collect.Arbiter]
		want    status.Voter
	}{
		{
			name:    "uninitialized",
			arbiter: up(collect.Arbiter{}),
			want: status.Voter{
				Name: coord.ArbiterName, Type: status.VoterArbiter, Reachable: true, Status: coord.StatusUninitialized,
			},
		},
		{
			name:    "initialized_without_state",
			arbiter: up(collect.Arbiter{Initialized: true}),
			want: status.Voter{
				Name: coord.ArbiterName, Type: status.VoterArbiter, Reachable: true, Status: coord.StatusUninitialized,
			},
		},
		{
			name:    "unreachable",
			arbiter: unreachable(up(collect.Arbiter{Initialized: true})),
			want: status.Voter{
				Name: coord.ArbiterName, Type: status.VoterArbiter, Status: coord.StatusUnreachable,
				Reason: source.ReasonRefused,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := warm()
			s.Arbiter = &tt.arbiter

			st := status.Build(s, testConfig(), now)
			assert.Equal(t, tt.want, st.Voters[2])
			assert.Equal(t, 2, st.Quorum.ReachableCount, "an arbiter that does not vote is not counted")
		})
	}
}

func TestBuild_stretchVoter(t *testing.T) {
	st := status.Build(stretch(), testConfig(), now)

	require.Len(t, st.Voters, 1)
	assert.Equal(t, coord.StretchName, st.Voters[0].Name)
	assert.Equal(t, status.VoterDatacenter, st.Voters[0].Type)
	assert.Empty(t, st.Voters[0].Datacenter)
	assert.Equal(t, status.Quorum{
		Available: true, Mode: status.QuorumQuorum, ReachableCount: 1, RequiredCount: 1, TotalCount: 1,
	}, st.Quorum)
}

// A record read from a Consul without a leader does not vote, yet it shows the state
// exists.
func TestBuild_initializedByVoterWithoutLeader(t *testing.T) {
	s := stretch()
	s.Voters[0].KV.Data.Meta.KnownLeader = false

	st := status.Build(s, testConfig(), now)

	assert.True(t, st.Initialized)
	assert.False(t, st.View.Initialized)
	assert.Nil(t, st.Effective)
	assert.False(t, st.Quorum.Available, "no quorum without a Consul leader")
	assert.Equal(t, status.QuorumForcedOnly, st.Quorum.Mode)
}

// A key missing in one voter is an alarm only once the state exists somewhere.
func TestBuild_keyMissingOnlyWhenInitialized(t *testing.T) {
	s := warm()
	uninitialized(s)

	for _, v := range status.Build(s, testConfig(), now).Voters {
		assert.False(t, v.KeyMissing, v.Name)
	}
}

func opEntry(t *testing.T, name string, state dcctlv1.Operation_State, created time.Time) *consul.KVEntry {
	t.Helper()

	b, err := kv.EncodeProto(&dcctlv1.Operation{
		Name: name, State: state, CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(created),
		Leader: &dcctlv1.Instance{Datacenter: dcB},
	})
	require.NoError(t, err)

	return &consul.KVEntry{Key: kv.OpKey, Value: b}
}

func TestBuild_operation(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		s := warm()
		s.Voters[0].KV.Data.Op = opEntry(t, "operations/old", dcctlv1.Operation_SUCCEEDED, lost)
		s.Voters[1].KV.Data.Op = opEntry(t, "operations/new", dcctlv1.Operation_RUNNING, observed)

		op := status.Build(s, testConfig(), now).Operation
		require.NotNil(t, op)
		assert.Equal(t, "operations/new", op.GetName())
	})

	t.Run("finished_in_another_copy", func(t *testing.T) {
		s := warm()
		s.Voters[0].KV.Data.Op = opEntry(t, "operations/x", dcctlv1.Operation_RUNNING, lost)
		s.Voters[1].KV.Data.Op = opEntry(t, "operations/x", dcctlv1.Operation_SUCCEEDED, lost)

		assert.Nil(t, status.Build(s, testConfig(), now).Operation)
	})

	t.Run("none", func(t *testing.T) {
		st := status.Build(warm(), testConfig(), now)
		assert.Nil(t, st.Operation)
		assert.Nil(t, status.Operations(warm()).Blocking())
	})

	t.Run("invalid_copy", func(t *testing.T) {
		s := warm()
		s.Voters[1].KV.Data.Op = &consul.KVEntry{Key: kv.OpKey, Value: []byte("{")}

		st := status.Build(s, testConfig(), now)
		assert.Nil(t, st.Operation)

		blocking := status.Operations(s).Blocking()
		require.NotNil(t, blocking)
		assert.Equal(t, dcB, blocking.Name)
		assert.Equal(t, coord.StatusInvalid, blocking.Status)
	})
}

func TestConfigFrom(t *testing.T) {
	cfg := &config.Serve{
		Consul:  config.Consul{NodeMetaKey: "dc"},
		Apps:    config.Apps{MinHealthyTime: time.Minute},
		Patroni: config.Patroni{PlannedMaxLagBytes: 42},
		Status: config.Status{
			Interval: 5 * time.Second, RabbitMQService: "rabbitmq-{datacenter}", InfoServices: []string{"engine"},
		},
	}

	assert.Equal(t, status.Config{
		Interval: 5 * time.Second, NodeMetaKey: "dc", MinHealthyTime: time.Minute, PlannedMaxLagBytes: 42,
		RabbitMQService: "rabbitmq-{datacenter}", InfoServices: []string{"engine"},
	}, status.ConfigFrom(cfg))
}
