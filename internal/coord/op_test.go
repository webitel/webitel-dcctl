package coord_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/kv"
	"github.com/webitel/webitel-dcctl/internal/source"
	"github.com/webitel/webitel-dcctl/internal/source/consul"
)

var opTime = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

func operation(id string, state dcctlv1.Operation_State, leader string) *dcctlv1.Operation {
	return &dcctlv1.Operation{
		Name:       "operations/" + id,
		Type:       dcctlv1.Operation_SWITCH,
		State:      state,
		Leader:     &dcctlv1.Instance{Node: "node-" + leader, Datacenter: "datacenters/" + leader},
		CreateTime: timestamppb.New(opTime),
		UpdateTime: timestamppb.New(opTime),
	}
}

func opJSON(t *testing.T, op *dcctlv1.Operation) []byte {
	t.Helper()

	b, err := kv.EncodeProto(op)
	require.NoError(t, err)

	return b
}

func storedOp(t *testing.T, f *fakeConsul) *dcctlv1.Operation {
	t.Helper()

	op := new(dcctlv1.Operation)
	require.NoError(t, kv.DecodeProto(f.value(kv.OpKey), op))

	return op
}

func newOpStore(t *testing.T, kvs ...coord.KV) *coord.OpStore {
	t.Helper()

	s, err := coord.NewOpStore(kvs, testTimeout, quietLog())
	require.NoError(t, err)

	return s
}

func readOps(t *testing.T, kvs ...coord.KV) coord.OpView {
	t.Helper()

	v, err := newOpStore(t, kvs...).Read(t.Context())
	require.NoError(t, err)

	return v
}

func writes(ws []coord.OpWrite) map[string]coord.OpWriteOutcome {
	out := map[string]coord.OpWriteOutcome{}
	for _, w := range ws {
		out[w.Name] = w.Outcome
	}

	return out
}

func TestNewOpStoreValidates(t *testing.T) {
	_, err := coord.NewOpStore(nil, testTimeout, quietLog())
	require.Error(t, err)

	_, err = coord.NewOpStore([]coord.KV{{Name: "dc_a", Client: newFakeConsul()}}, 0, quietLog())
	require.Error(t, err)
}

func TestCheckIdleInterruptedElsewhere(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_SUCCEEDED, "dc_a")))
	b.set(kv.OpKey, opJSON(t, operation("2", dcctlv1.Operation_INTERRUPTED, "dc_b")))

	err := readOps(t, kvs...).CheckIdle()

	var running *coord.OperationRunningError
	require.ErrorAs(t, err, &running)
	assert.Equal(t, "dc_b", running.Name)
	assert.Equal(t, "operations/2", running.Operation.GetName())

	st, info := errorInfo(t, err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, "OPERATION_RUNNING", info.GetReason())
	assert.Equal(t, "operations/2", info.GetMetadata()["operation"])
}

func TestCheckIdleFinished(t *testing.T) {
	tests := []struct {
		name  string
		state dcctlv1.Operation_State
	}{
		{name: "succeeded", state: dcctlv1.Operation_SUCCEEDED},
		{name: "failed", state: dcctlv1.Operation_FAILED},
		{name: "abandoned", state: dcctlv1.Operation_ABANDONED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, kvs := twoDCs()
			a.set(kv.OpKey, opJSON(t, operation("1", tt.state, "dc_a")))

			v := readOps(t, kvs...)
			require.NoError(t, v.CheckIdle())
			assert.Nil(t, v.Blocking())
		})
	}
}

func TestCheckIdleRunning(t *testing.T) {
	a, _, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_a")))

	var running *coord.OperationRunningError
	require.ErrorAs(t, readOps(t, kvs...).CheckIdle(), &running)
	assert.Equal(t, "dc_a", running.Name)
}

func TestCheckIdleUnknownState(t *testing.T) {
	_, b, kvs := twoDCs()
	b.set(kv.OpKey, []byte(`{"name":"operations/3","state":"PAUSED"}`))

	var running *coord.OperationRunningError
	require.ErrorAs(t, readOps(t, kvs...).CheckIdle(), &running, "a state a newer dcctl added is not finished")
	assert.Equal(t, "operations/3", running.Operation.GetName())
}

