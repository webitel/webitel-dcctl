package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/apierr"
)

func succeedingWatch(e *env) {
	e.api.watch = stream(false,
		opAt(dcctlv1.Operation_RUNNING, dcctlv1.Operation_Step_RUNNING),
		opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED),
	)
}

func TestSwitchFlow(t *testing.T) {
	e := newEnv(t)
	e.stdin = "dc_b\n"
	succeedingWatch(e)
	e.script(answer(planOf()), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res := e.run("switch", "dc_b")

	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "switch_flow", res.stdout)
	assert.Contains(t, res.stderr, `Type "dc_b" to continue`)
	assert.Equal(t, []string{rpcSwitch, rpcSwitch, methodWatch}, e.api.methods())

	reqs := requestsOf[*dcctlv1.SwitchDatacenterRequest](t, e, rpcSwitch)
	require.Len(t, reqs, 2)
	assert.True(t, reqs[0].GetValidateOnly())
	assert.False(t, reqs[1].GetValidateOnly())
	assert.Equal(t, "datacenters/dc_b", reqs[1].GetName())
	assert.Regexp(t, uuid4, reqs[1].GetRequestId())
	assert.Equal(t, reqs[0].GetRequestId(), reqs[1].GetRequestId(), "one request ID for the invocation")
	assert.Nil(t, reqs[1].GetConfirmation())
}

func TestTypedNameGuard(t *testing.T) {
	for name, stdin := range map[string]string{
		"another datacenter": "dc_a\n",
		"another case":       "DC_B\n",
		"empty line":         "\n",
		"no answer":          "",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.stdin = stdin
			e.script(answer(planOf()))

			res := e.run("switch", "dc_b")

			assert.Equal(t, ExitFailed, res.code)
			assertNoStart(t, e, rpcSwitch)
			assert.Len(t, e.api.requests(rpcSwitch), 1, "only the dry-run")
		})
	}
}

func TestYesSkipsTheDryRun(t *testing.T) {
	e := newEnv(t)
	succeedingWatch(e)
	e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res := e.run("switch", "dc_b", "--yes")

	require.Equal(t, ExitOK, res.code, res.stderr)

	reqs := requestsOf[*dcctlv1.SwitchDatacenterRequest](t, e, rpcSwitch)
	require.Len(t, reqs, 1)
	assert.False(t, reqs[0].GetValidateOnly())
	assert.NotContains(t, res.stderr, "Type")
}

func TestYesSendsOnlyTheFlags(t *testing.T) {
	e := newEnv(t)
	succeedingWatch(e)
	e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res := e.run("failover", "dc_b", "--yes", "--confirm-isolation", "--max-discard-bytes", "42")

	require.Equal(t, ExitOK, res.code, res.stderr)

	reqs := requestsOf[*dcctlv1.FailoverDatacenterRequest](t, e, rpcFailover)
	require.Len(t, reqs, 1)
	assert.True(t, reqs[0].GetConfirmation().GetIsolation())
	assert.False(t, reqs[0].GetConfirmation().GetNotMaxLsn())
	assert.EqualValues(t, 42, reqs[0].GetConfirmation().GetMaxDiscardBytes())
}

func TestYesFailsWithTheFactsOfARefusal(t *testing.T) {
	e := newEnv(t)
	e.script(failWith(refusal(apierr.ReasonConfirmationRequired, "confirmation required",
		isolationCheck(map[string]any{"mode": "forced_only"}))))

	res := e.run("failover", "dc_b", "--yes")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "needs --confirm-isolation: coord.isolation")
	assert.Contains(t, res.stderr, `"mode":"forced_only"`)
	assert.Len(t, e.api.requests(rpcFailover), 1, "no second call")
}

func TestDryRun(t *testing.T) {
	e := newEnv(t)
	e.script(answer(planOf()))

	res := e.run("switch", "dc_b", "--dry-run")

	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stdout, "Dry run, nothing was changed")
	assert.Contains(t, res.stdout, "coord.lock_free")

	reqs := requestsOf[*dcctlv1.SwitchDatacenterRequest](t, e, rpcSwitch)
	require.Len(t, reqs, 1)
	assert.True(t, reqs[0].GetValidateOnly())

	e = newEnv(t)
	e.script(answer(planOf()))

	res = e.run("switch", "dc_b", "--dry-run", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "switch_dry_run_json", res.stdout)
}

