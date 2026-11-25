package answer

import (
	"fmt"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
)

// DryRun returns the answer to validate_only. An action v allows gets the operation it
// would start, without a name: all checks, plan in PENDING, mode, the warnings and the
// confirmations it would record. The caller sets the type, target and request. Any
// other verdict gets the error of [Refusal], the same as the request itself.
func DryRun(v check.Verdict, plan []*dcctlv1.Operation_Step, mode dcctlv1.Operation_Mode) (*dcctlv1.Operation, error) {
	if v.Outcome != check.Allowed {
		return nil, Refusal(v, plan, mode)
	}

	checks, err := Checks(v.Checks)
	if err != nil {
		return nil, fmt.Errorf("preflight answer: %w", err)
	}

	return &dcctlv1.Operation{
		Checks:       checks,
		Steps:        pending(plan),
		Mode:         mode,
		Warnings:     Warnings(v),
		Confirmation: Confirmation(v.Confirmed),
	}, nil
}

// Warnings returns a line for each warning of v: the warnings of the operation, which
// its start entry in the journal takes as they are.
func Warnings(v check.Verdict) []string {
	out := make([]string, 0, len(v.Warnings))

	for _, c := range v.Warnings {
		where := string(c.ID)
		if c.Datacenter != "" {
			where += " in " + c.Datacenter
		}

		out = append(out, where+": "+c.Detail)
	}

	return out
}