func TestCheckIdleInvalid(t *testing.T) {
	a, _, kvs := twoDCs()
	a.set(kv.OpKey, []byte("not json"))

	v := readOps(t, kvs...)
	assert.Equal(t, coord.StatusInvalid, v.Copies[0].Status)

	err := v.CheckIdle()

	var running *coord.OperationRunningError
	require.ErrorAs(t, err, &running, "an unreadable record may hold an unfinished operation")
	assert.Nil(t, running.Operation)
	require.Error(t, running.Err)

	st, info := errorInfo(t, err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.NotContains(t, info.GetMetadata(), "operation")
}

func TestCheckIdleSkipsUnreachable(t *testing.T) {
	_, b, kvs := twoDCs()
	b.set(kv.OpKey, opJSON(t, operation("2", dcctlv1.Operation_RUNNING, "dc_b")))
	b.getErr = unreachable()

	v := readOps(t, kvs...)
	require.NoError(t, v.CheckIdle())

	assert.Equal(t, coord.StatusAbsent, v.Copies[0].Status)
	assert.Equal(t, coord.StatusUnreachable, v.Copies[1].Status)
	assert.Equal(t, source.ReasonRefused, v.Copies[1].Reason)
}

func TestViewLatestAndPrimary(t *testing.T) {
	g, a, b := newFakeConsul(), newFakeConsul(), newFakeConsul()
	kvs := []coord.KV{{Name: "dc_a", Client: a}, {Name: "dc_b", Client: b}, {Name: "dc_c", Client: g}}

	older := operation("0", dcctlv1.Operation_SUCCEEDED, "dc_a")
	older.CreateTime = timestamppb.New(opTime.Add(-time.Hour))
	g.set(kv.OpKey, opJSON(t, older))

	stale := operation("1", dcctlv1.Operation_RUNNING, "dc_b")
	b.set(kv.OpKey, opJSON(t, stale))

	fresh := operation("1", dcctlv1.Operation_INTERRUPTED, "dc_b")
	fresh.UpdateTime = timestamppb.New(opTime.Add(time.Minute))
	a.set(kv.OpKey, opJSON(t, fresh))

	v := readOps(t, kvs...)

	require.NotNil(t, v.Latest())
	assert.Equal(t, "dc_a", v.Latest().Name, "the latest update of the newest operation")
	require.NotNil(t, v.Primary())
	assert.Equal(t, "dc_b", v.Primary().Name, "the copy in the datacenter of the leader")
	assert.Equal(t, dcctlv1.Operation_RUNNING, v.Primary().Operation.GetState())
}

func TestViewPrimaryOtherOperation(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))

	older := operation("0", dcctlv1.Operation_SUCCEEDED, "dc_b")
	older.CreateTime = timestamppb.New(opTime.Add(-time.Hour))
	b.set(kv.OpKey, opJSON(t, older))

	v := readOps(t, kvs...)
	assert.Equal(t, "dc_a", v.Latest().Name)
	assert.Nil(t, v.Primary(), "the leader's datacenter has another operation")
}

func TestViewStretchPrimary(t *testing.T) {
	g := newFakeConsul()
	g.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))

	v := readOps(t, coord.KV{Name: coord.StretchName, Client: g})
	require.NotNil(t, v.Primary())
	assert.Equal(t, coord.StretchName, v.Primary().Name)
}

func TestViewEmpty(t *testing.T) {
	_, _, kvs := twoDCs()

	v := readOps(t, kvs...)
	assert.Nil(t, v.Latest())
	assert.Nil(t, v.Primary())
	require.NoError(t, v.CheckIdle())
}

func TestWriteAllAndPrimary(t *testing.T) {
	a, b, kvs := twoDCs()
	op := operation("1", dcctlv1.Operation_RUNNING, "dc_b")

	got, err := newOpStore(t, kvs...).Write(t.Context(), op)
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, coord.OpWrite{Name: "dc_a", Outcome: coord.OpWritten}, got[0])
	assert.Equal(t, coord.OpWrite{Name: "dc_b", Primary: true, Outcome: coord.OpWritten}, got[1])

	assert.Equal(t, "RUNNING", storedOp(t, a).GetState().String())
	assert.Equal(t, "operations/1", storedOp(t, b).GetName())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(a.value(kv.OpKey), &raw))
	assert.Contains(t, raw, "create_time", "proto field names")
}

func TestWriteStretchPrimary(t *testing.T) {
	g := newFakeConsul()

	got, err := newOpStore(t, coord.KV{Name: coord.StretchName, Client: g}).
		Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_b"))
	require.NoError(t, err)
	assert.Equal(t, []coord.OpWrite{{Name: coord.StretchName, Primary: true, Outcome: coord.OpWritten}}, got)
}

