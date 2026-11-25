package status_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/collect"
	"github.com/webitel/webitel-dcctl/internal/config"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/journal"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
	"github.com/webitel/webitel-dcctl/internal/status"
)

const (
	// cycle is the time between two collection cycles of the tests.
	cycle         = 5 * time.Second
	degradedAfter = time.Minute
	peerAddress   = "10.1.0.81:10023"
)

type fakeJournal struct {
	mu      sync.Mutex
	entries []*dcctlv1.JournalEntry
}

func (j *fakeJournal) Append(_ context.Context, e *dcctlv1.JournalEntry) (journal.Appended, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.entries = append(j.entries, proto.CloneOf(e))

	return journal.Appended{Entry: e}, nil
}

// take returns the entries appended since the last take.
func (j *fakeJournal) take() []*dcctlv1.JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()

	out := j.entries
	j.entries = nil

	return out
}

// harness feeds snapshots to a tracker the way the collection loop does, in a synctest
// bubble. The active datacenter dc_a renews the lease on every cycle unless frozen.
type harness struct {
	t       *testing.T
	tracker *status.Tracker
	journal *fakeJournal
	counter int64
	frozen  bool
	seq     uint64
}

func trackerConfig() status.TrackerConfig {
	return status.TrackerConfig{
		Model:             testConfig(),
		DegradedAfter:     degradedAfter,
		LeasePeriod:       30 * time.Second,
		LeaseAcquireAfter: 30 * time.Second,
		PeerAddress:       peerAddress,
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	j := &fakeJournal{}

	tr, err := status.NewTracker(trackerConfig(), j, logging.New(logging.WithProvider(logtest.NewRecorder())))
	require.NoError(t, err)

	return &harness{t: t, tracker: tr, journal: j}
}

// observe passes s to the tracker as a snapshot collected now.
func (h *harness) observe(s *collect.Snapshot) *status.Observed {
	h.t.Helper()

	if !h.frozen {
		h.counter++
	}

	at := time.Now()
	h.seq++
	s.Seq, s.ObserveTime = h.seq, at

	for i := range s.Voters {
		s.Voters[i].KV.Data.Lease = leaseEntry(dcA, 5, h.counter)
	}

	if s.Arbiter != nil {
		s.Arbiter.Data.Lease = arbiterRecord(kv.Lease{Holder: dcA, Generation: 5, Counter: h.counter})
	}

	// The tasks of the fixtures started long before the cycle.
	for _, d := range s.Datacenters {
		for _, allocs := range d.Nomad.Data.Allocs {
			for i := range allocs {
				for name, ts := range allocs[i].TaskStates {
					ts.StartedAt = at.Add(-time.Hour)
					allocs[i].TaskStates[name] = ts
				}
			}
		}
	}

	return h.tracker.Observe(h.t.Context(), s)
}

// settle observes two healthy cycles, which see the lease renewed.
func (h *harness) settle(base func() *collect.Snapshot) {
	h.t.Helper()

	h.observe(base())
	time.Sleep(cycle)

	obs := h.observe(base())
	require.Empty(h.t, obs.Alarms)
	require.Equal(h.t, coord.LeaseHeld, obs.Lease.State)

	h.journal.take()
	time.Sleep(cycle)
}

// until observes snap every cycle until the alarm of key is raised or limit passes,
// and returns how long it took.
func (h *harness) until(snap func() *collect.Snapshot, k status.AlarmKey, limit time.Duration) (time.Duration, bool) {
	h.t.Helper()

	start := time.Now()

	for time.Since(start) <= limit {
		if alarmOf(h.observe(snap()), k) != nil {
			return time.Since(start), true
		}

		time.Sleep(cycle)
	}

	return 0, false
}

// hold observes snap every cycle for d and reports whether the alarm of key was raised
// on every one of them.
func (h *harness) hold(snap func() *collect.Snapshot, k status.AlarmKey, d time.Duration) bool {
	h.t.Helper()

	always := true

	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(cycle) {
		always = always && alarmOf(h.observe(snap()), k) != nil
	}

	return always
}

func alarmOf(obs *status.Observed, k status.AlarmKey) *status.Alarm {
	for i := range obs.Alarms {
		if obs.Alarms[i].Key() == k {
			return &obs.Alarms[i]
		}
	}

	return nil
}

func dcResource(dc string) string {
	if dc == "" {
		return ""
	}

	return "datacenters/" + dc
}

// entriesOf returns the journal entries of the alarm of k.
func entriesOf(es []*dcctlv1.JournalEntry, k status.AlarmKey) []*dcctlv1.JournalEntry {
	var out []*dcctlv1.JournalEntry

	for _, e := range es {
		if e.GetAlarmType() != k.ID.Type() || e.GetDatacenter() != dcResource(k.Datacenter) {
			continue
		}

		if k.Voter != "" && e.GetParams().GetFields()["voter"].GetStringValue() != k.Voter {
			continue
		}

		out = append(out, e)
	}

	return out
}

func results(es []*dcctlv1.JournalEntry) []dcctlv1.JournalEntry_Result {
	out := make([]dcctlv1.JournalEntry_Result, 0, len(es))
	for _, e := range es {
		out = append(out, e.GetResult())
	}

	return out
}

func TestTracker_raiseAndClearEachAlarm(t *testing.T) {
	tests := []struct {
		name   string
		base   func() *collect.Snapshot
		bad    func(s *collect.Snapshot)
		frozen bool
		key    status.AlarmKey
		// after is how long the condition holds before the alarm is raised.
		after time.Duration
		want  dcctlv1.JournalEntry_Result
	}{
		{
			name: "conflict", base: warm, bad: func(s *collect.Snapshot) { setVoterState(s, dcB, forcedState(5, dcB)) },
			key: key(status.AlarmConflict, ""), want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "uncommitted", base: warm, bad: func(s *collect.Snapshot) { setVoterState(s, dcB, stateEntry(6, dcB)) },
			key: key(status.AlarmUncommitted, ""), want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "stale", base: warm, bad: func(s *collect.Snapshot) { setVoterState(s, dcB, stateEntry(4, dcA)) },
			key: key(status.AlarmStale, dcB), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "split_brain", base: warm, bad: startEngineIn(dcB),
			key: key(status.AlarmSplitBrain, dcB), want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "split_brain_role_undetermined", base: warm, bad: all(startEngineIn(dcB), roleUndetermined),
			key: key(status.AlarmSplitBrain, ""), want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "db_split", base: warm, bad: promoteIn(dcB, "pg-b1"),
			key: key(status.AlarmDBSplit, dcB), want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "multiple_primaries", base: warm, bad: tagPrimary(dcA),
			key: key(status.AlarmMultiplePrimaries, ""), want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "db_primary_missing", base: warm, bad: dropLeader(dcA, "pg-a1"),
			key: key(status.AlarmDBPrimaryMissing, dcA), after: degradedAfter, want: dcctlv1.JournalEntry_ERROR,
		},
		{
			name: "datacenter_unreachable", base: warm, bad: allSourcesDownIn(dcB),
			key: key(status.AlarmDatacenterUnreachable, dcB), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "db_leader_mismatch", base: stretch, bad: leaderIn,
			key: key(status.AlarmDBLeaderMismatch, ""), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			// The counter last changed a cycle before it froze; the lease is lost once
			// T + acquire_after passed since.
			name: "lease_lost", base: warm, bad: func(*collect.Snapshot) {}, frozen: true,
			key: key(status.AlarmLeaseLost, ""), after: 60 * time.Second, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "apps_degraded", base: warm, bad: unhealthyEngine(dcA),
			key: key(status.AlarmAppsDegraded, dcA), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "db_degraded", base: warm, bad: memberDown("pg-b2"),
			key: key(status.AlarmDBDegraded, dcB), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "standby_not_following", base: warm, bad: setStandbyHost("10.0.1.2"),
			key: key(status.AlarmStandbyNotFollowing, dcB), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "voter_unreachable", base: warm, bad: voterDownFn(dcB),
			key: voterKey(status.AlarmVoterUnreachable, dcB, dcB), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "quorum_lost", base: warm, bad: roleUndetermined,
			key: key(status.AlarmQuorumLost, ""), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "arbiter_uninitialized", base: warm, bad: func(s *collect.Snapshot) { s.Arbiter.Data = collect.Arbiter{} },
			key:   voterKey(status.AlarmArbiterUninitialized, "", coord.ArbiterName),
			after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
		{
			name: "operation_interrupted", base: warm, bad: operationIn(t, dcctlv1.Operation_INTERRUPTED),
			key: key(status.AlarmOperationInterrupted, ""), after: degradedAfter, want: dcctlv1.JournalEntry_WARNING,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.settle(tt.base)

				h.frozen = tt.frozen
				took, ok := h.until(with(tt.base, tt.bad), tt.key, 3*time.Minute)
				require.True(t, ok, "not raised")
				assert.Equal(t, tt.after, took)
				assert.Equal(t, []dcctlv1.JournalEntry_Result{tt.want}, results(entriesOf(h.journal.take(), tt.key)))

				time.Sleep(cycle)

				h.frozen = false
				assert.Nil(t, alarmOf(h.observe(tt.base()), tt.key), "not cleared")
				assert.Equal(t, []dcctlv1.JournalEntry_Result{dcctlv1.JournalEntry_OK}, results(entriesOf(h.journal.take(), tt.key)))
			})
		})
	}
}

