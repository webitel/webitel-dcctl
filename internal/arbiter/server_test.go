package arbiter_test

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/webitel/webitel-go-kit/pkg/logging"
	"github.com/webitel/webitel-go-kit/pkg/logging/logtest"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
	"github.com/webitel/webitel-dcctl/internal/arbiter"
	"github.com/webitel/webitel-dcctl/internal/arbiter/store"
)

var (
	stateJSON = []byte(`{"generation":1,"active_datacenter":"dc_a"}`)
	leaseJSON = []byte(`{"holder":"dc_a","generation":1,"counter":7}`)
	nextJSON  = []byte(`{"generation":2,"active_datacenter":"dc_b"}`)
)

func testLogger() *logging.Logger {
	return logging.New(logging.WithProvider(logtest.NewRecorder()))
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()

	st, err := store.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	return st
}

func newServer(t *testing.T) (*arbiter.Server, *store.Store) {
	t.Helper()

	st := openStore(t, t.TempDir())

	return arbiter.NewServer(st, testLogger()), st
}

func update(t *testing.T, s *arbiter.Server, name, etag string, value []byte, allowMissing bool) (*dcctlv1.Record, error) {
	t.Helper()

	return s.UpdateRecord(t.Context(), &dcctlv1.UpdateRecordRequest{
		Record:       &dcctlv1.Record{Name: name, Value: value, Etag: etag},
		AllowMissing: allowMissing,
	})
}

// initialize creates state and lease and returns them.
func initialize(t *testing.T, s *arbiter.Server) (state, lease *dcctlv1.Record) {
	t.Helper()

	state, err := update(t, s, "records/state", "", stateJSON, true)
	require.NoError(t, err)

	lease, err = update(t, s, "records/lease", "", leaseJSON, true)
	require.NoError(t, err)

	return state, lease
}

// requireReason checks the code and the ErrorInfo reason of err.
func requireReason(t *testing.T, err error, code codes.Code, reason apierr.Reason) *status.Status {
	t.Helper()

	st := status.Convert(err)
	require.Equal(t, code, st.Code(), "error = %v", err)

	var info *errdetails.ErrorInfo

	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}

	require.NotNil(t, info, "no ErrorInfo in %v", err)
	assert.Equal(t, apierr.Domain, info.GetDomain())
	assert.Equal(t, string(reason), info.GetReason())

	return st
}

// mismatchRecord checks that err is ETAG_MISMATCH and returns its record.
func mismatchRecord(t *testing.T, err error) *dcctlv1.Record {
	t.Helper()

	st := requireReason(t, err, codes.Aborted, apierr.ReasonEtagMismatch)

	for _, d := range st.Details() {
		if r, ok := d.(*dcctlv1.Record); ok {
			return r
		}
	}

	require.FailNow(t, "no Record in the details", "error = %v", err)

	return nil
}

func TestUninitialized(t *testing.T) {
	s, _ := newServer(t)

	_, err := s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: "records/state"})
	requireReason(t, err, codes.FailedPrecondition, apierr.ReasonArbiterUninitialized)

	_, err = s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: "records/lease"})
	requireReason(t, err, codes.FailedPrecondition, apierr.ReasonArbiterUninitialized)

	_, err = update(t, s, "records/lease", "", leaseJSON, true)
	requireReason(t, err, codes.FailedPrecondition, apierr.ReasonArbiterUninitialized)

	_, err = update(t, s, "records/state", "1", stateJSON, false)
	requireReason(t, err, codes.FailedPrecondition, apierr.ReasonArbiterUninitialized)
}

func TestCreateState(t *testing.T) {
	s, _ := newServer(t)

	got, err := update(t, s, "records/state", "", stateJSON, true)
	require.NoError(t, err)

	assert.Equal(t, "records/state", got.GetName())
	assert.Equal(t, stateJSON, got.GetValue())
	assert.NotZero(t, got.GetUpdateTime().AsTime())

	index, err := strconv.ParseUint(got.GetEtag(), 10, 64)
	require.NoError(t, err, "etag must be a decimal uint64")
	assert.NotZero(t, index)

	read, err := s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: "records/state"})
	require.NoError(t, err)
	assert.True(t, proto.Equal(got, read), "GetRecord() = %v; want %v", read, got)

	_, err = s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: "records/lease"})
	requireReason(t, err, codes.NotFound, apierr.ReasonRecordNotFound)
}

func TestUpdate(t *testing.T) {
	s, _ := newServer(t)
	state, _ := initialize(t, s)

	got, err := update(t, s, "records/state", state.GetEtag(), nextJSON, false)
	require.NoError(t, err)
	assert.Equal(t, nextJSON, got.GetValue())
	assert.NotEqual(t, state.GetEtag(), got.GetEtag())

	_, err = update(t, s, "records/state", state.GetEtag(), stateJSON, false)
	assert.True(t, proto.Equal(got, mismatchRecord(t, err)), "the current record must be in the details")
}