func TestDryRunRefused(t *testing.T) {
	e := newEnv(t)
	e.script(failWith(refusal(apierr.ReasonPreflightFailed, "preflight failed: patroni.target_lag",
		testCheck("patroni.target_lag", dcctlv1.Check_FAIL, dcctlv1.Check_BLOCK, "dc_b lags 52428800 bytes"))))

	res := e.run("switch", "dc_b", "--dry-run")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "preflight failed [PREFLIGHT_FAILED]"[:16])
	assert.Contains(t, res.stderr, "patroni.target_lag")
}

func TestDryRunNeedsFlagsForConfirmations(t *testing.T) {
	e := newEnv(t)
	e.script(failWith(refusal(apierr.ReasonConfirmationRequired, "confirmation required", isolationCheck(nil))))

	res := e.run("failover", "dc_b", "--dry-run")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stdout, "coord.isolation")
	assert.Contains(t, res.stderr, "--confirm-isolation")

	e = newEnv(t)
	e.script(
		failWith(refusal(apierr.ReasonConfirmationRequired, "confirmation required", isolationCheck(nil))),
		answer(planOf(isolationCheck(nil))),
	)

	res = e.run("failover", "dc_b", "--dry-run", "--confirm-isolation")

	assert.Equal(t, ExitOK, res.code, res.stderr)

	reqs := requestsOf[*dcctlv1.FailoverDatacenterRequest](t, e, rpcFailover)
	require.Len(t, reqs, 2)
	assert.True(t, reqs[1].GetConfirmation().GetIsolation())
	assertNoStart(t, e, rpcFailover)
}

func TestDetach(t *testing.T) {
	e := newEnv(t)
	e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res := e.run("switch", "dc_b", "--yes", "--detach")

	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stdout, "operations/op7")
	assert.Contains(t, res.stdout, "webitel-dcctl watch op7")
	assert.Equal(t, []string{rpcSwitch}, e.api.methods(), "the operation is not watched")

	e = newEnv(t)
	e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res = e.run("switch", "dc_b", "--yes", "--detach", "-o", "json")
	require.Equal(t, ExitOK, res.code, res.stderr)
	golden(t, "switch_detach_json", res.stdout)
}

func TestFailoverAsksForConfirmations(t *testing.T) {
	asked := refusal(apierr.ReasonConfirmationRequired, "confirmation required",
		isolationCheck(map[string]any{"mode": "forced_only"}),
		confirmCheck("patroni.target_max_lsn", confNotMaxLSN, nil))
	covered := planOf(isolationCheck(map[string]any{"mode": "forced_only"}), confirmCheck("patroni.target_max_lsn", confNotMaxLSN, nil))

	e := newEnv(t)
	e.stdin = "y\nyes\ndc_b\n"
	succeedingWatch(e)
	e.script(failWith(asked), answer(covered), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res := e.run("failover", "dc_b")

	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Contains(t, res.stderr, "Confirm that the old primary is isolated? [y/N]")
	assert.Contains(t, res.stderr, `"mode":"forced_only"`)

	reqs := requestsOf[*dcctlv1.FailoverDatacenterRequest](t, e, rpcFailover)
	require.Len(t, reqs, 3)
	assert.Nil(t, reqs[0].GetConfirmation())

	for _, r := range reqs[1:] {
		assert.True(t, r.GetConfirmation().GetIsolation())
		assert.True(t, r.GetConfirmation().GetNotMaxLsn())
		assert.Zero(t, r.GetConfirmation().GetMaxDiscardBytes())
	}

	// A refused question starts nothing.
	e = newEnv(t)
	e.stdin = "n\n"
	e.script(failWith(asked))

	res = e.run("failover", "dc_b")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "not confirmed")
	assertNoStart(t, e, rpcFailover)
}