func TestWritePreservesUnknownFields(t *testing.T) {
	a, _, kvs := twoDCs()

	var prev map[string]any
	require.NoError(t, json.Unmarshal(opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_a")), &prev))
	prev["future"] = map[string]any{"x": 1.0}
	prev["leader"].(map[string]any)["zone"] = "z1"

	b, err := json.Marshal(prev)
	require.NoError(t, err)
	a.set(kv.OpKey, b)

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_INTERRUPTED, "dc_a"))
	require.NoError(t, err)
	assert.Equal(t, coord.OpWritten, writes(got)["dc_a"])

	var stored map[string]any
	require.NoError(t, json.Unmarshal(a.value(kv.OpKey), &stored))
	assert.Equal(t, "INTERRUPTED", stored["state"])
	assert.Equal(t, map[string]any{"x": 1.0}, stored["future"])
	assert.Equal(t, "z1", stored["leader"].(map[string]any)["zone"])
}

func TestWriteDoesNotInheritFromOtherOperation(t *testing.T) {
	a, _, kvs := twoDCs()
	a.set(kv.OpKey, []byte(`{"name":"operations/0","state":"SUCCEEDED","future":1}`))

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)
	assert.Equal(t, coord.OpWritten, writes(got)["dc_a"])
	assert.NotContains(t, string(a.value(kv.OpKey)), "future")
}

func TestWriteRereadsOnMismatch(t *testing.T) {
	a, _, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_a")))
	a.beforeCAS = func(f *fakeConsul) {
		f.set(kv.OpKey, []byte(`{"name":"operations/1","state":"RUNNING","future":2}`))
	}

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_INTERRUPTED, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, coord.OpWritten, writes(got)["dc_a"])
	assert.Equal(t, 2, a.casCalls, "the write is retried on a fresh read")
	assert.Contains(t, string(a.value(kv.OpKey)), `"future":2`, "with what the fresh read had")
}

func TestWriteOutcomeUnknownRereads(t *testing.T) {
	a, _, kvs := twoDCs()
	a.casErr = outcomeUnknown()
	a.casApplied = true

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, coord.OpWritten, writes(got)["dc_a"], "the re-read finds the write applied")
	assert.Equal(t, 1, a.casCalls)
}

func TestWriteOutcomeUnknown(t *testing.T) {
	a, _, kvs := twoDCs()
	a.casErr = outcomeUnknown()

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	require.Equal(t, coord.OpUnknown, writes(got)["dc_a"])
	require.ErrorIs(t, got[0].Err, consul.ErrOutcomeUnknown)
	require.ErrorIs(t, got[0].Err, coord.ErrOutcomeUnknown)
	assert.Equal(t, coord.OpWritten, writes(got)["dc_b"])
}

func TestWriteKeepsOtherUnfinished(t *testing.T) {
	a, b, kvs := twoDCs()
	other := opJSON(t, operation("9", dcctlv1.Operation_INTERRUPTED, "dc_b"))
	b.set(kv.OpKey, other)

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, map[string]coord.OpWriteOutcome{"dc_a": coord.OpWritten, "dc_b": coord.OpConflict}, writes(got))
	assert.Equal(t, other, b.value(kv.OpKey), "another unfinished operation is never overwritten")
	assert.Equal(t, "operations/1", storedOp(t, a).GetName())

	var running *coord.OperationRunningError
	require.ErrorAs(t, got[1].Err, &running)
}

func TestWriteKeepsInvalid(t *testing.T) {
	a, _, kvs := twoDCs()
	a.set(kv.OpKey, []byte("not json"))

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, coord.OpConflict, writes(got)["dc_a"])
	assert.Equal(t, []byte("not json"), a.value(kv.OpKey))
}

func TestWriteKeepsFinished(t *testing.T) {
	a, _, kvs := twoDCs()
	done := opJSON(t, operation("1", dcctlv1.Operation_ABANDONED, "dc_a"))
	a.set(kv.OpKey, done)

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_INTERRUPTED, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, coord.OpConflict, writes(got)["dc_a"])
	require.ErrorIs(t, got[0].Err, coord.ErrOperationFinished)
	assert.Equal(t, done, a.value(kv.OpKey))
}

func TestWriteUnreachable(t *testing.T) {
	_, b, kvs := twoDCs()
	b.getErr = unreachable()

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, map[string]coord.OpWriteOutcome{"dc_a": coord.OpWritten, "dc_b": coord.OpFailed}, writes(got))
	assert.Equal(t, 0, b.casCalls, "nothing is written without a read")
}