// Data a source gave before it stopped answering never clears an alarm, however fine
// it looks.
func TestTracker_unreachableSourceKeepsAlarm(t *testing.T) {
	tests := []struct {
		name        string
		bad         func(s *collect.Snapshot)
		unreachable func(s *collect.Snapshot)
		key         status.AlarmKey
	}{
		{name: "split_brain_nomad", bad: startEngineIn(dcB), unreachable: nomadDown(dcB), key: key(status.AlarmSplitBrain, dcB)},
		{name: "db_split_patroni", bad: promoteIn(dcB, "pg-b1"), unreachable: patroniDown(dcB), key: key(status.AlarmDBSplit, dcB)},
		{
			name: "split_brain_datacenter", bad: startEngineIn(dcB), unreachable: allSourcesDownIn(dcB),
			key: key(status.AlarmSplitBrain, dcB),
		},
		{
			name: "conflict_voter", bad: func(s *collect.Snapshot) { setVoterState(s, dcB, forcedState(5, dcB)) },
			unreachable: voterDownFn(dcB), key: key(status.AlarmConflict, ""),
		},
		{
			name: "apps_degraded_role", bad: unhealthyEngine(dcA), unreachable: roleUndetermined,
			key: key(status.AlarmAppsDegraded, dcA),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.settle(warm)

				_, ok := h.until(with(warm, tt.bad), tt.key, 3*time.Minute)
				require.True(t, ok, "not raised")
				h.journal.take()

				time.Sleep(cycle)
				assert.True(t, h.hold(with(warm, tt.unreachable), tt.key, 3*time.Minute), "cleared while unreachable")
				assert.Empty(t, entriesOf(h.journal.take(), tt.key))

				assert.Nil(t, alarmOf(h.observe(warm()), tt.key), "not cleared once the source answers")
			})
		})
	}
}

