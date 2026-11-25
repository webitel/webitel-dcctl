package preflight

import (
	"fmt"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// verifyPatroni has a result for each datacenter in warm_standby: the primary in the
// active one, a standby leader streaming from it on its timeline in the others. In
// stretch the leader runs in the active datacenter. A datacenter that does not answer
// is a warning.
func (in *Input) verifyPatroni() []check.Check {
	a, ok := in.active()
	if !ok {
		return []check.Check{info(check.VerifyPatroni, "").fail("the active datacenter is not known", nil)}
	}

	if !in.warm() {
		c := info(check.VerifyPatroni, "")
		s := in.pgCluster()

		switch {
		case !s.src.Reachable:
			return []check.Check{c.warn("Patroni does not answer: "+string(s.src.Reason), nil)}
		case s.leader == nil || s.leader.Datacenter != a:
			return []check.Check{c.fail("the leader does not run in "+a, nil)}
		}

		return []check.Check{c.pass("the leader runs in "+a, map[string]any{"leader": memberFacts(*s.leader)})}
	}

	out := make([]check.Check, 0, len(in.Status.Datacenters))

	for _, d := range in.Status.Datacenters {
		c := info(check.VerifyPatroni, d.Name)
		s := in.pgDatacenter(d.Name)

		if !s.src.Reachable {
			out = append(out, c.warn("Patroni of "+d.Name+" does not answer: "+string(s.src.Reason), nil))

			continue
		}

		if d.Name == a {
			if s.leader == nil || s.leader.Role != status.RolePrimary {
				out = append(out, c.fail("the leader of "+a+" is not a primary", nil))
			} else {
				out = append(out, c.pass("the leader of "+a+" is the primary", map[string]any{"leader": memberFacts(*s.leader)}))
			}

			continue
		}

		if why, facts := in.follows(d.Name, a); len(why) > 0 {
			out = append(out, c.fail(strings.Join(why, "; "), facts))
		} else {
			out = append(out, c.pass(d.Name+" streams from "+a+" on its timeline", facts))
		}
	}

	return out
}

// verifyJobs has a result for each datacenter: managed jobs healthy in the active one
// and stopped in the others. A datacenter whose Nomad does not answer is a warning.
func (in *Input) verifyJobs() []check.Check {
	a, ok := in.active()
	if !ok {
		return []check.Check{info(check.VerifyJobs, "").fail("the active datacenter is not known", nil)}
	}

	out := make([]check.Check, 0, len(in.Status.Datacenters))

	for _, d := range in.Status.Datacenters {
		c := info(check.VerifyJobs, d.Name)

		if !d.Source(status.SystemNomad).Reachable {
			out = append(out, c.warn("Nomad of "+d.Name+" does not answer: "+string(d.Source(status.SystemNomad).Reason), nil))

			continue
		}

		want := status.JobStopped
		if d.Name == a {
			want = status.JobHealthy
		}

		var wrong []any

		for _, j := range jobsOf(d.Nomad) {
			if j.State != want {
				wrong = append(wrong, map[string]any{"job": j.Name, "state": string(j.State)})
			}
		}

		if len(wrong) > 0 {
			out = append(out, c.fail(fmt.Sprintf("%d managed jobs of %s are not %s", len(wrong), d.Name, want),
				map[string]any{"jobs": wrong}))
		} else {
			out = append(out, c.pass(fmt.Sprintf("every managed job of %s is %s", d.Name, want), nil))
		}
	}

	return out
}

// verifyState needs the effective state on a majority of voters and on every voter
// that answers.
func (in *Input) verifyState() check.Check {
	c := info(check.VerifyState, "")
	facts := map[string]any{"voters": in.votersFacts()}

	eff := in.Status.View.Effective
	if in.Status.Effective == nil || eff == nil {
		return c.fail("the voters give no effective state", facts)
	}

	if len(eff.Voters) < in.Status.View.Majority {
		return c.fail(fmt.Sprintf("generation %d is on %d voters, %d required", eff.State.Generation, len(eff.Voters),
			in.Status.View.Majority), facts)
	}

	var behind []string

	for _, v := range in.Status.Voters {
		if v.Reachable && !v.Valid {
			behind = append(behind, v.Name)
		}
	}

	if len(behind) > 0 {
		return c.fail("voters do not hold the effective state: "+strings.Join(behind, ", "), facts)
	}

	return c.pass(fmt.Sprintf("generation %d with %s active is on every voter that answers", eff.State.Generation,
		eff.State.ActiveDatacenter), facts)
}