// An answer given to some facts does not stand for other facts.
func TestChangedFactsAreAskedAgain(t *testing.T) {
	first := refusal(apierr.ReasonConfirmationRequired, "confirmation required", isolationCheck(map[string]any{"voters": "a"}))
	second := planOf(isolationCheck(map[string]any{"voters": "b"}))

	e := newEnv(t)
	e.stdin = "y\ny\ndc_b\n"
	succeedingWatch(e)
	e.script(failWith(first), answer(second), answer(second), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

	res := e.run("failover", "dc_b")

	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Equal(t, 2, strings.Count(res.stderr, "Confirm that the old primary is isolated?"), "asked again for the new facts")
	assert.Contains(t, res.stderr, `"voters":"b"`)
}

// The facts change between the check and the start: the operator sees the
// new facts and types the name again.
func TestFactsChangedUnderTheTypedName(t *testing.T) {
	a := isolationCheck(map[string]any{"voters": "a"})
	b := isolationCheck(map[string]any{"voters": "b"})

	e := newEnv(t)
	e.stdin = "y\ndc_b\ny\ndc_b\n"
	succeedingWatch(e)
	e.script(
		failWith(refusal(apierr.ReasonConfirmationRequired, "x", a)), // dry-run 1
		answer(planOf(a)), // dry-run 2, with the isolation
		failWith(refusal(apierr.ReasonConfirmationRequired, "facts changed", b)), // the start
		answer(planOf(b)), // dry-run 3: the isolation of the operator is for facts a
		answer(planOf(b)), // dry-run 4
		answer(testOperation("op7", dcctlv1.Operation_RUNNING)),
	)

	res := e.run("failover", "dc_b")

	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.Equal(t, 2, strings.Count(res.stderr, `Type "dc_b" to continue`))
	assert.Contains(t, res.stderr, "checking again")
}

func TestDiscardBytes(t *testing.T) {
	divergence := func(n float64) *dcctlv1.Check {
		return confirmCheck("patroni.divergence", confMaxDiscard, map[string]any{"divergence_bytes": n})
	}

	t.Run("the amount the operator saw", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "y\ndc_a\n"
		succeedingWatch(e)
		e.script(
			failWith(refusal(apierr.ReasonConfirmationRequired, "x", divergence(4096))),
			answer(planOf(divergence(4096))),
			answer(testOperation("op7", dcctlv1.Operation_RUNNING)),
		)

		res := e.run("demote", "dc_a")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.Contains(t, res.stderr, "Accept discarding up to 4096 bytes of data? [y/N]")

		reqs := requestsOf[*dcctlv1.DemoteDatacenterRequest](t, e, rpcDemote)
		assert.EqualValues(t, 4096, reqs[len(reqs)-1].GetConfirmation().GetMaxDiscardBytes())
	})

	// The flag answers the question; the amount that was shown is what is
	// confirmed, however large the bound is.
	t.Run("a flag above the amount is not sent", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "dc_a\n"
		succeedingWatch(e)
		e.script(
			failWith(refusal(apierr.ReasonConfirmationRequired, "x", divergence(4096))),
			answer(planOf(divergence(4096))),
			answer(testOperation("op7", dcctlv1.Operation_RUNNING)),
		)

		res := e.run("demote", "dc_a", "--max-discard-bytes", "1000000000")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.NotContains(t, res.stderr, "Accept discarding")
		assert.Contains(t, res.stdout, `"divergence_bytes":4096`, "the facts are shown to the operator who gave the flag")

		for _, r := range requestsOf[*dcctlv1.DemoteDatacenterRequest](t, e, rpcDemote) {
			assert.LessOrEqual(t, r.GetConfirmation().GetMaxDiscardBytes(), int64(4096))
		}
	})

	t.Run("a flag below the amount is an error", func(t *testing.T) {
		e := newEnv(t)
		e.script(failWith(refusal(apierr.ReasonConfirmationRequired, "x", divergence(4096))))

		res := e.run("demote", "dc_a", "--max-discard-bytes", "10")

		assert.Equal(t, ExitFailed, res.code)
		assert.Contains(t, res.stderr, "needs --max-discard-bytes >= 4096")
		assert.NotContains(t, res.stderr, "[y/N]", "the operator is not asked")
		assertNoStart(t, e, rpcDemote)
	})

	t.Run("with --yes the flag is sent as it is", func(t *testing.T) {
		e := newEnv(t)
		succeedingWatch(e)
		e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

		res := e.run("demote", "dc_a", "--yes", "--max-discard-bytes", "1000000000")

		require.Equal(t, ExitOK, res.code, res.stderr)

		reqs := requestsOf[*dcctlv1.DemoteDatacenterRequest](t, e, rpcDemote)
		require.Len(t, reqs, 1)
		assert.EqualValues(t, 1000000000, reqs[0].GetConfirmation().GetMaxDiscardBytes())
	})

	t.Run("more than the flag covers", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "dc_a\n"
		succeedingWatch(e)
		e.script(
			failWith(refusal(apierr.ReasonConfirmationRequired, "x", divergence(4096))),
			answer(planOf(divergence(4096))),
			failWith(refusal(apierr.ReasonConfirmationRequired, "x", divergence(20000))),
			failWith(refusal(apierr.ReasonConfirmationRequired, "x", divergence(20000))),
		)

		res := e.run("demote", "dc_a", "--max-discard-bytes", "5000")

		assert.Equal(t, ExitFailed, res.code)
		assert.Contains(t, res.stderr, "needs --max-discard-bytes >= 20000")
	})

	t.Run("an amount the server does not tell", func(t *testing.T) {
		e := newEnv(t)
		e.script(failWith(refusal(apierr.ReasonConfirmationRequired, "x",
			confirmCheck("patroni.divergence", confMaxDiscard, nil))))

		res := e.run("demote", "dc_a")

		assert.Equal(t, ExitFailed, res.code)
		assert.Contains(t, res.stderr, "does not say how much")
		assertNoStart(t, e, rpcDemote)
	})
}