// runningRecord leaves the switch to dc_b running in dcctl/op of every KV, with no
// lock behind it, as after its leader died.
func runningRecord(t *testing.T) func(s *collect.Snapshot) {
	t.Helper()

	return func(s *collect.Snapshot) {
		b, err := kv.EncodeProto(&dcctlv1.Operation{
			Name: "operations/switch-1", Type: dcctlv1.Operation_SWITCH, State: dcctlv1.Operation_RUNNING,
			Target: "datacenters/" + dcB,
		})
		require.NoError(t, err)

		for i := range s.Voters {
			s.Voters[i].KV.Data.Op = &consul.KVEntry{Key: kv.OpKey, Value: b}
		}
	}
}

// lockFor holds dcctl/lock of the KV of dc for the operation id.
func lockFor(dc, id, session string) func(s *collect.Snapshot) {
	return func(s *collect.Snapshot) {
		for i := range s.Voters {
			if s.Voters[i].Datacenter == dc {
				e := entry(kv.LockKey, kv.Lock{Instance: "dcctl-a", Datacenter: dcA, Operation: id})
				e.Session = session
				s.Voters[i].KV.Data.Lock = e
			}
		}
	}
}

// runningOperation is the switch to dc_b with its leader holding the lock in dc_a.
func runningOperation(t *testing.T) func(s *collect.Snapshot) {
	t.Helper()

	return all(runningRecord(t), lockFor(dcA, "switch-1", "session-1"))
}

