package cli

import (
	"regexp"
	"strings"

	"github.com/urfave/cli/v2"
)

var (
	// datacenterID is what the API accepts as the name of a datacenter.
	datacenterID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	// operationID is the ID of an operation: a UUID, or a shorter ID.
	operationID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// datacenterArg returns the ID of the datacenter in arg, which is an ID or
// the resource name datacenters/{id}.
func datacenterArg(arg string) (string, error) {
	id := strings.TrimPrefix(arg, "datacenters/")
	if !datacenterID.MatchString(id) {
		return "", usageErrorf("%q is not a datacenter name", arg)
	}

	return id, nil
}

// operationArg returns the ID of the operation in arg, which is an ID or the
// resource name operations/{id}.
func operationArg(arg string) (string, error) {
	id := strings.TrimPrefix(arg, "operations/")
	if !operationID.MatchString(id) {
		return "", usageErrorf("%q is not an operation ID", arg)
	}

	return id, nil
}

// exactArgs returns the check that the command gets n arguments.
func exactArgs(n int, usage string) cli.BeforeFunc {
	return func(c *cli.Context) error {
		if c.NArg() != n {
			return usageErrorf("%s: expected %s, got %d argument(s)", c.Command.Name, usage, c.NArg())
		}

		return nil
	}
}
