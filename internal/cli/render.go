package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// marshalJSON returns m as JSON with the field names of the proto files. The
// output is the same for the same message: protojson adds random spaces on
// purpose, which the round through encoding/json takes out.
func marshalJSON(m proto.Message, indent bool) ([]byte, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode the response: %w", err)
	}

	var buf bytes.Buffer

	if indent {
		err = json.Indent(&buf, raw, "", "  ")
	} else {
		err = json.Compact(&buf, raw)
	}

	if err != nil {
		return nil, fmt.Errorf("encode the response: %w", err)
	}

	return buf.Bytes(), nil
}

// printJSON writes m to the output as indented JSON.
func (s *session) printJSON(m proto.Message) error {
	return writeJSON(s.out, m, true)
}

func writeJSON(w io.Writer, m proto.Message, indent bool) error {
	b, err := marshalJSON(m, indent)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "%s\n", b)

	return err
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func row(w io.Writer, cols ...string) {
	_, _ = fmt.Fprintln(w, strings.TrimRight(strings.Join(cols, "\t"), "\t"))
}

func timeOf(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "-"
	}

	return ts.AsTime().UTC().Format(time.RFC3339)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// shortName returns the ID of a resource name: "dc_a" for "datacenters/dc_a".
func shortName(name string) string {
	_, id, ok := strings.Cut(name, "/")
	if !ok {
		return name
	}

	return id
}

// printChecks writes the checks as a table.
func printChecks(w io.Writer, checks []*dcctlv1.Check) {
	if len(checks) == 0 {
		return
	}

	tw := newTable(w)

	row(tw, "CHECK", "RESULT", "EFFECT", "DATACENTER", "DETAIL")

	for _, c := range checks {
		row(tw, c.GetId(), c.GetResult().String(), c.GetEffect().String(), dash(shortName(c.GetDatacenter())), c.GetDetail())
	}

	_ = tw.Flush()
}

// printSteps writes the steps of an operation as a table.
func printSteps(w io.Writer, steps []*dcctlv1.Operation_Step) {
	if len(steps) == 0 {
		return
	}

	tw := newTable(w)

	row(tw, "STEP", "TYPE", "STAGE", "STATE", "DATACENTER", "DETAIL")

	for _, st := range steps {
		stage := "-"
		if st.GetStage() != 0 {
			stage = fmt.Sprint(st.GetStage())
		}

		detail := st.GetDetail()
		if st.GetPointOfNoReturn() {
			detail = strings.TrimSpace(detail + " (point of no return)")
		}

		row(tw, fmt.Sprint(st.GetIndex()), st.GetType().String(), stage, st.GetState().String(),
			dash(shortName(st.GetDatacenter())), detail)

		for _, w2 := range st.GetWarnings() {
			row(tw, "", "", "", "", "", "warning: "+w2)
		}
	}

	_ = tw.Flush()
}

// printCheckFacts writes the facts of the checks that did not pass, under the
// table of checks: they are what a confirmation is given on, so the operator
// sees them whoever answered the question, the flags or the terminal.
func printCheckFacts(w io.Writer, checks []*dcctlv1.Check) {
	for _, c := range checks {
		if c.GetFacts() == nil || c.GetResult() != dcctlv1.Check_FAIL && c.GetResult() != dcctlv1.Check_WARN {
			continue
		}

		b, err := marshalJSON(c.GetFacts(), false)
		if err != nil {
			continue
		}

		_, _ = fmt.Fprintf(w, "facts of %s %s: %s\n", c.GetId(), dash(shortName(c.GetDatacenter())), b)
	}
}