// Only a running record confirmed by the lock of its leader holds alarms back.
func TestTracker_operationNeedsLock(t *testing.T) {
	tests := []struct {
		name string
		lock func(s *collect.Snapshot)
	}{
		{name: "no_lock", lock: func(*collect.Snapshot) {}},
		{name: "lock_of_another_operation", lock: lockFor(dcA, "failover-2", "session-1")},
		{name: "lock_without_session", lock: lockFor(dcA, "switch-1", "")},
		{name: "lock_in_unreachable_kv", lock: all(lockFor(dcA, "switch-1", "session-1"), voterDownFn(dcA))},
		{
			name: "lock_in_kv_without_leader",
			lock: all(lockFor(dcA, "switch-1", "session-1"), func(s *collect.Snapshot) { s.Voters[0].KV.Data.Meta.KnownLeader = false }),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.settle(warm)

				k := key(status.AlarmAppsDegraded, dcA)
				took, ok := h.until(with(warm, all(unhealthyEngine(dcA), runningRecord(t), tt.lock)), k, 3*time.Minute)
				require.True(t, ok, "suppressed")
				assert.Equal(t, degradedAfter, took)
			})
		})
	}
}

// After resolve, a decided effective state of a higher generation clears the conflict
// even while a voter that held one of its records does not answer.
func TestTracker_conflictResolved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		k := key(status.AlarmConflict, "")
		conflict := func(s *collect.Snapshot) { setVoterState(s, dcB, forcedState(5, dcB)) }
		_, ok := h.until(with(warm, conflict), k, time.Minute)
		require.True(t, ok)

		time.Sleep(cycle)
		assert.True(t, h.hold(with(warm, all(conflict, voterDownFn(dcB))), k, time.Minute), "cleared at the same generation")

		resolved := func(s *collect.Snapshot) {
			conflict(s)
			setVoterState(s, dcA, stateEntry(6, dcA))
			s.Arbiter.Data.State = arbiterRecord(kv.State{Generation: 6, ActiveDatacenter: dcA})
			voterDown(s, dcB)
		}
		assert.Nil(t, alarmOf(h.observe(with(warm, resolved)()), k))
		assert.Equal(t, []dcctlv1.JournalEntry_Result{dcctlv1.JournalEntry_ERROR, dcctlv1.JournalEntry_OK},
			results(entriesOf(h.journal.take(), k)))
	})
}

// A snapshot not newer than the last one observed changes nothing.
func TestTracker_dropsOlderSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		cur := h.tracker.Current()

		old := with(warm, startEngineIn(dcB))()
		old.Seq, old.ObserveTime = h.seq, time.Now()

		assert.Same(t, cur, h.tracker.Observe(t.Context(), old))
		assert.Same(t, cur, h.tracker.Current())
		assert.Empty(t, h.journal.take())
	})
}

