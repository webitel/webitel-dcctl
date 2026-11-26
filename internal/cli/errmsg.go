package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

// hinted is an error with advice the operator can act on.
type hinted struct {
	err  error
	hint string
}

func (h *hinted) Error() string { return h.err.Error() }
func (h *hinted) Unwrap() error { return h.err }

func withHint(err error, format string, args ...any) error {
	return &hinted{err: err, hint: fmt.Sprintf(format, args...)}
}

// statusOf returns the gRPC status in the chain of err.
func statusOf(err error) (*status.Status, bool) {
	var gs interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &gs) {
		return nil, false
	}

	return gs.GRPCStatus(), true
}

// detailsOf returns the preflight detail and the violations of a refusal.
func detailsOf(st *status.Status) (*dcctlv1.Preflight, []*errdetails.PreconditionFailure_Violation) {
	var (
		pf         *dcctlv1.Preflight
		violations []*errdetails.PreconditionFailure_Violation
	)

	for _, d := range st.Details() {
		switch d := d.(type) {
		case *dcctlv1.Preflight:
			pf = d
		case *errdetails.PreconditionFailure:
			violations = append(violations, d.GetViolations()...)
		}
	}

	return pf, violations
}

// plain removes the CLI layers from err: the exit code and the hints. What is
// left is the error of the call, or of a daemon command that has nothing to do
// with the API.
func plain(err error) ([]string, error) {
	var hints []string

	for {
		switch e := err.(type) {
		case *Error:
			if e.Err == nil {
				return hints, nil
			}

			err = e.Err
		case *hinted:
			hints = append(hints, e.hint)
			err = e.err
		default:
			return hints, err
		}
	}
}

// directStatus returns the status that err itself is. An error that wraps one,
// as "arbiter: ..." does, is told by its own text, which has the prefix.
func directStatus(err error) (*status.Status, bool) {
	gs, ok := err.(interface{ GRPCStatus() *status.Status })
	if !ok {
		return nil, false
	}

	return gs.GRPCStatus(), true
}

// describe returns the text the operator reads for err.
func describe(err error) string {
	hints, err := plain(err)
	if err == nil {
		return ""
	}

	var b strings.Builder

	if st, ok := directStatus(err); ok {
		describeStatus(&b, err, st)
	} else {
		b.WriteString(err.Error())
	}

	for _, h := range hints {
		b.WriteString("\n" + h)
	}

	return b.String()
}

func describeStatus(b *strings.Builder, err error, st *status.Status) {
	reason := apierr.ReasonOf(err)

	switch {
	case reason == apierr.ReasonAuthenticationRequired || st.Code() == codes.Unauthenticated:
		b.WriteString("authentication failed: the user or the password is not accepted by the server")

		return
	case reason == "" && st.Code() == codes.Unavailable:
		fmt.Fprintf(b, "the server is not reachable: %s", st.Message())

		return
	case reason == "" && st.Code() == codes.DeadlineExceeded:
		b.WriteString("the call did not finish in time; the outcome of an action is not known")

		return
	}

	b.WriteString(st.Message())

	if reason != "" {
		fmt.Fprintf(b, " [%s]", reason)
	}

	pf, violations := detailsOf(st)

	for _, v := range violations {
		fmt.Fprintf(b, "\n  %s %s: %s", v.GetType(), v.GetSubject(), v.GetDescription())
	}

	if reason == apierr.ReasonConfirmationRequired && pf != nil {
		for _, c := range pf.GetChecks() {
			flag := confirmationFlag(c.GetConfirmation())
			if flag == "" || c.GetEffect() != dcctlv1.Check_CONFIRM || c.GetResult() != dcctlv1.Check_FAIL {
				continue
			}

			fmt.Fprintf(b, "\n  needs %s: %s: %s", flag, c.GetId(), c.GetDetail())

			if facts, err := marshalJSON(c.GetFacts(), false); err == nil && c.GetFacts() != nil {
				fmt.Fprintf(b, "\n    facts: %s", facts)
			}
		}
	}

	md := apierr.InfoOf(err).GetMetadata()
	if op := md["operation"]; op != "" && reason == apierr.ReasonOperationRunning {
		fmt.Fprintf(b, "\nwatch it with: webitel-dcctl watch %s", strings.TrimPrefix(op, "operations/"))
	}

	if reason == apierr.ReasonRequestIDMismatch {
		b.WriteString("\nthe request id belongs to another request; use a new --request-id")
	}
}

func confirmationFlag(kind string) string {
	switch kind {
	case confIsolation:
		return "--confirm-isolation"
	case confNotMaxLSN:
		return "--confirm-not-max-lsn"
	case confMaxDiscard:
		return "--max-discard-bytes N"
	}

	return ""
}

// errorJSON returns err as the JSON object -o json prints on stderr.
func errorJSON(err error) string {
	hints, err := plain(err)
	if err == nil {
		return `{"error":{}}`
	}

	body := map[string]any{"message": err.Error()}

	if st, ok := directStatus(err); ok {
		body["message"] = st.Message()
		body["code"] = st.Code().String()

		if info := apierr.InfoOf(err); info != nil {
			body["reason"] = info.GetReason()

			if len(info.GetMetadata()) > 0 {
				body["metadata"] = info.GetMetadata()
			}
		}

		if pf, _ := detailsOf(st); pf != nil {
			if raw, mErr := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(pf); mErr == nil {
				body["preflight"] = json.RawMessage(raw)
			}
		}
	}

	if len(hints) > 0 {
		body["hints"] = hints
	}

	out, mErr := json.Marshal(map[string]any{"error": body})
	if mErr != nil {
		return `{"error":{"message":"the error cannot be encoded"}}`
	}

	return string(out)
}