func TestWriteRequiresName(t *testing.T) {
	_, _, kvs := twoDCs()

	_, err := newOpStore(t, kvs...).Write(t.Context(), &dcctlv1.Operation{State: dcctlv1.Operation_RUNNING})
	require.Error(t, err)
}

func TestCheckIdleStaleCopyOfFinished(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))
	b.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_SUCCEEDED, "dc_b")))

	v := readOps(t, kvs...)
	assert.Nil(t, v.Blocking(), "the operation finished in another datacenter")
	require.NoError(t, v.CheckIdle())
}

func TestCheckIdleStaleCopyOfOtherOperation(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))
	b.set(kv.OpKey, opJSON(t, operation("2", dcctlv1.Operation_SUCCEEDED, "dc_b")))

	var running *coord.OperationRunningError
	require.ErrorAs(t, readOps(t, kvs...).CheckIdle(), &running)
	assert.Equal(t, "operations/1", running.Operation.GetName())
}

func TestOpAlignFinishesStaleCopy(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))

	final := []byte(`{"name":"operations/1","state":"SUCCEEDED","future":1}`)
	b.set(kv.OpKey, final)

	got, err := newOpStore(t, kvs...).Align(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []coord.Alignment{{Voter: "dc_a", Outcome: coord.AlignWritten}}, got)
	assert.Equal(t, final, a.value(kv.OpKey), "the final copy as stored")

	got, err = newOpStore(t, kvs...).Align(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got, "nothing left to align")
}

func TestOpAlignLeavesOtherOperation(t *testing.T) {
	a, b, kvs := twoDCs()
	running := opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b"))
	a.set(kv.OpKey, running)
	b.set(kv.OpKey, opJSON(t, operation("2", dcctlv1.Operation_SUCCEEDED, "dc_b")))

	got, err := newOpStore(t, kvs...).Align(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, running, a.value(kv.OpKey))
}

func TestOpAlignMismatch(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))
	b.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_FAILED, "dc_b")))

	a.beforeCAS = func(f *fakeConsul) {
		f.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_INTERRUPTED, "dc_b")))
	}

	got, err := newOpStore(t, kvs...).Align(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, coord.AlignMismatch, got[0].Outcome)
	require.ErrorIs(t, got[0].Err, coord.ErrVersionMismatch)
}

func TestOpAlignOutcomeUnknown(t *testing.T) {
	a, b, kvs := twoDCs()
	a.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_RUNNING, "dc_b")))
	b.set(kv.OpKey, opJSON(t, operation("1", dcctlv1.Operation_ABANDONED, "dc_b")))

	a.casErr = outcomeUnknown()

	got, err := newOpStore(t, kvs...).Align(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, coord.AlignUnknown, got[0].Outcome)
	require.ErrorIs(t, got[0].Err, coord.ErrOutcomeUnknown)
}

func TestWriteUnknownThenUnreachable(t *testing.T) {
	a, _, kvs := twoDCs()
	a.casErr = outcomeUnknown()
	a.casApplied = true
	a.beforeCAS = func(f *fakeConsul) { f.with(func(f *fakeConsul) { f.getErr = unreachable() }) }

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, coord.OpUnknown, writes(got)["dc_a"], "the write may have been applied")
	require.ErrorIs(t, got[0].Err, coord.ErrOutcomeUnknown)
}

func TestWriteUnknownThenConflict(t *testing.T) {
	a, _, kvs := twoDCs()
	a.casErr = outcomeUnknown()
	a.beforeCAS = func(f *fakeConsul) {
		f.set(kv.OpKey, opJSON(t, operation("9", dcctlv1.Operation_RUNNING, "dc_b")))
	}

	got, err := newOpStore(t, kvs...).Write(t.Context(), operation("1", dcctlv1.Operation_RUNNING, "dc_a"))
	require.NoError(t, err)

	assert.Equal(t, coord.OpUnknown, writes(got)["dc_a"])
	require.ErrorIs(t, got[0].Err, coord.ErrOutcomeUnknown)

	var running *coord.OperationRunningError
	require.ErrorAs(t, got[0].Err, &running, "the cause is kept")
}

