package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

func confirmCheck(id, kind string, facts map[string]any) *dcctlv1.Check {
	c := testCheck(id, dcctlv1.Check_FAIL, dcctlv1.Check_CONFIRM, id+" failed")
	c.Confirmation = kind

	if facts != nil {
		c.Facts = mustStruct(facts)
	}

	return c
}

func TestNeedsOf(t *testing.T) {
	isolation := confirmCheck("coord.isolation", confIsolation, map[string]any{"mode": "forced_only"})
	notMax := confirmCheck("patroni.target_max_lsn", confNotMaxLSN, nil)
	divergence := confirmCheck("patroni.divergence", confMaxDiscard, map[string]any{"divergence_bytes": 4096.0})
	loss := confirmCheck("patroni.candidate_loss", confMaxDiscard, map[string]any{"lag_bytes": 8192.0})
	passed := testCheck("coord.lock_free", dcctlv1.Check_PASS, dcctlv1.Check_BLOCK, "free")
	blocked := testCheck("patroni.reachable", dcctlv1.Check_FAIL, dcctlv1.Check_BLOCK, "down")
	coveredPass := testCheck("coord.isolation", dcctlv1.Check_PASS, dcctlv1.Check_CONFIRM, "ok")
	noQuorum := testCheck(checkQuorum, dcctlv1.Check_WARN, dcctlv1.Check_WARN_ONLY, "1 of 3 voters answer")
	quorum := testCheck(checkQuorum, dcctlv1.Check_PASS, dcctlv1.Check_WARN_ONLY, "3 of 3 voters answer")

	tests := []struct {
		name     string
		checks   []*dcctlv1.Check
		resuming bool
		want     map[string]int64 // kind -> amount
	}{
		{"nothing", []*dcctlv1.Check{passed, blocked, coveredPass}, false, map[string]int64{}},
		{"failover", []*dcctlv1.Check{passed, isolation, notMax}, false, map[string]int64{confIsolation: -1, confNotMaxLSN: -1}},
		{"discard is the largest amount", []*dcctlv1.Check{divergence, loss}, false, map[string]int64{confMaxDiscard: 8192}},
		{"no quorum asks for isolation on resume", []*dcctlv1.Check{passed, noQuorum}, true, map[string]int64{confIsolation: -1}},
		{"no quorum is not a need of another action", []*dcctlv1.Check{noQuorum}, false, map[string]int64{}},
		{"quorum asks for nothing on resume", []*dcctlv1.Check{quorum}, true, map[string]int64{}},
		{"isolation asked twice is one need", []*dcctlv1.Check{isolation, noQuorum}, true, map[string]int64{confIsolation: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			needs, err := needsOf(tt.checks, tt.resuming)
			require.NoError(t, err)

			got := make(map[string]int64)
			for _, n := range needs {
				got[n.kind] = n.amount
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUnknownConfirmationBlocks(t *testing.T) {
	for _, kind := range []string{"quantum_flux", "", "ISOLATION", "max_discard_bytes "} {
		_, err := needsOf([]*dcctlv1.Check{confirmCheck("x.new", kind, nil)}, false)

		require.Error(t, err, "%q", kind)
		assert.Contains(t, err.Error(), "x.new")
		assert.Contains(t, err.Error(), "does not know")
	}
}

func TestDiscardAmountIsOnlyAWholeNumber(t *testing.T) {
	for name, facts := range map[string]map[string]any{
		"missing":    {"other": 1.0},
		"no facts":   nil,
		"negative":   {"divergence_bytes": -1.0},
		"fractional": {"divergence_bytes": 1.5},
		"a string":   {"divergence_bytes": "10"},
		"huge":       {"divergence_bytes": 1e30},
	} {
		needs, err := needsOf([]*dcctlv1.Check{confirmCheck("patroni.divergence", confMaxDiscard, facts)}, false)

		require.NoError(t, err, name)
		require.Len(t, needs, 1, name)
		assert.EqualValues(t, -1, needs[0].amount, name)
	}
}

func TestFactsDoNotDependOnOrder(t *testing.T) {
	a := confirmCheck("patroni.divergence", confMaxDiscard, map[string]any{"divergence_bytes": 1.0})
	b := confirmCheck("patroni.candidate_loss", confMaxDiscard, map[string]any{"lag_bytes": 1.0})

	assert.Equal(t, factsOf([]*dcctlv1.Check{a, b}), factsOf([]*dcctlv1.Check{b, a}))

	c := confirmCheck("patroni.divergence", confMaxDiscard, map[string]any{"divergence_bytes": 2.0})

	assert.NotEqual(t, factsOf([]*dcctlv1.Check{a}), factsOf([]*dcctlv1.Check{c}))
}

func need1(kind string, amount int64, facts string) need {
	return need{kind: kind, amount: amount, facts: facts}
}

func TestDecisions(t *testing.T) {
	d := decisions{
		confIsolation:  {facts: "f1"},
		confMaxDiscard: {facts: "f2", amount: 100},
	}

	assert.True(t, d.covers(need1(confIsolation, -1, "f1")))
	assert.False(t, d.covers(need1(confIsolation, -1, "other facts")), "the facts changed")
	assert.False(t, d.covers(need1(confNotMaxLSN, -1, "f1")), "another kind")
	assert.True(t, d.covers(need1(confMaxDiscard, 100, "f2")))
	assert.False(t, d.covers(need1(confMaxDiscard, 101, "f2")), "more than the operator saw")
	assert.False(t, d.covers(need1(confMaxDiscard, 100, "f3")), "the facts changed")
	assert.False(t, d.covers(need1(confMaxDiscard, -1, "f2")), "an amount that is not known is not covered by an answer")

	flagged := decisions{confIsolation: {flag: true}, confMaxDiscard: {flag: true, amount: 100}}

	assert.True(t, flagged.covers(need1(confIsolation, -1, "any")))
	assert.True(t, flagged.covers(need1(confMaxDiscard, 50, "any")))
	assert.False(t, flagged.covers(need1(confMaxDiscard, 101, "any")), "a flag does not cover more than it says")
	assert.True(t, flagged.covers(need1(confMaxDiscard, -1, "any")), "the server compares a flag with the amount")
}

func TestDecisionsKeepOnlyWhatStands(t *testing.T) {
	d := decisions{
		confIsolation:  {facts: "old"},
		confNotMaxLSN:  {facts: "same"},
		confMaxDiscard: {flag: true, amount: 10},
	}

	d.keep([]need{need1(confIsolation, -1, "new"), need1(confNotMaxLSN, -1, "same")})

	assert.Equal(t, decisions{confNotMaxLSN: {facts: "same"}}, d, "isolation was given to other facts, the bound is not asked for")
}

func TestConfirmationSendsOnlyWhatWasDecided(t *testing.T) {
	assert.Nil(t, decisions{}.confirmation())

	c := decisions{confNotMaxLSN: {}}.confirmation()
	assert.True(t, c.GetNotMaxLsn())
	assert.False(t, c.GetIsolation(), "isolation is not sent unless it was confirmed")
	assert.Zero(t, c.GetMaxDiscardBytes())

	c = decisions{confIsolation: {}, confMaxDiscard: {amount: 77}}.confirmation()
	assert.True(t, c.GetIsolation())
	assert.EqualValues(t, 77, c.GetMaxDiscardBytes())
}

func TestOfferAnswers(t *testing.T) {
	bound := int64(500)
	o := offer{isolation: true, discard: &bound}

	_, ok, err := o.answer(need1(confIsolation, -1, "f"))
	require.NoError(t, err)
	assert.True(t, ok)

	_, ok, err = o.answer(need1(confNotMaxLSN, -1, "f"))
	require.NoError(t, err)
	assert.False(t, ok, "a flag that was not given answers nothing")

	// The flag answers; what is confirmed is the amount that was shown.
	dec, ok, err := o.answer(need1(confMaxDiscard, 300, "f"))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 300, dec.amount, "not the bound of the flag")

	dec, ok, err = o.answer(need1(confMaxDiscard, 500, "f"))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 500, dec.amount)

	_, ok, err = o.answer(need1(confMaxDiscard, 501, "f"))
	require.Error(t, err, "the bound is below what the checks show")
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "needs --max-discard-bytes >= 501")

	dec, ok, err = o.answer(need1(confMaxDiscard, -1, "f"))
	require.NoError(t, err)
	assert.True(t, ok, "an amount the server does not tell is compared by the server")
	assert.EqualValues(t, 500, dec.amount)

	assert.Nil(t, offer{}.confirmation())
	assert.EqualValues(t, 500, o.confirmation().GetMaxDiscardBytes(), "--yes sends the bound as it is")
	assert.True(t, o.confirmation().GetIsolation())
}
