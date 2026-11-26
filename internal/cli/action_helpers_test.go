package cli

import (
	"regexp"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

const (
	rpcSwitch   = "/webitel.dcctl.v1.DatacenterService/SwitchDatacenter"
	rpcFailover = "/webitel.dcctl.v1.DatacenterService/FailoverDatacenter"
	rpcDemote   = "/webitel.dcctl.v1.DatacenterService/DemoteDatacenter"
	rpcResume   = "/webitel.dcctl.v1.OperationService/ResumeOperation"
	rpcAbandon  = "/webitel.dcctl.v1.OperationService/AbandonOperation"
)

var uuid4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// step is a scripted answer of the fake API to an action.
type step func(method string, req proto.Message) (*dcctlv1.Operation, error)

// script answers the actions in order, the last answer for the calls after
// the script ends.
type script struct {
	mu    sync.Mutex
	steps []step
	n     int
}

func (s *script) next() step {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := min(s.n, len(s.steps)-1)
	s.n++

	return s.steps[i]
}

func (e *env) script(steps ...step) {
	sc := &script{steps: steps}

	e.api.act = func(method string, req proto.Message) (*dcctlv1.Operation, error) {
		return sc.next()(method, req)
	}
}

func answer(op *dcctlv1.Operation) step {
	return func(string, proto.Message) (*dcctlv1.Operation, error) { return op, nil }
}

func failWith(err error) step {
	return func(string, proto.Message) (*dcctlv1.Operation, error) { return nil, err }
}

// planOf is the answer to a dry-run: the checks and the plan, no name.
func planOf(checks ...*dcctlv1.Check) *dcctlv1.Operation {
	op := testOperation("", dcctlv1.Operation_STATE_UNSPECIFIED)
	op.Name = ""
	op.Initiator = nil
	op.Leader = nil
	op.Mode = dcctlv1.Operation_QUORUM
	op.PreviousGeneration, op.NewGeneration = 0, 0
	op.CreateTime, op.UpdateTime = nil, nil

	op.Checks = append([]*dcctlv1.Check{testCheck("coord.lock_free", dcctlv1.Check_PASS, dcctlv1.Check_BLOCK, "the lock is free")}, checks...)

	for _, st := range op.GetSteps() {
		st.State = dcctlv1.Operation_Step_PENDING
	}

	return op
}

func isolationCheck(facts map[string]any) *dcctlv1.Check {
	return confirmCheck("coord.isolation", confIsolation, facts)
}

// refusal is the error of the API: the status with its ErrorInfo, the
// violations and the preflight detail.
func refusal(reason apierr.Reason, msg string, checks ...*dcctlv1.Check) error {
	pf := &dcctlv1.Preflight{Checks: append([]*dcctlv1.Check{testCheck("coord.lock_free", dcctlv1.Check_PASS, dcctlv1.Check_BLOCK, "free")}, checks...), Mode: dcctlv1.Operation_QUORUM}

	violations := make([]*errdetails.PreconditionFailure_Violation, 0, len(checks))

	for _, c := range checks {
		violations = append(violations, &errdetails.PreconditionFailure_Violation{Type: "confirmation", Subject: c.GetId(), Description: c.GetDetail()})
	}

	return apierr.Status(reason, msg, nil, &errdetails.PreconditionFailure{Violations: violations}, pf).Err()
}

// requestsOf returns the requests the API got for the RPC.
func requestsOf[T proto.Message](t *testing.T, e *env, method string) []T {
	t.Helper()

	out := make([]T, 0, 4)

	for _, r := range e.api.requests(method) {
		m, ok := r.(T)
		require.True(t, ok, "%T", r)

		out = append(out, m)
	}

	return out
}

func assertNoStart(t *testing.T, e *env, method string) {
	t.Helper()

	for _, r := range e.api.requests(method) {
		switch r := r.(type) {
		case *dcctlv1.SwitchDatacenterRequest:
			assert.True(t, r.GetValidateOnly(), "the action was started")
		case *dcctlv1.FailoverDatacenterRequest:
			assert.True(t, r.GetValidateOnly(), "the action was started")
		case *dcctlv1.DemoteDatacenterRequest:
			assert.True(t, r.GetValidateOnly(), "the action was started")
		case *dcctlv1.ResumeOperationRequest:
			assert.True(t, r.GetValidateOnly(), "the action was started")
		case *dcctlv1.AbandonOperationRequest:
			assert.True(t, r.GetValidateOnly(), "the action was started")
		}
	}
}
