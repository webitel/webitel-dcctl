package cli

import (
	"fmt"
	"io"
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

func kv(w io.Writer, key, value string) {
	_, _ = fmt.Fprintf(w, "%-16s %s\n", key+":", value)
}

func instanceText(i *dcctlv1.Instance) string {
	if i == nil {
		return "-"
	}

	text := dash(i.GetNode())
	if dc := shortName(i.GetDatacenter()); dc != "" {
		text += " (" + dc + ")"
	}

	return text
}

func initiatorText(i *dcctlv1.Initiator) string {
	switch {
	case i == nil:
		return "-"
	case i.GetSystem():
		return "system"
	}

	text := dash(i.GetUser())
	if i.GetAddress() != "" {
		text += "@" + i.GetAddress()
	}

	return text
}

func printStatus(w io.Writer, st *dcctlv1.Status) {
	active := dash(shortName(st.GetActiveDatacenter()))
	if st.GetActiveDatacenter() != "" {
		active = fmt.Sprintf("%s (generation %d%s)", active, st.GetGeneration(), map[bool]string{true: ", forced"}[st.GetForced()])
	}

	kv(w, "Active", active)

	if d := st.GetDetectedActiveDatacenter(); d != "" && d != st.GetActiveDatacenter() {
		kv(w, "Detected", shortName(d)+" runs the database primary")
	}

	kv(w, "Profile", st.GetProfile().String())
	kv(w, "Initialized", yesNo(st.GetInitialized()))
	kv(w, "Observed by", instanceText(st.GetObserver())+" at "+timeOf(st.GetObserveTime()))

	if q := st.GetQuorum(); q != nil {
		kv(w, "Quorum", fmt.Sprintf("%s: %d of %d voters answer, %d required (%s)",
			map[bool]string{true: "available", false: "NOT available"}[q.GetAvailable()],
			q.GetReachableCount(), q.GetTotalCount(), q.GetRequiredCount(), q.GetMode()))
	}

	if l := st.GetLease(); l != nil {
		kv(w, "Lease", fmt.Sprintf("held by %s, generation %d, lost: %s", dash(shortName(l.GetHolder())), l.GetGeneration(), yesNo(l.GetLost())))
	}

	if op := st.GetOperation(); op != nil {
		kv(w, "Operation", fmt.Sprintf("%s %s %s -> %s", op.GetName(), op.GetType(), op.GetState(), dash(shortName(op.GetTarget()))))
	}

	if n := st.GetNeedsRecoveryDatacenters(); len(n) > 0 {
		kv(w, "Needs recovery", strings.Join(n, ", "))
	}

	for _, a := range st.GetAlarms() {
		kv(w, "Alarm", fmt.Sprintf("%s %s %s: %s", a.GetSeverity(), a.GetType(), dash(shortName(a.GetDatacenter())), a.GetDetail()))
	}

	if len(st.GetDatacenters()) > 0 {
		_, _ = fmt.Fprintln(w)
		printDatacenters(w, st.GetDatacenters())
	}
}

func lagText(v *int64) string {
	if v == nil {
		return "-"
	}

	return fmt.Sprintf("%d", *v)
}

func printDatacenters(w io.Writer, dcs []*dcctlv1.Datacenter) {
	tw := newTable(w)

	row(tw, "DATACENTER", "STATE", "HEALTH", "DB LEADER", "DB ROLE", "LAG BYTES", "NOMAD READY NODES")

	for _, d := range dcs {
		row(tw, shortName(d.GetName()), d.GetState().String(), d.GetHealth().String(), dash(d.GetPatroni().GetLeader()),
			d.GetPatroni().GetRole().String(), lagText(d.GetPatroni().LagBytes), fmt.Sprint(d.GetNomad().GetReadyNodeCount()))
	}

	_ = tw.Flush()
}

func printDatacenter(w io.Writer, d *dcctlv1.Datacenter) {
	kv(w, "Datacenter", shortName(d.GetName()))
	kv(w, "State", d.GetState().String())
	kv(w, "Health", d.GetHealth().String()+reasonsText(d.GetHealthReasons()))
	kv(w, "Needs recovery", yesNo(d.GetNeedsRecovery()))
	kv(w, "Observed by", instanceText(d.GetObserver())+" at "+timeOf(d.GetObserveTime()))

	if len(d.GetSources()) > 0 {
		_, _ = fmt.Fprintln(w)

		tw := newTable(w)

		row(tw, "SOURCE", "ENDPOINT", "REACHABLE", "OBSERVED")

		for _, src := range d.GetSources() {
			row(tw, src.GetSystem().String(), dash(src.GetEndpoint()), yesNo(src.GetReachable()), timeOf(src.GetObserveTime()))
		}

		_ = tw.Flush()
	}

	if p := d.GetPatroni(); p != nil {
		_, _ = fmt.Fprintln(w)
		kv(w, "DB leader", dash(p.GetLeader())+" "+p.GetRole().String())
		kv(w, "DB timeline", lagText(p.Timeline))
		kv(w, "DB lag bytes", lagText(p.LagBytes))
		kv(w, "DB divergence", p.GetDivergenceState().String())
		printMembers(w, p.GetMembers())
	}

	if n := d.GetNomad(); n != nil {
		_, _ = fmt.Fprintln(w)
		kv(w, "Nomad leader", dash(n.GetLeader()))
		kv(w, "Nomad nodes", fmt.Sprintf("%d ready, %d ineligible, %d draining, %d down",
			n.GetReadyNodeCount(), n.GetIneligibleNodeCount(), n.GetDrainingNodeCount(), n.GetDownNodeCount()))
	}

	if len(d.GetInfoChecks()) > 0 {
		_, _ = fmt.Fprintln(w)
		printChecks(w, d.GetInfoChecks())
	}
}

func reasonsText(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}

	return " (" + strings.Join(reasons, ", ") + ")"
}