func TestViewOf(t *testing.T) {
	present := func(name string, op *dcctlv1.Operation) coord.OpCopy {
		return coord.OpCopy{Name: name, Status: coord.StatusPresent, Operation: op}
	}
	updated := func(op *dcctlv1.Operation, d time.Duration) *dcctlv1.Operation {
		op.UpdateTime = timestamppb.New(opTime.Add(d))

		return op
	}

	tests := []struct {
		name   string
		copies []coord.OpCopy
		want   string
	}{
		{
			name: "finished_over_newer_unfinished",
			copies: []coord.OpCopy{
				present("dc_a", updated(operation("1", dcctlv1.Operation_SUCCEEDED, "dc_a"), time.Minute)),
				present("dc_b", updated(operation("1", dcctlv1.Operation_RUNNING, "dc_b"), 2*time.Minute)),
			},
			want: "dc_a",
		},
		{
			name: "leader_copy_over_newer",
			copies: []coord.OpCopy{
				present("dc_a", updated(operation("1", dcctlv1.Operation_RUNNING, "dc_b"), time.Minute)),
				present("dc_b", operation("1", dcctlv1.Operation_RUNNING, "dc_b")),
			},
			want: "dc_b",
		},
		{
			name: "latest_without_leader_copy",
			copies: []coord.OpCopy{
				present("dc_a", updated(operation("1", dcctlv1.Operation_RUNNING, "dc_b"), time.Minute)),
				present("dc_c", operation("1", dcctlv1.Operation_RUNNING, "dc_b")),
				{Name: "dc_b", Status: coord.StatusUnreachable, Reason: source.ReasonTimeout},
			},
			want: "dc_a",
		},
		{
			name: "leader_kv_holds_another_operation",
			copies: []coord.OpCopy{
				present("dc_a", operation("1", dcctlv1.Operation_RUNNING, "dc_b")),
				present("dc_b", operation("2", dcctlv1.Operation_RUNNING, "dc_b")),
			},
			want: "dc_a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coord.OpView{Copies: tt.copies}.Of("operations/1")

			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.Name)
			assert.Equal(t, "operations/1", got.Operation.GetName())
		})
	}
}

func TestViewOfMissing(t *testing.T) {
	v := coord.OpView{Copies: []coord.OpCopy{
		{Name: "dc_a", Status: coord.StatusPresent, Operation: operation("2", dcctlv1.Operation_RUNNING, "dc_a")},
		{Name: "dc_b", Status: coord.StatusInvalid},
	}}

	assert.Nil(t, v.Of("operations/1"))
}

func TestWriteKeepsNewerRevision(t *testing.T) {
	withRevision := func(rev int64, state dcctlv1.Operation_State, leader string) *dcctlv1.Operation {
		op := operation("op-a", state, leader)
		op.Revision = rev

		return op
	}

	tests := []struct {
		name   string
		stored *dcctlv1.Operation
		write  *dcctlv1.Operation
		want   coord.OpWriteOutcome
	}{
		{
			// A write that started under the lock lands after another
			// instance interrupted the operation.
			name:   "older",
			stored: withRevision(6, dcctlv1.Operation_INTERRUPTED, "dc_b"),
			write:  withRevision(5, dcctlv1.Operation_RUNNING, "dc_a"),
			want:   coord.OpConflict,
		},
		{
			name:   "same_revision_other_change",
			stored: withRevision(6, dcctlv1.Operation_INTERRUPTED, "dc_b"),
			write:  withRevision(6, dcctlv1.Operation_INTERRUPTED, "dc_a"),
			want:   coord.OpConflict,
		},
		{
			name:   "newer",
			stored: withRevision(6, dcctlv1.Operation_RUNNING, "dc_a"),
			write:  withRevision(7, dcctlv1.Operation_RUNNING, "dc_a"),
			want:   coord.OpWritten,
		},
		{
			name:   "written_before_revisions",
			stored: withRevision(0, dcctlv1.Operation_RUNNING, "dc_a"),
			write:  withRevision(0, dcctlv1.Operation_INTERRUPTED, "dc_a"),
			want:   coord.OpWritten,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFakeConsul()
			a.set(kv.OpKey, opJSON(t, tt.stored))

			ws, err := newOpStore(t, coord.KV{Name: "dc_a", Client: a}).Write(t.Context(), tt.write)
			require.NoError(t, err)
			assert.Equal(t, tt.want, ws[0].Outcome)

			if tt.want == coord.OpConflict {
				require.ErrorIs(t, ws[0].Err, coord.ErrOpSuperseded)
				assert.Equal(t, tt.stored.GetRevision(), storedOp(t, a).GetRevision())
				assert.Equal(t, tt.stored.GetLeader().GetNode(), storedOp(t, a).GetLeader().GetNode())
			}
		})
	}
}
