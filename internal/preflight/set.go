package preflight

import (
	"errors"
	"fmt"
	"slices"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// ErrUnknownAction is returned for an action without a set of checks.
var ErrUnknownAction = errors.New("unknown action")

// Checks evaluates the checks of in.Action, in the order of its set.
func Checks(in *Input) ([]check.Check, error) {
	dc := in.Params.Datacenter
	a := in.activeName()

	switch in.Action {
	case Switch:
		if in.warm() {
			return in.switchWarm(a, dc), nil
		}

		return in.switchStretch(a, dc), nil
	case Failover:
		if in.warm() {
			return in.failoverWarm(dc), nil
		}

		return in.failoverStretch(dc), nil
	case Demote:
		return concat(
			[]check.Check{in.profileSupported()},
			in.common(dc),
			[]check.Check{in.dcNotActive()},
			in.basicOf(dc), in.basicOf(a),
			[]check.Check{in.activePrimary(), in.dcPrimary(), in.divergence(), in.dcAppsStopped()},
		), nil
	case Repoint:
		return concat(
			[]check.Check{in.profileSupported()},
			in.common(""),
			[]check.Check{in.dcNotActive()},
			in.basicOf(dc), in.basicOf(a),
			[]check.Check{in.activePrimary(), in.dcStandby(), in.divergence()},
		), nil
	case MarkRecovered:
		return concat(in.common(""), []check.Check{
			in.quorum(check.Block), in.payloadSettled(), in.dcNeedsRecovery(), in.dcRecovered(),
		}), nil
	case Resume:
		cs := []check.Check{
			in.opInterrupted(), in.opStepsKnown(), in.opNotRolledBack(), in.lockFree(), in.leaderReachable(),
			in.noConflict(), in.quorum(check.WarnOnly),
		}

		// A failover not promoted yet decides its confirmations anew, on what the
		// datacenters show now.
		r, unpromoted := failoverRouteOf(in.operation())
		if in.Params.ToForced || unpromoted {
			cs = append(cs, in.isolation())
		}

		switch {
		case unpromoted && in.warm():
			cs = append(cs, in.maxLSN(r.target, r.others), in.sourceDiscard(r.source, r.target))
		case unpromoted:
			cs = append(cs, in.candidateLossOf(r.target))
		}

		return append(cs, in.recoveryResume(in.operation())...), nil
	case Abandon:
		return []check.Check{in.opInterrupted(), in.opGenerationDetermined(), in.pointOfNoReturn()}, nil
	case Resolve:
		return []check.Check{
			in.resolveConflict(), in.resolveWinner(), in.noOperation(), in.lockFree(), in.leaderReachable(),
			in.quorum(check.WarnOnly), in.isolation(),
		}, nil
	case Init:
		return []check.Check{in.initStateAbsent(), in.initAllReachable(), in.initActiveValid(), in.initActiveHasPrimary()}, nil
	case ArbiterInit:
		return []check.Check{
			in.noOperation(), in.initAllReachable(), in.arbiterReachable(), in.arbiterEmpty(), in.arbiterStatesEqual(),
		}, nil
	case AppsSync:
		return concat(in.common(""), []check.Check{
			in.activePrimary(), in.nomadReachable(a), in.stagesValid(a), in.activePlan(),
		}), nil
	case Verify:
		return concat(in.verifyPatroni(), in.verifyJobs(), []check.Check{in.verifyState()}), nil
	}

	return nil, fmt.Errorf("%w: %q", ErrUnknownAction, in.Action)
}

// recoveryResume returns what a resumed demote, repoint or mark-recovered op decides
// anew, on what the datacenters show now, before the step that acts on it: the WAL a
// demotion or a repoint that did not succeed discards, and the payload split the write
// of mark-recovered must not meet. A demotion that took already, its datacenter a
// standby of the active one, discards nothing more.
func (in *Input) recoveryResume(op *dcctlv1.Operation) []check.Check {
	target := datacenterID(op.GetTarget())

	//exhaustive:ignore // the other operations decide nothing more on resume
	switch op.GetType() {
	case dcctlv1.Operation_DEMOTE:
		if !Unfinished(op, dcctlv1.Operation_Step_PATRONI_DEMOTE) {
			return nil
		}

		if a := in.activeName(); a != "" && in.standbyOf(target, a) {
			return []check.Check{
				confirm(check.PatroniDivergence, check.ConfirmMaxDiscardBytes, target).skip(target + " is a standby of " + a + " already"),
			}
		}

		return []check.Check{in.divergenceOf(target)}
	case dcctlv1.Operation_REPOINT:
		if Unfinished(op, dcctlv1.Operation_Step_PATRONI_REPOINT) {
			return []check.Check{in.divergenceOf(target)}
		}
	case dcctlv1.Operation_MARK_RECOVERED:
		if Unfinished(op, dcctlv1.Operation_Step_GENERATION) {
			return []check.Check{in.payloadSettled()}
		}
	}

	return nil
}

// standbyOf reports whether the leader of dc is a standby leader of a.
func (in *Input) standbyOf(dc, a string) bool {
	s := in.pgDatacenter(dc)

	return s.src.Reachable && s.leader != nil && s.leader.Role == status.RoleStandbyLeader && s.model.Patroni.UpstreamDatacenter == a
}

// Unfinished reports whether op has a step of type typ that neither succeeded nor was
// skipped.
func Unfinished(op *dcctlv1.Operation, typ dcctlv1.Operation_Step_Type) bool {
	return slices.ContainsFunc(op.GetSteps(), func(st *dcctlv1.Operation_Step) bool {
		return st.GetType() == typ && st.GetState() != dcctlv1.Operation_Step_SUCCEEDED &&
			st.GetState() != dcctlv1.Operation_Step_SKIPPED
	})
}

func concat(sets ...[]check.Check) []check.Check {
	return slices.Concat(sets...)
}

// common is the set most actions start with; demote does not count the datacenter it
// demotes as split.
func (in *Input) common(except string) []check.Check {
	return []check.Check{
		in.statePresent(), in.noOperation(), in.lockFree(), in.leaderReachable(), in.noConflict(),
		in.noUncommitted(), in.noSplit(except),
	}
}

// basicOf is [Input.basic] for datacenter dc, which is empty when it is the active one
// and that is not known.
func (in *Input) basicOf(dc string) []check.Check {
	if dc != "" {
		return in.basic(in.pgDatacenter(dc))
	}

	ids := []check.ID{
		check.PatroniMembersFound, check.PatroniReachable, check.PatroniVersion, check.PatroniNotPaused,
		check.PatroniHasLeader,
	}

	out := make([]check.Check, len(ids))
	for i, id := range ids {
		out[i] = block(id, "").fail("the active datacenter is not known", nil)
	}

	return out
}

// apps is the Nomad part of a switch: the apps move from a to b.
func (in *Input) apps(a, b string) []check.Check {
	return []check.Check{
		in.nomadReachable(a), in.nomadReachable(b), in.stagesValid(a), in.stagesValid(b),
		in.targetJobsPresent(), in.targetJobsStopped(), in.targetPlan(), in.targetNodes(),
	}
}

func (in *Input) switchWarm(a, b string) []check.Check {
	return concat(
		in.common(""),
		[]check.Check{in.quorum(check.Block), in.targetValid(), in.targetNotNeedsRecovery()},
		in.basicOf(a), in.basicOf(b),
		[]check.Check{
			in.singlePrimary(), in.sourcePrimary(), in.targetStandbyLeader(), in.targetFollowsSource(), in.targetLag(),
		},
		in.othersFollowSource(),
		in.apps(a, b),
		[]check.Check{in.targetRabbitMQ(check.Block)},
	)
}

func (in *Input) switchStretch(a, b string) []check.Check {
	return concat(
		in.common(""),
		[]check.Check{in.quorum(check.Block), in.targetValid()},
		in.basic(in.pgCluster()),
		[]check.Check{in.targetCandidate()},
		in.apps(a, b),
		[]check.Check{in.targetRabbitMQ(check.Block)},
	)
}

// failoverApps is the Nomad part of a failover: only the target is asked, the old
// active datacenter may be gone.
func (in *Input) failoverApps(b string) []check.Check {
	return []check.Check{
		in.nomadReachable(b), in.stagesValid(b), in.targetPlan(), in.targetNodes(), in.targetRabbitMQ(check.WarnOnly),
	}
}

// failoverWarm takes the quorum as a warning: without a majority the generation is
// written forced, which the isolation confirmation allows.
func (in *Input) failoverWarm(b string) []check.Check {
	return concat(
		in.common(""),
		[]check.Check{in.quorum(check.WarnOnly), in.isolation(), in.targetValid(), in.targetNotNeedsRecovery()},
		in.basicOf(b),
		[]check.Check{in.targetStandbyLeader(), in.targetNotStreaming(), in.targetMaxLSN(), in.sourceDiscard(in.activeName(), b)},
		in.failoverApps(b),
	)
}

// failoverStretch needs the quorum: the minority side of a partition must not take
// over.
func (in *Input) failoverStretch(b string) []check.Check {
	return concat(
		in.common(""),
		[]check.Check{
			in.quorum(check.Block), in.isolation(), in.targetValid(), in.membersFound(in.pgDatacenter(b)),
			in.targetLeader(), in.candidateLoss(),
		},
		in.failoverApps(b),
	)
}

// planJobs returns the managed jobs to plan for the action by datacenter: every one of
// the target of a switch or failover, and the stopped ones of the active datacenter
// for apps sync. A datacenter whose Nomad does not answer has none; its checks fail.
func (in *Input) planJobs() map[string][]string {
	var (
		dc   string
		keep func(status.Job) bool
	)

	switch in.Action {
	case Switch, Failover:
		dc, keep = in.Params.Datacenter, func(status.Job) bool { return true }
	case AppsSync:
		dc, keep = in.activeName(), func(j status.Job) bool { return j.State == status.JobStopped }
	case Demote, Repoint, MarkRecovered, Resume, Abandon, Resolve, Init, ArbiterInit, Verify:
		return nil
	}

	n, _, ok := in.nomadOf(dc)
	if dc == "" || !ok {
		return nil
	}

	var jobs []string

	for _, j := range jobsOf(n) {
		if keep(j) {
			jobs = append(jobs, j.Name)
		}
	}

	if len(jobs) == 0 {
		return nil
	}

	return map[string][]string{dc: jobs}
}
