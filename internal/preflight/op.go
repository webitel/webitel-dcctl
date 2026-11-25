package preflight

import (
	"strings"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
	"github.com/webitel/webitel-dcctl/internal/coord"
	"github.com/webitel/webitel-dcctl/internal/status"
)

// passing is the Consul check status of a healthy instance.
const passing = "passing"

// operation returns the operation of [Params.Operation] as the latest updated copy
// shows it; nil when no copy that answered holds it.
func (in *Input) operation() *dcctlv1.Operation {
	name := "operations/" + in.Params.Operation

	var out *dcctlv1.Operation

	for _, c := range status.Operations(in.Snapshot).Copies {
		if c.Status != coord.StatusPresent || c.Operation.GetName() != name {
			continue
		}

		if out == nil || c.Operation.GetUpdateTime().AsTime().After(out.GetUpdateTime().AsTime()) {
			out = c.Operation
		}
	}

	return out
}

// datacenterID returns the datacenter of a name that may be the resource name
// datacenters/{datacenter}.
func datacenterID(name string) string {
	return strings.TrimPrefix(name, "datacenters/")
}
