package cli

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// Confirmations an action can need, as the checks name them.
const (
	confIsolation  = "isolation"
	confNotMaxLSN  = "not_max_lsn"
	confMaxDiscard = "max_discard_bytes"
)

// checkQuorum is the check that tells whether a majority of voters answers.
const checkQuorum = "coord.quorum"

// need is one confirmation an action asks for, with the checks that ask.
type need struct {
	kind   string
	checks []*dcctlv1.Check
	// amount is the most data the checks would discard, -1 when the checks do
	// not tell. It is set for max_discard_bytes only.
	amount int64
	// facts identifies what the operator is shown: when it changes, an answer
	// given to the earlier facts no longer counts.
	facts string
}

// discardFacts names the fact of a check that holds the bytes it discards.
// The API has no field for the amount yet.
var discardFacts = map[string]string{
	"patroni.divergence":     "divergence_bytes",
	"patroni.candidate_loss": "lag_bytes",
}

// needsOf returns the confirmations the checks ask for: the failed checks
// whose effect is CONFIRM. A confirmation this client does not know is an
// error, so the action does not go on with an answer the operator was not
// able to give.
//
// With resuming set, no quorum is a need for isolation as well: the resume
// goes on in forced mode only with it, so the client asks for it only when
// the checks show the quorum is lost.
func needsOf(checks []*dcctlv1.Check, resuming bool) ([]need, error) {
	byKind := make(map[string]*need)

	add := func(kind string, c *dcctlv1.Check) {
		n := byKind[kind]
		if n == nil {
			n = &need{kind: kind}
			byKind[kind] = n
		}

		n.checks = append(n.checks, c)
	}

	for _, c := range checks {
		if c.GetEffect() != dcctlv1.Check_CONFIRM || c.GetResult() != dcctlv1.Check_FAIL {
			continue
		}

		switch kind := c.GetConfirmation(); kind {
		case confIsolation, confNotMaxLSN, confMaxDiscard:
			add(kind, c)
		default:
			return nil, fmt.Errorf("the check %s asks for the confirmation %q, which this client does not know: "+
				"update webitel-dcctl", c.GetId(), kind)
		}
	}

	if resuming {
		for _, c := range checks {
			if c.GetId() == checkQuorum && (c.GetResult() == dcctlv1.Check_WARN || c.GetResult() == dcctlv1.Check_FAIL) {
				add(confIsolation, c)
			}
		}
	}

	needs := make([]need, 0, len(byKind))

	for _, n := range byKind {
		n.facts = factsOf(n.checks)
		n.amount = amountOf(n)
		needs = append(needs, *n)
	}

	slices.SortFunc(needs, func(a, b need) int { return cmp.Compare(a.kind, b.kind) })

	return needs, nil
}

// factsOf returns a string that is the same for the same checks, whatever
// their order.
func factsOf(checks []*dcctlv1.Check) string {
	parts := make([]string, len(checks))

	for i, c := range checks {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
		if err != nil {
			// A check that cannot be encoded cannot be told from another one; its
			// text stands for it.
			b = []byte(c.String())
		}

		parts[i] = string(b)
	}

	slices.Sort(parts)

	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))

	return hex.EncodeToString(sum[:])
}

// amountOf returns the largest amount of discarded data the checks of a
// max_discard_bytes need show, or -1 when one of them shows none.
func amountOf(n *need) int64 {
	if n.kind != confMaxDiscard {
		return -1
	}

	var most int64

	for _, c := range n.checks {
		fact := cmp.Or(discardFacts[c.GetId()], "discard_bytes")

		v, ok := c.GetFacts().AsMap()[fact].(float64)
		if !ok || v < 0 || v != math.Trunc(v) || v > math.MaxInt64/2 {
			return -1
		}

		most = max(most, int64(v))
	}

	return most
}

// decision is what the operator confirmed.
type decision struct {
	// facts are those the operator was shown when they answered.
	facts string
	// amount is the bound of max_discard_bytes.
	amount int64
	// flag is set when the answer is a flag of the command, which stays given
	// when the facts change: the operator sees the facts of the last dry-run
	// before they type the name.
	flag bool
}

// decisions are the confirmations given so far, by kind.
type decisions map[string]decision

// covers reports whether the decision for n still stands.
func (d decisions) covers(n need) bool {
	dec, ok := d[n.kind]
	if !ok {
		return false
	}

	if n.kind == confMaxDiscard {
		if n.amount < 0 {
			// The server compares the bound with an amount the client is not told.
			return dec.flag
		}

		return dec.amount >= n.amount && (dec.flag || dec.facts == n.facts)
	}

	return dec.flag || dec.facts == n.facts
}

// keep drops the decisions that no longer stand for the needs: those for
// something the action does not ask for, and those given to other facts.
func (d decisions) keep(needs []need) {
	for kind := range d {
		i := slices.IndexFunc(needs, func(n need) bool { return n.kind == kind })
		if i < 0 || !d.covers(needs[i]) {
			delete(d, kind)
		}
	}
}

// confirmation returns the confirmation to send: only what the operator
// confirmed.
func (d decisions) confirmation() *dcctlv1.Confirmation {
	if len(d) == 0 {
		return nil
	}

	c := &dcctlv1.Confirmation{}

	_, c.Isolation = d[confIsolation]
	_, c.NotMaxLsn = d[confNotMaxLSN]

	if dec, ok := d[confMaxDiscard]; ok {
		c.MaxDiscardBytes = dec.amount
	}

	return c
}

// unmet returns the needs no decision covers.
func (d decisions) unmet(needs []need) []need {
	var out []need

	for _, n := range needs {
		if !d.covers(n) {
			out = append(out, n)
		}
	}

	return out
}

// offer is what the flags of the command give: the answers the operator
// gave up front.
type offer struct {
	isolation bool
	notMaxLSN bool
	// discard is the bound of --max-discard-bytes, nil without the flag.
	discard *int64
}

// answer returns the decision the flags give for n, if they give one.
func (o offer) answer(n need) (decision, bool) {
	switch n.kind {
	case confIsolation:
		return decision{flag: true}, o.isolation
	case confNotMaxLSN:
		return decision{flag: true}, o.notMaxLSN
	case confMaxDiscard:
		if o.discard != nil && (n.amount < 0 || *o.discard >= n.amount) {
			return decision{flag: true, amount: *o.discard}, true
		}
	}

	return decision{}, false
}

// confirmation returns the confirmation of the flags as they are, for a call
// that skips the dry-run.
func (o offer) confirmation() *dcctlv1.Confirmation {
	if !o.isolation && !o.notMaxLSN && o.discard == nil {
		return nil
	}

	c := &dcctlv1.Confirmation{Isolation: o.isolation, NotMaxLsn: o.notMaxLSN}
	if o.discard != nil {
		c.MaxDiscardBytes = *o.discard
	}

	return c
}