func TestCreateOverExisting(t *testing.T) {
	s, _ := newServer(t)
	state, _ := initialize(t, s)

	_, err := update(t, s, "records/state", "", nextJSON, true)
	assert.True(t, proto.Equal(state, mismatchRecord(t, err)))
}

func TestUpdateMissingLease(t *testing.T) {
	s, _ := newServer(t)
	state, err := update(t, s, "records/state", "", stateJSON, true)
	require.NoError(t, err)

	_, err = update(t, s, "records/lease", state.GetEtag(), leaseJSON, false)
	requireReason(t, err, codes.NotFound, apierr.ReasonRecordNotFound)
}

func TestInvalidEtag(t *testing.T) {
	s, _ := newServer(t)
	state, _ := initialize(t, s)

	tests := []struct {
		name         string
		etag         string
		allowMissing bool
	}{
		{"empty_without_allow_missing", "", false},
		{"not_a_number", "abc", false},
		{"over_uint64", "18446744073709551616", false},
		{"zero", "0", false},
		{"etag_with_allow_missing", state.GetEtag(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := update(t, s, "records/state", tt.etag, nextJSON, tt.allowMissing)
			requireReason(t, err, codes.InvalidArgument, apierr.ReasonRequestInvalid)
		})
	}
}

func TestUnknownRecord(t *testing.T) {
	s, _ := newServer(t)
	initialize(t, s)

	for _, name := range []string{"records/journal", "state", ""} {
		_, err := s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: name})
		st := requireReason(t, err, codes.InvalidArgument, apierr.ReasonRequestInvalid)
		assert.Equal(t, []string{"name"}, violatedFields(st), "GetRecord(%q)", name)

		_, err = update(t, s, name, "7", nextJSON, false)
		st = requireReason(t, err, codes.InvalidArgument, apierr.ReasonRequestInvalid)
		assert.Equal(t, []string{"record.name"}, violatedFields(st), "UpdateRecord(%q)", name)
	}
}

// violatedFields returns the fields of the BadRequest detail of st.
func violatedFields(st *status.Status) []string {
	var fields []string

	for _, d := range st.Details() {
		if bad, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range bad.GetFieldViolations() {
				fields = append(fields, v.GetField())
			}
		}
	}

	return fields
}

func TestConcurrentUpdate(t *testing.T) {
	s, _ := newServer(t)
	state, _ := initialize(t, s)

	values := [][]byte{[]byte(`{"writer":1}`), []byte(`{"writer":2}`)}
	results := make([]*dcctlv1.Record, len(values))
	errs := make([]error, len(values))

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)

	for i, v := range values {
		wg.Go(func() {
			<-start

			results[i], errs[i] = update(t, s, "records/state", state.GetEtag(), v, false)
		})
	}

	close(start)
	wg.Wait()

	winner, loser := 0, 1
	if errs[0] != nil {
		winner, loser = 1, 0
	}

	require.NoError(t, errs[winner])
	assert.Equal(t, values[winner], results[winner].GetValue())
	assert.True(t, proto.Equal(results[winner], mismatchRecord(t, errs[loser])),
		"the loser must get the winner's record")
}

func TestRecordsSurviveRestart(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	require.NoError(t, err)

	state, lease := initialize(t, arbiter.NewServer(st, testLogger()))
	require.NoError(t, st.Close())

	s := arbiter.NewServer(openStore(t, dir), testLogger())

	for _, want := range []*dcctlv1.Record{state, lease} {
		got, err := s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: want.GetName()})
		require.NoError(t, err)
		assert.True(t, proto.Equal(want, got), "GetRecord(%s) = %v; want %v", want.GetName(), got, want)
	}
}

func TestEtagAboveInt64(t *testing.T) {
	s, _ := newServer(t)
	initialize(t, s)

	_, err := update(t, s, "records/state", "18446744073709551615", nextJSON, false)
	mismatchRecord(t, err)
}

func TestClosedStore(t *testing.T) {
	s, st := newServer(t)
	state, _ := initialize(t, s)
	require.NoError(t, st.Close())

	_, err := s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: "records/state"})
	assert.Equal(t, codes.Unavailable, status.Code(err), "GetRecord() error = %v", err)

	_, err = update(t, s, "records/state", state.GetEtag(), nextJSON, false)
	assert.Equal(t, codes.Unavailable, status.Code(err), "UpdateRecord() error = %v", err)
}

func TestCorruptRecord(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(dir)
	require.NoError(t, err)
	initialize(t, arbiter.NewServer(st, testLogger()))
	require.NoError(t, st.Close())

	db, err := bolt.Open(filepath.Join(dir, "arbiter.db"), 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("records")).Put([]byte("lease"), []byte{1, 2})
	}))
	require.NoError(t, db.Close())

	s := arbiter.NewServer(openStore(t, dir), testLogger())

	_, err = s.GetRecord(t.Context(), &dcctlv1.GetRecordRequest{Name: "records/lease"})
	require.Equal(t, codes.Internal, status.Code(err), "GetRecord() error = %v", err)
	assert.NotContains(t, status.Convert(err).Message(), dir, "the message must not leak the store path")
}
