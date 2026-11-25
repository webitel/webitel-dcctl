package preflight

import (
	"fmt"
	"slices"
	"strings"

	"github.com/webitel/webitel-dcctl/internal/check"
	"github.com/webitel/webitel-dcctl/internal/jobstate"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// nomadOf returns the Nomad of dc when it answered the fresh collection.
func (in *Input) nomadOf(dc string) (status.Nomad, status.Source, bool) {
	d, ok := in.datacenter(dc)
	if !ok {
		return status.Nomad{}, status.Source{System: status.SystemNomad}, false
	}

	src := d.Source(status.SystemNomad)

	return d.Nomad, src, src.Reachable
}

func jobsOf(n status.Nomad) []status.Job {
	var out []status.Job
	for _, st := range n.Stages {
		out = append(out, st.Jobs...)
	}

	return out
}

func (in *Input) nomadReachable(dc string) check.Check {
	c := block(check.NomadReachable, dc)

	if dc == "" {
		return c.fail("the active datacenter is not known", nil)
	}

	n, src, ok := in.nomadOf(dc)
	if !ok {
		return c.unreachable("Nomad of "+dc, src.Reason)
	}

	return c.pass("Nomad of "+dc+" answers", map[string]any{"version": n.Version})
}

// stagesValid fails on a managed job with an invalid meta.dcctl_stage: stop takes such
// a job down and start refuses to run, so the apps of the datacenter could not be
// started again.
func (in *Input) stagesValid(dc string) check.Check {
	c := block(check.NomadStagesValid, dc)

	if dc == "" {
		return c.fail("the active datacenter is not known", nil)
	}

	n, src, ok := in.nomadOf(dc)
	if !ok {
		return c.unreachable("Nomad of "+dc, src.Reason)
	}

	var invalid []string

	for _, j := range jobsOf(n) {
		if j.StageInvalid {
			invalid = append(invalid, j.Name)
		}
	}

	if len(invalid) > 0 {
		return c.fail("managed jobs have an invalid meta.dcctl_stage: "+strings.Join(invalid, ", "),
			map[string]any{"jobs": strs(invalid)})
	}

	return c.pass("every managed job has a valid stage", nil)
}

func (in *Input) targetJobsPresent() check.Check {
	b := in.Params.Datacenter
	c := block(check.NomadTargetJobsPresent, b)

	a, ok := in.active()
	if !ok {
		return c.fail("the active datacenter is not known", nil)
	}

	na, srcA, okA := in.nomadOf(a)
	nb, srcB, okB := in.nomadOf(b)

	switch {
	case !okA:
		return c.unreachable("Nomad of "+a, srcA.Reason)
	case !okB:
		return c.unreachable("Nomad of "+b, srcB.Reason)
	}

	var missing []string

	for _, j := range jobsOf(na) {
		if !slices.ContainsFunc(jobsOf(nb), func(x status.Job) bool { return x.Name == j.Name }) {
			missing = append(missing, j.Name)
		}
	}

	if len(missing) > 0 {
		return c.fail("managed jobs of "+a+" are missing in "+b+": "+strings.Join(missing, ", "),
			map[string]any{"missing": strs(missing)})
	}

	return c.pass("every managed job of "+a+" is in "+b, nil)
}

// notStopped returns the managed jobs of n that are not stopped, as facts.
func notStopped(n status.Nomad) []any {
	var out []any

	for _, j := range jobsOf(n) {
		if j.State != status.JobStopped {
			out = append(out, map[string]any{"job": j.Name, "state": string(j.State), "detail": j.Detail})
		}
	}

	return out
}

func (in *Input) targetJobsStopped() check.Check {
	b := in.Params.Datacenter
	c := block(check.NomadTargetJobsStopped, b)

	n, src, ok := in.nomadOf(b)
	if !ok {
		return c.unreachable("Nomad of "+b, src.Reason)
	}

	if js := notStopped(n); len(js) > 0 {
		return c.fail(fmt.Sprintf("%d managed jobs of %s are not stopped", len(js), b), map[string]any{"jobs": js})
	}

	return c.pass("every managed job of "+b+" is stopped", nil)
}

func (in *Input) dcAppsStopped() check.Check {
	dc := in.Params.Datacenter
	c := warnOnly(check.NomadDCAppsStopped, dc)

	n, src, ok := in.nomadOf(dc)
	if !ok {
		return c.unreachable("Nomad of "+dc, src.Reason)
	}

	if js := notStopped(n); len(js) > 0 {
		return c.fail(fmt.Sprintf("%d managed jobs of %s are not stopped", len(js), dc), map[string]any{"jobs": js})
	}

	return c.pass("every managed job of "+dc+" is stopped", nil)
}

// plan checks the plans of the jobs of dc that keep: a job without a plan, whose plan
// failed or that Nomad cannot place fails it, and so does no job at all when needJobs.
func (in *Input) plan(id check.ID, dc string, needJobs bool, keep func(status.Job) bool) check.Check {
	c := block(id, dc)

	if dc == "" {
		return c.fail("the active datacenter is not known", nil)
	}

	n, src, ok := in.nomadOf(dc)
	if !ok {
		return c.unreachable("Nomad of "+dc, src.Reason)
	}

	var (
		failed  []any
		planned int
	)

	for _, j := range jobsOf(n) {
		if !keep(j) {
			continue
		}

		planned++

		p, ok := in.Plans[dc][j.Name]

		switch {
		case !ok:
			failed = append(failed, map[string]any{"job": j.Name, "error": "not planned"})
		case p.Err != nil:
			failed = append(failed, map[string]any{"job": j.Name, "error": p.Err.Error()})
		case len(p.Result.FailedTGAllocs) > 0:
			failed = append(failed, map[string]any{"job": j.Name, "task_groups": strs(p.Result.FailedTGAllocs)})
		}
	}

	facts := map[string]any{"failed": failed, "planned": planned}

	if needJobs && planned == 0 {
		return c.fail(dc+" has no managed job to start", facts)
	}

	if len(failed) > 0 {
		return c.fail(fmt.Sprintf("%d managed jobs of %s cannot be placed", len(failed), dc), facts)
	}

	return c.pass(fmt.Sprintf("Nomad places every one of %d managed jobs of %s", planned, dc), facts)
}

// targetPlan needs every managed job of the target placed; a target without any would
// take over without apps.
func (in *Input) targetPlan() check.Check {
	return in.plan(check.NomadTargetPlan, in.Params.Datacenter, true, func(status.Job) bool { return true })
}

func (in *Input) activePlan() check.Check {
	return in.plan(check.NomadActivePlan, in.activeName(), false, func(j status.Job) bool { return j.State == status.JobStopped })
}

// targetNodes warns about the nodes of the target out of scheduling that run the
// service of a managed system job: that job will not run there.
func (in *Input) targetNodes() check.Check {
	b := in.Params.Datacenter
	c := warnOnly(check.NomadTargetNodes, b)

	d, ok := in.Snapshot.Datacenter(b)
	if !ok || !d.Nomad.Reachable {
		reason := "unknown"
		if ok {
			reason = string(d.Nomad.Reason)
		}

		return c.fail("Nomad of "+b+" does not answer: "+reason, nil)
	}

	var out []any

	for _, node := range d.Nomad.Data.Nodes {
		reason := jobstate.OutOfService(node)
		if reason == "" {
			continue
		}

		for _, j := range d.Nomad.Data.Managed {
			if j.Type == jobstate.TypeSystem && jobstate.RunsService(node, j.ID) {
				out = append(out, map[string]any{"node": node.Name, "reason": string(reason), "job": j.ID})
			}
		}
	}

	if len(out) > 0 {
		return c.fail(fmt.Sprintf("%d nodes of system jobs in %s are out of scheduling", len(out), b), map[string]any{"nodes": out})
	}

	return c.pass("every node of the system jobs of "+b+" is in scheduling", nil)
}

// targetRabbitMQ reuses the informational check of the status: the RabbitMQ service of
// the target has a passing instance there.
func (in *Input) targetRabbitMQ(effect check.Effect) check.Check {
	b := in.Params.Datacenter
	c := spec{id: check.InfraTargetRabbitMQ, effect: effect, datacenter: b}

	d, ok := in.datacenter(b)
	if !ok {
		return c.fail(fmt.Sprintf("%q is not a configured datacenter", b), nil)
	}

	i := slices.IndexFunc(d.InfoChecks, func(x check.Check) bool { return x.ID == check.InfoRabbitMQ })
	if i < 0 {
		return c.fail("RabbitMQ of "+b+" is not known", nil)
	}

	ic := d.InfoChecks[i]
	if ic.Result != check.Pass {
		return c.fail(ic.Detail, ic.Facts)
	}

	return c.pass(ic.Detail, ic.Facts)
}