func TestUnknownConfirmationBlocksTheAction(t *testing.T) {
	e := newEnv(t)
	e.stdin = "y\ndc_b\n"
	e.script(failWith(refusal(apierr.ReasonConfirmationRequired, "x", confirmCheck("x.new", "teleport", nil))))

	res := e.run("failover", "dc_b", "--confirm-isolation")

	assert.Equal(t, ExitFailed, res.code)
	assert.Contains(t, res.stderr, "does not know")
	assertNoStart(t, e, rpcFailover)
}

func interrupted() *dcctlv1.Operation {
	op := testOperation("op7", dcctlv1.Operation_INTERRUPTED)
	op.Target = "datacenters/dc_b"

	return op
}

func TestResumeSendsIsolationOnlyWithoutQuorum(t *testing.T) {
	noQuorum := testCheck(checkQuorum, dcctlv1.Check_WARN, dcctlv1.Check_WARN_ONLY, "1 of 3 voters answer")
	quorum := testCheck(checkQuorum, dcctlv1.Check_PASS, dcctlv1.Check_WARN_ONLY, "3 of 3 voters answer")

	t.Run("quorum: a flag is not sent", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "dc_b\n"
		succeedingWatch(e)
		e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }
		e.script(answer(planOf(quorum)), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

		res := e.run("resume", "op7", "--confirm-isolation")

		require.Equal(t, ExitOK, res.code, res.stderr)

		for _, r := range requestsOf[*dcctlv1.ResumeOperationRequest](t, e, rpcResume) {
			assert.Nil(t, r.GetConfirmation(), "isolation would put the resume in forced mode")
		}
	})

	t.Run("no quorum: the operator is asked", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "y\ndc_b\n"
		succeedingWatch(e)
		e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }
		e.script(
			answer(planOf(noQuorum)),
			answer(planOf(noQuorum, isolationCheck(map[string]any{"mode": "forced_only"}))),
			answer(planOf(noQuorum, isolationCheck(map[string]any{"mode": "forced_only"}))),
			answer(testOperation("op7", dcctlv1.Operation_RUNNING)),
		)

		res := e.run("resume", "op7")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.Contains(t, res.stderr, "Confirm that the old primary is isolated?")

		reqs := requestsOf[*dcctlv1.ResumeOperationRequest](t, e, rpcResume)
		assert.Nil(t, reqs[0].GetConfirmation(), "the first check is without isolation")
		assert.True(t, reqs[len(reqs)-1].GetConfirmation().GetIsolation())
		assert.False(t, reqs[len(reqs)-1].GetValidateOnly())
	})

	t.Run("no quorum: the flag answers", func(t *testing.T) {
		e := newEnv(t)
		e.stdin = "dc_b\n"
		succeedingWatch(e)
		e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }
		e.script(
			answer(planOf(noQuorum)),
			answer(planOf(noQuorum, isolationCheck(nil))),
			answer(planOf(noQuorum, isolationCheck(nil))),
			answer(testOperation("op7", dcctlv1.Operation_RUNNING)),
		)

		res := e.run("resume", "op7", "--confirm-isolation")

		require.Equal(t, ExitOK, res.code, res.stderr)
		assert.NotContains(t, res.stderr, "Confirm that")
	})
}