func TestTracker_operationSuppressesListedAlarms(t *testing.T) {
	tests := []struct {
		name   string
		base   func() *collect.Snapshot
		bad    func(s *collect.Snapshot)
		frozen bool
		key    status.AlarmKey
		// afterOperation is how long the condition holds after the operation before
		// the alarm is raised: the delay starts anew.
		afterOperation time.Duration
	}{
		{
			name: "db_primary_missing", base: warm, bad: dropLeader(dcA, "pg-a1"),
			key: key(status.AlarmDBPrimaryMissing, dcA), afterOperation: degradedAfter,
		},
		{
			name: "apps_degraded", base: warm, bad: unhealthyEngine(dcA),
			key: key(status.AlarmAppsDegraded, dcA), afterOperation: degradedAfter,
		},
		{
			name: "standby_not_following", base: warm, bad: setStandbyHost("10.0.1.2"),
			key: key(status.AlarmStandbyNotFollowing, dcB), afterOperation: degradedAfter,
		},
		{
			name: "db_leader_mismatch", base: stretch, bad: leaderIn,
			key: key(status.AlarmDBLeaderMismatch, ""), afterOperation: degradedAfter,
		},
		{name: "lease_lost", base: warm, bad: func(*collect.Snapshot) {}, frozen: true, key: key(status.AlarmLeaseLost, "")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.settle(tt.base)

				h.frozen = tt.frozen
				_, raised := h.until(with(tt.base, all(tt.bad, runningOperation(t))), tt.key, 3*time.Minute)
				assert.False(t, raised, "raised during the operation")

				took, ok := h.until(with(tt.base, tt.bad), tt.key, 3*time.Minute)
				require.True(t, ok, "not raised after the operation")
				assert.Equal(t, tt.afterOperation, took)
			})
		})
	}
}

func TestTracker_operationKeepsOtherAlarms(t *testing.T) {
	tests := []struct {
		name string
		bad  func(s *collect.Snapshot)
		key  status.AlarmKey
	}{
		{name: "conflict", bad: func(s *collect.Snapshot) { setVoterState(s, dcB, forcedState(5, dcB)) }, key: key(status.AlarmConflict, "")},
		{name: "split_brain", bad: startEngineIn(dcB), key: key(status.AlarmSplitBrain, dcB)},
		{name: "db_split", bad: promoteIn(dcB, "pg-b1"), key: key(status.AlarmDBSplit, dcB)},
		{name: "multiple_primaries", bad: tagPrimary(dcA), key: key(status.AlarmMultiplePrimaries, "")},
		{name: "db_degraded", bad: memberDown("pg-b2"), key: key(status.AlarmDBDegraded, dcB)},
		{name: "datacenter_unreachable", bad: allSourcesDownIn(dcB), key: key(status.AlarmDatacenterUnreachable, dcB)},
		{name: "voter_unreachable", bad: voterDownFn(dcB), key: voterKey(status.AlarmVoterUnreachable, dcB, dcB)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.settle(warm)

				_, raised := h.until(with(warm, all(tt.bad, runningOperation(t))), tt.key, 3*time.Minute)
				assert.True(t, raised)
			})
		})
	}
}

// An alarm raised before an operation starts stays raised while it runs.
func TestTracker_operationKeepsRaisedAlarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		k := key(status.AlarmAppsDegraded, dcA)
		_, ok := h.until(with(warm, unhealthyEngine(dcA)), k, 3*time.Minute)
		require.True(t, ok)

		time.Sleep(cycle)
		assert.True(t, h.hold(with(warm, all(unhealthyEngine(dcA), runningOperation(t))), k, 2*time.Minute))
		assert.Nil(t, alarmOf(h.observe(with(warm, runningOperation(t))()), k), "cleared once the jobs are healthy")
	})
}

