package answer

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
)

// Check returns c as the API message. DiscardBytes has no field; the facts show the
// amount. It fails when the facts hold a value google.protobuf.Struct does not take.
func Check(c check.Check) (*dcctlv1.Check, error) {
	out := &dcctlv1.Check{
		Id:           string(c.ID),
		Result:       resultOf(c.Result),
		Effect:       effectOf(c.Effect),
		Confirmation: string(c.Confirmation),
		Datacenter:   datacenterName(c.Datacenter),
		Detail:       c.Detail,
	}

	if c.Facts != nil {
		facts, err := structpb.NewStruct(c.Facts)
		if err != nil {
			return nil, fmt.Errorf("facts of %s: %w", c.ID, err)
		}

		out.Facts = facts
	}

	return out, nil
}

// Checks returns cs as API messages, in their order.
func Checks(cs []check.Check) ([]*dcctlv1.Check, error) {
	out := make([]*dcctlv1.Check, len(cs))

	for i, c := range cs {
		m, err := Check(c)
		if err != nil {
			return nil, err
		}

		out[i] = m
	}

	return out, nil
}

// Confirmation returns the confirmations an operation records, nil when there are none.
func Confirmation(c check.Confirmations) *dcctlv1.Confirmation {
	if c == (check.Confirmations{}) {
		return nil
	}

	return &dcctlv1.Confirmation{Isolation: c.Isolation, NotMaxLsn: c.NotMaxLSN, MaxDiscardBytes: c.MaxDiscardBytes}
}

func resultOf(r check.Result) dcctlv1.Check_Result {
	switch r {
	case check.Pass:
		return dcctlv1.Check_PASS
	case check.Fail:
		return dcctlv1.Check_FAIL
	case check.Warn:
		return dcctlv1.Check_WARN
	case check.Skip:
		return dcctlv1.Check_SKIP
	}

	return dcctlv1.Check_RESULT_UNSPECIFIED
}

func effectOf(e check.Effect) dcctlv1.Check_Effect {
	switch e {
	case check.Block:
		return dcctlv1.Check_BLOCK
	case check.WarnOnly:
		return dcctlv1.Check_WARN_ONLY
	case check.Confirm:
		return dcctlv1.Check_CONFIRM
	case check.EffectNone:
	}

	return dcctlv1.Check_EFFECT_UNSPECIFIED
}

// datacenterName returns the resource name of dc, or "" for none.
func datacenterName(dc string) string {
	if dc == "" {
		return ""
	}

	return "datacenters/" + dc
}
