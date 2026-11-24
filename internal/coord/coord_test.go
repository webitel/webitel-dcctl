package coord_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
)

type casCall struct {
	value   []byte
	version coord.Version
}

// fakeVoter holds dcctl/state the way a KV or an arbiter does.
type fakeVoter struct {
	name string
	kind coord.Kind

	mu            sync.Mutex
	value         []byte
	version       coord.Version
	uninitialized bool
	getErr        error
	casErr        error
	// block makes Get wait for its context.
	block bool
	// beforeCAS runs at the start of CAS, as another writer would.
	beforeCAS func(f *fakeVoter)
	cas       []casCall
}

func newFake(name string, kind coord.Kind, value []byte) *fakeVoter {
	f := &fakeVoter{name: name, kind: kind, value: value}
	if value != nil {
		f.version = 10
	}

	return f
}

func (f *fakeVoter) Name() string { return f.name }

func (f *fakeVoter) Kind() coord.Kind { return f.kind }

func (f *fakeVoter) Get(ctx context.Context, key coord.Key) (coord.Entry, error) {
	if key != coord.KeyState {
		return coord.Entry{}, fmt.Errorf("unexpected key %s", key)
	}

	f.mu.Lock()
	block := f.block
	f.mu.Unlock()

	if block {
		<-ctx.Done()

		return coord.Entry{}, source.Classify("fake", ctx.Err())
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.getErr != nil:
		return coord.Entry{}, f.getErr
	case f.uninitialized:
		return coord.Entry{}, coord.ErrUninitialized
	case f.value == nil:
		return coord.Entry{}, coord.ErrNotFound
	}

	return coord.Entry{Value: f.value, Version: f.version}, nil
}

func (f *fakeVoter) CAS(_ context.Context, key coord.Key, value []byte, version coord.Version) error {
	if key != coord.KeyState {
		return fmt.Errorf("unexpected key %s", key)
	}

	if f.beforeCAS != nil {
		f.beforeCAS(f)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.cas = append(f.cas, casCall{value: value, version: version})

	switch {
	case f.casErr != nil:
		return f.casErr
	case f.kind == coord.KindArbiter && version == 0:
		return coord.ErrCreateRefused
	case f.uninitialized, version != f.version:
		return coord.ErrVersionMismatch
	}

	f.value = value
	f.version += 7

	return nil
}

func (f *fakeVoter) set(value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.value = value
	f.version += 3
}

func (f *fakeVoter) state() ([]byte, []casCall) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.value, f.cas
}

func raw(t *testing.T, gen int64, dc string, forced bool) []byte {
	t.Helper()

	return stateJSON(t, kv.State{Generation: gen, ActiveDatacenter: dc, Forced: forced, OpID: fmt.Sprintf("op%d", gen)})
}

func newCoordinator(t *testing.T, timeout time.Duration, voters ...*fakeVoter) *coord.Coordinator {
	t.Helper()

	vs := make([]coord.Voter, 0, len(voters))
	for _, v := range voters {
		vs = append(vs, v)
	}

	c, err := coord.New(vs, timeout, logging.New(logging.WithProvider(logtest.NewRecorder())))
	require.NoError(t, err)

	return c
}

// readAlign reads the voters through c and aligns them.
func readAlign(t *testing.T, c *coord.Coordinator) (coord.View, []coord.Alignment) {
	t.Helper()

	view, err := c.Read(t.Context())
	require.NoError(t, err)

	res, err := c.Align(t.Context(), view)
	require.NoError(t, err)

	return view, res
}

func TestNewValidates(t *testing.T) {
	log := logging.New(logging.WithProvider(logtest.NewRecorder()))
	a := newFake("dc_a", coord.KindKV, nil)

	tests := []struct {
		name    string
		voters  []coord.Voter
		timeout time.Duration
	}{
		{name: "no_voters", timeout: time.Second},
		{name: "duplicate_name", voters: []coord.Voter{a, newFake("dc_a", coord.KindKV, nil)}, timeout: time.Second},
		{name: "nil_voter", voters: []coord.Voter{a, nil}, timeout: time.Second},
		{name: "zero_timeout", voters: []coord.Voter{a}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := coord.New(tt.voters, tt.timeout, log)
			require.Error(t, err)
		})
	}
}