// lease_lost is raised when the lease is lost, kept while it cannot be told and
// cleared only once it is held again.
func TestTracker_leaseLostHysteresis(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		k := key(status.AlarmLeaseLost, "")
		h.frozen = true
		_, ok := h.until(warm, k, 3*time.Minute)
		require.True(t, ok)

		// With dc_a and the arbiter unreachable the voters tell neither a held nor a
		// lost lease, even as the counter moves.
		h.frozen = false

		time.Sleep(cycle)

		obs := h.observe(with(warm, all(voterDownFn(dcA), arbiterDown))())
		assert.Equal(t, coord.LeaseUnknown, obs.Lease.State)
		assert.True(t, h.hold(with(warm, all(voterDownFn(dcA), arbiterDown)), k, 2*time.Minute))

		// A voter that answers again after a gap is renewed once seen changing again.
		var cleared bool

		for range 3 {
			obs = h.observe(warm())
			if alarmOf(obs, k) == nil {
				cleared = true

				break
			}

			assert.NotEqual(t, coord.LeaseHeld, obs.Lease.State, "raised while held")
			time.Sleep(cycle)
		}

		assert.True(t, cleared)
		assert.Equal(t, coord.LeaseHeld, obs.Lease.State)
		assert.Equal(t, []dcctlv1.JournalEntry_Result{dcctlv1.JournalEntry_WARNING, dcctlv1.JournalEntry_OK},
			results(entriesOf(h.journal.take(), k)))
	})
}

// A warning whose condition does not hold for status.degraded_after in a row is never
// raised.
func TestTracker_warningFlapping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		k := voterKey(status.AlarmVoterUnreachable, dcB, dcB)

		for range 4 {
			for range 10 {
				assert.Nil(t, alarmOf(h.observe(with(warm, voterDownFn(dcB))()), k))
				time.Sleep(cycle)
			}

			h.observe(warm())
			time.Sleep(cycle)
		}

		assert.Empty(t, h.journal.take())
	})
}

func TestTracker_journalEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		raisedAt := time.Now()
		obs := h.observe(with(warm, startEngineIn(dcB))())

		a := alarmOf(obs, key(status.AlarmSplitBrain, dcB))
		require.NotNil(t, a)
		assert.Equal(t, status.SeverityCritical, a.Severity)
		assert.True(t, raisedAt.Equal(a.StartTime))
		assert.Equal(t, []any{"engine"}, a.Facts["jobs"])

		es := h.journal.take()
		require.Len(t, es, 1)

		e := es[0]
		assert.Equal(t, dcctlv1.JournalEntry_ALARM, e.GetType())
		assert.Equal(t, dcctlv1.Status_Alarm_SPLIT_BRAIN, e.GetAlarmType())
		assert.Equal(t, dcctlv1.JournalEntry_ERROR, e.GetResult())
		assert.Equal(t, "datacenters/dc_b", e.GetDatacenter())
		assert.Equal(t, a.Detail, e.GetDetail())
		assert.True(t, raisedAt.Equal(e.GetCreateTime().AsTime()))
		assert.True(t, proto.Equal(&dcctlv1.Initiator{
			Instance: &dcctlv1.Instance{Node: "dcctl-a", Datacenter: "datacenters/dc_a", Address: peerAddress},
			System:   true,
		}, e.GetInitiator()), e.GetInitiator().String())
		assert.Equal(t, "engine", e.GetParams().GetFields()["jobs"].GetListValue().GetValues()[0].GetStringValue())

		time.Sleep(cycle)
		h.observe(with(warm, startEngineIn(dcB))())
		assert.Empty(t, h.journal.take(), "nothing changed")

		time.Sleep(cycle)
		h.observe(warm())

		es = h.journal.take()
		require.Len(t, es, 1)
		assert.Equal(t, dcctlv1.JournalEntry_OK, es[0].GetResult())
		assert.Equal(t, dcctlv1.Status_Alarm_SPLIT_BRAIN, es[0].GetAlarmType())
		assert.True(t, strings.HasPrefix(es[0].GetDetail(), "cleared: "), es[0].GetDetail())
	})
}