func printMembers(w io.Writer, members []*dcctlv1.Datacenter_Patroni_Member) {
	if len(members) == 0 {
		return
	}

	_, _ = fmt.Fprintln(w)

	tw := newTable(w)

	row(tw, "MEMBER", "HOST", "ROLE", "STATE", "REACHABLE", "LAG BYTES", "USABLE")

	for _, m := range members {
		row(tw, m.GetMember(), dash(m.GetHost()), m.GetRole().String(), dash(m.GetPatroniState()), yesNo(m.GetReachable()),
			lagText(m.LagBytes), yesNo(m.GetUsable()))
	}

	_ = tw.Flush()
}

func printOperations(w io.Writer, ops []*dcctlv1.Operation) {
	tw := newTable(w)

	row(tw, "OPERATION", "TYPE", "STATE", "TARGET", "INITIATOR", "STARTED", "ENDED")

	for _, op := range ops {
		row(tw, shortName(op.GetName()), op.GetType().String(), op.GetState().String(), dash(shortName(op.GetTarget())),
			initiatorText(op.GetInitiator()), timeOf(op.GetCreateTime()), timeOf(op.GetEndTime()))
	}

	_ = tw.Flush()
}

// printOperation writes the details of an operation: what it is, how it
// ended, its checks and its steps.
// generationText is the generation op starts from and the one it writes; an operation
// that writes none, as demote, shows the one it acts under.
func generationText(op *dcctlv1.Operation) string {
	if op.GetNewGeneration() == 0 {
		return fmt.Sprint(op.GetPreviousGeneration())
	}

	return fmt.Sprintf("%d -> %d", op.GetPreviousGeneration(), op.GetNewGeneration())
}

func printOperation(w io.Writer, op *dcctlv1.Operation) {
	if op.GetName() != "" {
		kv(w, "Operation", op.GetName())
	}

	if op.GetType() != dcctlv1.Operation_TYPE_UNSPECIFIED {
		kv(w, "Type", op.GetType().String())
	}

	if op.GetState() != dcctlv1.Operation_STATE_UNSPECIFIED {
		kv(w, "State", op.GetState().String())
	}

	kv(w, "Target", dash(shortName(op.GetTarget())))
	kv(w, "Mode", op.GetMode().String())

	if op.GetName() != "" {
		kv(w, "Initiator", initiatorText(op.GetInitiator()))
		kv(w, "Leader", instanceText(op.GetLeader()))
		kv(w, "Generation", generationText(op))
		kv(w, "Started", timeOf(op.GetCreateTime()))
		kv(w, "Ended", timeOf(op.GetEndTime()))
	}

	if e := op.GetError(); e != nil {
		kv(w, "Error", fmt.Sprintf("%s: %s", dash(e.GetReason()), e.GetDetail()))
	}

	for _, warn := range op.GetWarnings() {
		kv(w, "Warning", warn)
	}

	if len(op.GetChecks()) > 0 {
		_, _ = fmt.Fprintln(w)
		printChecks(w, op.GetChecks())
		printCheckFacts(w, op.GetChecks())
	}

	if len(op.GetSteps()) > 0 {
		_, _ = fmt.Fprintln(w)
		printSteps(w, op.GetSteps())
	}
}

func printJournal(w io.Writer, entries []*dcctlv1.JournalEntry) {
	tw := newTable(w)

	row(tw, "TIME", "TYPE", "RESULT", "DATACENTER", "OPERATION", "INITIATOR", "DETAIL")

	for _, e := range entries {
		detail := e.GetDetail()
		if e.GetError() != nil {
			detail = strings.TrimSpace(e.GetError().GetReason() + " " + detail)
		}

		row(tw, timeOf(e.GetCreateTime()), e.GetType().String(), e.GetResult().String(), dash(shortName(e.GetDatacenter())),
			dash(shortName(e.GetOperation())), initiatorText(e.GetInitiator()), detail)
	}

	_ = tw.Flush()
}