func TestReadStatuses(t *testing.T) {
	unreachable := source.Classify("10.0.0.2:8500", context.DeadlineExceeded)

	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_b", false))
	b := newFake("dc_b", coord.KindKV, nil)
	b.getErr = unreachable
	cAbsent := newFake("dc_c", coord.KindKV, nil)
	d := newFake("dc_d", coord.KindKV, []byte(`{"generation":"8"}`))
	e := newFake("dc_e", coord.KindKV, []byte(`{"generation":8}`))
	arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)
	arb.uninitialized = true

	view, err := newCoordinator(t, time.Second, a, b, cAbsent, d, e, arb).Read(t.Context())
	require.NoError(t, err)
	require.Len(t, view.Voters, 6)

	got := make(map[string]coord.VoterState)
	for _, vs := range view.Voters {
		got[vs.Name] = vs
	}

	assert.Equal(t, coord.VoterState{
		Name: "dc_a", Kind: coord.KindKV, Status: coord.StatusPresent,
		State:   kv.State{Generation: 8, ActiveDatacenter: "dc_b", OpID: "op8"},
		Raw:     raw(t, 8, "dc_b", false),
		Version: 10,
	}, got["dc_a"])

	assert.Equal(t, coord.StatusUnreachable, got["dc_b"].Status)
	assert.Equal(t, source.ReasonTimeout, got["dc_b"].Reason)
	require.ErrorIs(t, got["dc_b"].Err, unreachable)

	assert.Equal(t, coord.StatusAbsent, got["dc_c"].Status)

	assert.Equal(t, coord.StatusInvalid, got["dc_d"].Status, "undecodable record")
	assert.Equal(t, source.ReasonInvalid, got["dc_d"].Reason)
	require.Error(t, got["dc_d"].Err)

	assert.Equal(t, coord.StatusInvalid, got["dc_e"].Status, "record without active datacenter")

	assert.Equal(t, coord.StatusUninitialized, got[coord.ArbiterName].Status)
	assert.Equal(t, coord.KindArbiter, got[coord.ArbiterName].Kind)
}

func TestReadTimeout(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_a", false))
	c := newFake("dc_c", coord.KindKV, nil)
	c.block = true

	start := time.Now()

	view, err := newCoordinator(t, 50*time.Millisecond, a, b, c).Read(t.Context())
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)

	assert.Equal(t, coord.StatusUnreachable, view.Voters[2].Status)
	assert.Equal(t, source.ReasonTimeout, view.Voters[2].Reason)
	require.NotNil(t, view.Effective)
	assert.Equal(t, int64(8), view.Effective.State.Generation)
}

func TestReadCanceled(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, nil)
	a.block = true

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := newCoordinator(t, time.Minute, a).Read(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAlignWritesStaleVoter(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	c := newFake("dc_c", coord.KindKV, raw(t, 7, "dc_c", false))
	co := newCoordinator(t, time.Second, a, b, c)

	_, res := readAlign(t, co)
	assert.Equal(t, []coord.Alignment{{Voter: "dc_c", Outcome: coord.AlignWritten}}, res)

	value, calls := c.state()
	assert.Equal(t, effective, value)
	assert.Equal(t, []casCall{{value: effective, version: 10}}, calls)

	view, res := readAlign(t, co)
	assert.Empty(t, res)
	assert.Equal(t, []string{"dc_a", "dc_b", "dc_c"}, view.Effective.Voters)
}

// A datacenter whose KV lost the key gets the effective state back.
func TestAlignCreatesAbsentKey(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	c := newFake("dc_c", coord.KindKV, nil)

	_, res := readAlign(t, newCoordinator(t, time.Second, a, b, c))
	assert.Equal(t, []coord.Alignment{{Voter: "dc_c", Outcome: coord.AlignWritten}}, res)

	value, calls := c.state()
	assert.Equal(t, effective, value)
	assert.Equal(t, []casCall{{value: effective, version: 0}}, calls, "create only if absent")
}

func TestAlignStaleArbiter(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 7, "dc_a", false))

	_, res := readAlign(t, newCoordinator(t, time.Second, a, b, arb))
	assert.Equal(t, []coord.Alignment{{Voter: coord.ArbiterName, Outcome: coord.AlignWritten}}, res)

	value, calls := arb.state()
	assert.Equal(t, effective, value)
	assert.Equal(t, []casCall{{value: effective, version: 10}}, calls)
}

func TestAlignSkipsUninitializedArbiter(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	arb := newFake(coord.ArbiterName, coord.KindArbiter, nil)
	arb.uninitialized = true

	view, res := readAlign(t, newCoordinator(t, time.Second, a, b, arb))
	require.NotNil(t, view.Effective)
	assert.Empty(t, res)

	_, calls := arb.state()
	assert.Empty(t, calls, "an uninitialized arbiter got a write")
}