func TestResumeEnds(t *testing.T) {
	tests := []struct {
		name string
		last *dcctlv1.Operation
		code int
	}{
		{"interrupted again", opAt(dcctlv1.Operation_INTERRUPTED), ExitInterrupted},
		{"failed", opAt(dcctlv1.Operation_FAILED), ExitFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.api.watch = stream(tt.code == ExitInterrupted, opAt(dcctlv1.Operation_RUNNING), tt.last)
			e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }
			e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

			assert.Equal(t, tt.code, e.run("resume", "op7", "--yes").code)
		})
	}
}

func TestAbandon(t *testing.T) {
	e := newEnv(t)
	e.stdin = "dc_b\n"
	e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }
	e.script(answer(planOf()), answer(testOperation("op7", dcctlv1.Operation_ABANDONED)))

	res := e.run("abandon", "operations/op7")

	require.Equal(t, ExitOK, res.code, res.stderr)
	assert.NotContains(t, e.api.methods(), methodWatch)

	reqs := requestsOf[*dcctlv1.AbandonOperationRequest](t, e, rpcAbandon)
	require.Len(t, reqs, 2)
	assert.Equal(t, "operations/op7", reqs[1].GetName())
}

func TestStartIsRepeatedWithTheSameRequestID(t *testing.T) {
	unavailable := failWith(apierr.New(apierr.ReasonLeaderUnavailable, "the leading instance did not answer in time"))

	t.Run("it works the second time", func(t *testing.T) {
		e := newEnv(t)
		succeedingWatch(e)
		e.script(unavailable, answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

		res := e.run("switch", "dc_b", "--yes")

		require.Equal(t, ExitOK, res.code, res.stderr)

		reqs := requestsOf[*dcctlv1.SwitchDatacenterRequest](t, e, rpcSwitch)
		require.Len(t, reqs, 2)
		assert.Equal(t, reqs[0].GetRequestId(), reqs[1].GetRequestId())
	})

	t.Run("it does not", func(t *testing.T) {
		e := newEnv(t)
		e.script(unavailable)

		res := e.run("switch", "dc_b", "--yes", "--request-id", "6F1C2B9E-8D4A-4F3B-9C2E-1A7D5E3F9B40")

		assert.Equal(t, ExitFailed, res.code)
		assert.Len(t, e.api.requests(rpcSwitch), maxStartAttempts)
		assert.Contains(t, res.stderr, "--request-id 6f1c2b9e-8d4a-4f3b-9c2e-1a7d5e3f9b40")
	})

	t.Run("an action without a request ID is not repeated", func(t *testing.T) {
		e := newEnv(t)
		e.api.getOperation = func(*dcctlv1.GetOperationRequest) (*dcctlv1.Operation, error) { return interrupted(), nil }
		e.script(failWith(status.Error(codes.Unavailable, "gone")))

		res := e.run("resume", "op7", "--yes")

		assert.Equal(t, ExitFailed, res.code)
		assert.Len(t, e.api.requests(rpcResume), 1)
		assert.Contains(t, res.stderr, "webitel-dcctl ops")
	})

	t.Run("a refusal is not repeated", func(t *testing.T) {
		e := newEnv(t)
		e.script(failWith(apierr.New(apierr.ReasonLockBusy, "busy")))

		assert.Equal(t, ExitFailed, e.run("switch", "dc_b", "--yes").code)
		assert.Len(t, e.api.requests(rpcSwitch), 1)
	})
}

func TestRequestIDFlag(t *testing.T) {
	e := newEnv(t)

	res := e.run("switch", "dc_b", "--yes", "--request-id", "not-a-uuid")

	assert.Equal(t, ExitUsage, res.code)
	assert.Empty(t, e.api.methods())
}

func TestFinalStatesOfAnAction(t *testing.T) {
	withWarning := opAt(dcctlv1.Operation_SUCCEEDED, dcctlv1.Operation_Step_SUCCEEDED)
	withWarning.Warnings = []string{"dc_c was not repointed"}

	failed := opAt(dcctlv1.Operation_FAILED, dcctlv1.Operation_Step_FAILED)
	failed.Error = &dcctlv1.OperationError{Reason: "STEP_FAILED", Detail: "no answer"}

	tests := []struct {
		name string
		last *dcctlv1.Operation
		code int
	}{
		{"succeeded", opAt(dcctlv1.Operation_SUCCEEDED), ExitOK},
		{"warnings", withWarning, ExitWarnings},
		{"failed", failed, ExitFailed},
		{"interrupted", opAt(dcctlv1.Operation_INTERRUPTED), ExitInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.api.watch = stream(tt.code == ExitInterrupted, opAt(dcctlv1.Operation_RUNNING), tt.last)
			e.script(answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

			assert.Equal(t, tt.code, e.run("switch", "dc_b", "--yes").code)
		})
	}

	// A repeated request ID returns an operation that is final already.
	e := newEnv(t)
	e.script(answer(testOperation("op7", dcctlv1.Operation_SUCCEEDED)))

	res := e.run("switch", "dc_b", "--yes")
	assert.Equal(t, ExitOK, res.code, res.stderr)
	assert.NotContains(t, e.api.methods(), methodWatch)
}

// Every refusal of the API ends the command with the same code; the message
// tells which one it is by the reason.
func TestRefusalsOfActions(t *testing.T) {
	for _, r := range apierr.Reasons() {
		t.Run(string(r), func(t *testing.T) {
			e := newEnv(t)
			e.script(failWith(apierr.Status(r, "refused", map[string]string{"operation": "operations/op9"}).Err()))

			res := e.run("switch", "dc_b", "--yes")

			assert.Equal(t, ExitFailed, res.code)
			assert.NotEqual(t, ExitInterrupted, res.code)

			if r != apierr.ReasonAuthenticationRequired {
				assert.Contains(t, res.stderr, "["+string(r)+"]")
			}
		})
	}

	e := newEnv(t)
	e.script(failWith(apierr.Status(apierr.ReasonOperationRunning, "an operation is running",
		map[string]string{"operation": "operations/op9"}).Err()))

	res := e.run("switch", "dc_b", "--yes")
	assert.Contains(t, res.stderr, "webitel-dcctl watch op9")
}

func TestOtherActions(t *testing.T) {
	tests := []struct {
		args   []string
		method string
		stdin  string
		check  func(t *testing.T, e *env)
	}{
		{[]string{"repoint", "dc_a"}, "/webitel.dcctl.v1.DatacenterService/RepointDatacenter", "dc_a\n", func(t *testing.T, e *env) {
			t.Helper()

			r := requestsOf[*dcctlv1.RepointDatacenterRequest](t, e, "/webitel.dcctl.v1.DatacenterService/RepointDatacenter")
			assert.Equal(t, "datacenters/dc_a", r[len(r)-1].GetName())
		}},
		{[]string{"mark-recovered", "dc_a"}, "/webitel.dcctl.v1.DatacenterService/MarkDatacenterRecovered", "dc_a\n", func(t *testing.T, e *env) {
			t.Helper()

			r := requestsOf[*dcctlv1.MarkDatacenterRecoveredRequest](t, e, "/webitel.dcctl.v1.DatacenterService/MarkDatacenterRecovered")
			assert.Equal(t, "datacenters/dc_a", r[len(r)-1].GetName())
		}},
		{[]string{"resolve", "dc_a"}, "/webitel.dcctl.v1.DatacenterService/ResolveConflict", "dc_a\n", func(t *testing.T, e *env) {
			t.Helper()

			r := requestsOf[*dcctlv1.ResolveConflictRequest](t, e, "/webitel.dcctl.v1.DatacenterService/ResolveConflict")
			assert.Equal(t, "datacenters/dc_a", r[len(r)-1].GetWinner())
		}},
		{[]string{"init", "--active", "dc_a"}, "/webitel.dcctl.v1.DatacenterService/InitializeDatacenters", "dc_a\n", func(t *testing.T, e *env) {
			t.Helper()

			r := requestsOf[*dcctlv1.InitializeDatacentersRequest](t, e, "/webitel.dcctl.v1.DatacenterService/InitializeDatacenters")
			assert.Equal(t, "datacenters/dc_a", r[len(r)-1].GetActiveDatacenter())
		}},
		{[]string{"init-arbiter"}, "/webitel.dcctl.v1.DatacenterService/InitializeArbiter", "arbiter\n", func(t *testing.T, e *env) {
			t.Helper()

			r := requestsOf[*dcctlv1.InitializeArbiterRequest](t, e, "/webitel.dcctl.v1.DatacenterService/InitializeArbiter")
			assert.Regexp(t, uuid4, r[len(r)-1].GetRequestId())
		}},
		{[]string{"apps", "sync"}, "/webitel.dcctl.v1.DatacenterService/SyncApps", "dc_b\n", func(t *testing.T, e *env) {
			t.Helper()

			r := requestsOf[*dcctlv1.SyncAppsRequest](t, e, "/webitel.dcctl.v1.DatacenterService/SyncApps")
			assert.False(t, r[len(r)-1].GetValidateOnly())
		}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			e := newEnv(t)
			e.stdin = tt.stdin
			succeedingWatch(e)
			e.script(answer(planOf()), answer(testOperation("op7", dcctlv1.Operation_RUNNING)))

			res := e.run(tt.args...)

			require.Equal(t, ExitOK, res.code, res.stderr)
			assert.Len(t, e.api.requests(tt.method), 2, "a dry-run and the start")
			tt.check(t, e)

			// --dry-run changes nothing.
			e = newEnv(t)
			e.script(answer(planOf()))

			res = e.run(append(tt.args, "--dry-run")...)
			require.Equal(t, ExitOK, res.code, res.stderr)
			assertNoStart(t, e, tt.method)
		})
	}
}

func TestActionUsage(t *testing.T) {
	e := newEnv(t)

	for _, args := range [][]string{
		{"switch"},
		{"switch", "a", "b"},
		{"switch", "bad name"},
		{"switch", "dc_b", "--confirm-isolation"},
		{"switch", "dc_b", "--nope"},
		{"failover", "dc_b", "--max-discard-bytes", "-1"},
		{"failover", "dc_b", "--max-discard-bytes", "many"},
		{"demote", "dc_a", "--confirm-not-max-lsn"},
		{"resume"},
		{"resume", "a b"},
		{"abandon", "a", "b"},
		{"resolve"},
		{"init"},
		{"init", "--active", "bad name"},
		{"init", "dc_a"},
		{"init-arbiter", "x"},
		{"apps"},
		{"apps", "nope"},
		{"apps", "sync", "x"},
	} {
		res := e.run(args...)

		assert.Equal(t, ExitUsage, res.code, "%v: %s", args, res.stderr)
	}

	assert.Empty(t, e.api.methods())
}