// dc_b, unreachable, becomes the active datacenter: its alarm turns critical, and the
// change is journaled.
func TestTracker_severityChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		k := key(status.AlarmDatacenterUnreachable, dcB)
		_, ok := h.until(with(warm, allSourcesDownIn(dcB)), k, 3*time.Minute)
		require.True(t, ok)
		h.journal.take()

		time.Sleep(cycle)

		obs := h.observe(with(warm, all(allSourcesDownIn(dcB), func(s *collect.Snapshot) {
			setVoterState(s, dcA, stateEntry(6, dcB))
			s.Arbiter.Data.State = arbiterRecord(kv.State{Generation: 6, ActiveDatacenter: dcB})
		}))())

		a := alarmOf(obs, k)
		require.NotNil(t, a)
		assert.Equal(t, status.SeverityCritical, a.Severity)
		assert.Equal(t, []dcctlv1.JournalEntry_Result{dcctlv1.JournalEntry_ERROR}, results(entriesOf(h.journal.take(), k)))
	})
}

func TestTracker_alarmsOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.settle(warm)

		obs := h.observe(with(warm, all(startEngineIn(dcB), promoteIn(dcB, "pg-b1")))())

		got := make([]status.AlarmID, 0, len(obs.Alarms))
		for _, a := range obs.Alarms {
			got = append(got, a.ID)
		}

		assert.Equal(t, []status.AlarmID{status.AlarmSplitBrain, status.AlarmDBSplit, status.AlarmMultiplePrimaries}, got)
	})
}

func TestTracker_current(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		assert.Nil(t, h.tracker.Current())

		obs := h.observe(warm())
		assert.Same(t, obs, h.tracker.Current())
		assert.Equal(t, dcA, obs.Status.Effective.ActiveDatacenter)
	})
}

// fakeWatcher publishes snapshots the way the collector does.
type fakeWatcher struct {
	mu   sync.Mutex
	snap *collect.Snapshot
	next chan struct{}
	seq  uint64
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{next: make(chan struct{})}
}

func (w *fakeWatcher) Watch() (*collect.Snapshot, <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.snap, w.next
}

func (w *fakeWatcher) publish(s *collect.Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.seq++
	s.Seq, s.ObserveTime = w.seq, time.Now()
	w.snap = s
	close(w.next)
	w.next = make(chan struct{})
}

func TestTracker_run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		w := newFakeWatcher()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			defer close(done)

			h.tracker.Run(ctx, w)
		}()

		synctest.Wait()
		assert.Nil(t, h.tracker.Current(), "nothing to observe yet")

		for range 2 {
			time.Sleep(cycle)
			w.publish(warm())
			synctest.Wait()

			cur := h.tracker.Current()
			require.NotNil(t, cur)
			assert.True(t, time.Now().Equal(cur.Status.ObserveTime))
		}

		cancel()
		<-done
	})
}

func TestNewTracker_invalid(t *testing.T) {
	log := logging.New(logging.WithProvider(logtest.NewRecorder()))

	tests := []struct {
		name string
		edit func(c *status.TrackerConfig)
	}{
		{name: "degraded_after", edit: func(c *status.TrackerConfig) { c.DegradedAfter = 0 }},
		{name: "lease_period", edit: func(c *status.TrackerConfig) { c.LeasePeriod = 0 }},
		{name: "lease_acquire_after", edit: func(c *status.TrackerConfig) { c.LeaseAcquireAfter = -time.Second }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := trackerConfig()
			tt.edit(&cfg)

			_, err := status.NewTracker(cfg, &fakeJournal{}, log)
			assert.Error(t, err)
		})
	}
}

func TestTrackerConfigFrom(t *testing.T) {
	cfg := &config.Serve{
		Listen:       config.ServeListen{Peer: peerAddress},
		Coordination: config.Coordination{LeasePeriod: 30 * time.Second, LeaseAcquireAfter: 20 * time.Second},
		Status:       config.Status{Interval: 5 * time.Second, DegradedAfter: time.Minute},
	}

	assert.Equal(t, status.TrackerConfig{
		Model:             status.ConfigFrom(cfg),
		DegradedAfter:     time.Minute,
		LeasePeriod:       30 * time.Second,
		LeaseAcquireAfter: 20 * time.Second,
		PeerAddress:       peerAddress,
	}, status.TrackerConfigFrom(cfg))
}