func TestAlignDoesNotPropagateUncommitted(t *testing.T) {
	committed := raw(t, 8, "dc_b", false)
	uncommitted := raw(t, 9, "dc_c", false)

	t.Run("stale_voter_gets_the_effective_state", func(t *testing.T) {
		a := newFake("dc_a", coord.KindKV, uncommitted)
		b := newFake("dc_b", coord.KindKV, committed)
		c := newFake("dc_c", coord.KindKV, committed)
		d := newFake("dc_d", coord.KindKV, committed)
		e := newFake("dc_e", coord.KindKV, raw(t, 7, "dc_b", false))

		view, res := readAlign(t, newCoordinator(t, time.Second, a, b, c, d, e))
		require.Len(t, view.Uncommitted, 1)
		assert.Equal(t, []coord.Alignment{{Voter: "dc_e", Outcome: coord.AlignWritten}}, res)

		value, _ := e.state()
		assert.Equal(t, committed, value)

		_, calls := a.state()
		assert.Empty(t, calls, "the voter holding the uncommitted record got a write")
	})

	t.Run("no_effective_state", func(t *testing.T) {
		a := newFake("dc_a", coord.KindKV, uncommitted)
		b := newFake("dc_b", coord.KindKV, committed)

		view, res := readAlign(t, newCoordinator(t, time.Second, a, b))
		assert.Nil(t, view.Effective)
		assert.Empty(t, res)

		_, calls := b.state()
		assert.Empty(t, calls)
	})
}

func TestAlignConflict(t *testing.T) {
	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", true))
	b := newFake("dc_b", coord.KindKV, raw(t, 8, "dc_b", true))
	c := newFake("dc_c", coord.KindKV, raw(t, 7, "dc_a", false))

	view, res := readAlign(t, newCoordinator(t, time.Second, a, b, c))
	require.Len(t, view.Conflict, 2)
	assert.Empty(t, res)

	_, calls := c.state()
	assert.Empty(t, calls)
}

func TestAlignSkipsInvalidRecord(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	c := newFake("dc_c", coord.KindKV, []byte(`{"generation":"9"}`))

	_, res := readAlign(t, newCoordinator(t, time.Second, a, b, c))
	assert.Empty(t, res)

	_, calls := c.state()
	assert.Empty(t, calls)
}

func TestAlignOutcomeUnknown(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	stale := raw(t, 7, "dc_c", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	c := newFake("dc_c", coord.KindKV, stale)
	c.casErr = fmt.Errorf("%w: connection reset", coord.ErrOutcomeUnknown)
	co := newCoordinator(t, time.Second, a, b, c)

	_, res := readAlign(t, co)
	require.Len(t, res, 1)
	assert.Equal(t, coord.AlignUnknown, res[0].Outcome)
	require.ErrorIs(t, res[0].Err, coord.ErrOutcomeUnknown)

	c.mu.Lock()
	c.casErr = nil
	c.mu.Unlock()

	view, res := readAlign(t, co)
	assert.True(t, view.Voters[2].Stale, "the next read sees the voter as it is")
	assert.Equal(t, []coord.Alignment{{Voter: "dc_c", Outcome: coord.AlignWritten}}, res)
}

func TestAlignReReadsBeforeCAS(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)

	setup := func(t *testing.T) (*coord.Coordinator, *fakeVoter, coord.View) {
		t.Helper()

		a := newFake("dc_a", coord.KindKV, effective)
		b := newFake("dc_b", coord.KindKV, effective)
		c := newFake("dc_c", coord.KindKV, raw(t, 7, "dc_c", false))
		co := newCoordinator(t, time.Second, a, b, c)

		view, err := co.Read(t.Context())
		require.NoError(t, err)
		require.True(t, view.Voters[2].Stale)

		return co, c, view
	}

	t.Run("version_changed_since_the_view", func(t *testing.T) {
		co, c, view := setup(t)
		c.set(raw(t, 7, "dc_c", true))

		res, err := co.Align(t.Context(), view)
		require.NoError(t, err)
		assert.Equal(t, []coord.Alignment{{Voter: "dc_c", Outcome: coord.AlignWritten}}, res)

		_, calls := c.state()
		require.Len(t, calls, 1)
		assert.Equal(t, coord.Version(13), calls[0].version, "the version of the view was reused")
	})

	t.Run("written_between_reread_and_cas", func(t *testing.T) {
		co, c, view := setup(t)
		concurrent := raw(t, 9, "dc_a", false)
		c.beforeCAS = func(f *fakeVoter) { f.set(concurrent) }

		res, err := co.Align(t.Context(), view)
		require.NoError(t, err)
		require.Len(t, res, 1)
		assert.Equal(t, coord.AlignMismatch, res[0].Outcome)
		require.ErrorIs(t, res[0].Err, coord.ErrVersionMismatch)

		value, _ := c.state()
		assert.Equal(t, concurrent, value, "the concurrent write was overwritten")
	})

	t.Run("voter_caught_up_since_the_view", func(t *testing.T) {
		co, c, view := setup(t)
		c.set(raw(t, 9, "dc_a", false))

		res, err := co.Align(t.Context(), view)
		require.NoError(t, err)
		assert.Equal(t, []coord.Alignment{{Voter: "dc_c", Outcome: coord.AlignSkipped}}, res)

		_, calls := c.state()
		assert.Empty(t, calls)
	})

	t.Run("voter_unreachable_on_reread", func(t *testing.T) {
		co, c, view := setup(t)
		c.mu.Lock()
		c.getErr = source.Classify("10.0.0.3:8500", context.DeadlineExceeded)
		c.mu.Unlock()

		res, err := co.Align(t.Context(), view)
		require.NoError(t, err)
		require.Len(t, res, 1)
		assert.Equal(t, coord.AlignFailed, res[0].Outcome)
		require.Error(t, res[0].Err)

		_, calls := c.state()
		assert.Empty(t, calls)
	})
}

// A newer dcctl may have written fields this version does not know; the
// aligned voter must get them too.
func TestAlignPreservesUnknownFields(t *testing.T) {
	effective := []byte(`{"generation":8,"active_datacenter":"dc_b","op_id":"op8","forced":false,` +
		`"patroni_timeline":5,"updated_at":"2026-11-23T15:30:00Z","witness":"arbiter","quorum":{"voters":3}}`)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	c := newFake("dc_c", coord.KindKV, raw(t, 7, "dc_c", false))

	_, res := readAlign(t, newCoordinator(t, time.Second, a, b, c))
	assert.Equal(t, []coord.Alignment{{Voter: "dc_c", Outcome: coord.AlignWritten}}, res)

	value, _ := c.state()
	assert.Equal(t, string(effective), string(value))
}

func TestAlignRejectsViewOfOtherVoters(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	c := newFake("dc_c", coord.KindKV, raw(t, 7, "dc_c", false))

	view, err := newCoordinator(t, time.Second, a, b, c).Read(t.Context())
	require.NoError(t, err)

	other := newCoordinator(t, time.Second, a, b, newFake("dc_d", coord.KindKV, nil))

	_, err = other.Align(t.Context(), view)
	require.Error(t, err)

	_, calls := c.state()
	assert.Empty(t, calls)
}

// Failover dc_a -> dc_c with dc_a down: 9 is on dc_b and dc_c, and
// needs_recovery reached only dc_c. Copying either payload to dc_a would
// make it the majority one.
func TestAlignSkipsUnsettledRecord(t *testing.T) {
	g9 := kv.State{Generation: 9, ActiveDatacenter: "dc_c", OpID: "op9"}
	recovering := g9
	recovering.NeedsRecovery = []string{"dc_a"}

	a := newFake("dc_a", coord.KindKV, raw(t, 8, "dc_a", false))
	b := newFake("dc_b", coord.KindKV, stateJSON(t, g9))
	c := newFake("dc_c", coord.KindKV, stateJSON(t, recovering))

	view, res := readAlign(t, newCoordinator(t, time.Second, a, b, c))
	require.NotNil(t, view.Effective)
	require.Len(t, res, 1)
	assert.Equal(t, "dc_a", res[0].Voter)
	assert.Equal(t, coord.AlignSkipped, res[0].Outcome)
	require.ErrorIs(t, res[0].Err, coord.ErrUnsettled)

	_, calls := a.state()
	assert.Empty(t, calls)
}

func TestAlignArbiterUninitializedOnReread(t *testing.T) {
	effective := raw(t, 8, "dc_b", false)
	a := newFake("dc_a", coord.KindKV, effective)
	b := newFake("dc_b", coord.KindKV, effective)
	arb := newFake(coord.ArbiterName, coord.KindArbiter, raw(t, 7, "dc_a", false))
	co := newCoordinator(t, time.Second, a, b, arb)

	view, err := co.Read(t.Context())
	require.NoError(t, err)
	require.True(t, view.Voters[2].Stale)

	arb.mu.Lock()
	arb.uninitialized = true
	arb.mu.Unlock()

	res, err := co.Align(t.Context(), view)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, coord.AlignSkipped, res[0].Outcome)
	require.ErrorIs(t, res[0].Err, coord.ErrUninitialized, "an uninitialized arbiter looks caught up")

	_, calls := arb.state()
	assert.Empty(t, calls)
}
