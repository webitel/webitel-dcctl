package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/urfave/cli/v2"
	"google.golang.org/protobuf/proto"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// maxPageSize is the largest page the API serves.
const maxPageSize = 500

const flagLimit = "limit"

func limitFlag(def int) cli.Flag {
	return &cli.IntFlag{Name: flagLimit, Usage: "show at most this many entries", Value: def}
}

// limitOf returns --limit, which must be at least 1.
func limitOf(c *cli.Context) (int, error) {
	n := c.Int(flagLimit)
	if n < 1 {
		return 0, usageErrorf("--%s must be at least 1", flagLimit)
	}

	return n, nil
}

func statusCommand(o Options) *cli.Command {
	return leaf(&cli.Command{
		Name:      "status",
		Usage:     "Show the state of the installation, or the details of one datacenter",
		ArgsUsage: "[<dc>]",
		Flags:     connFlags(),
		Before: func(c *cli.Context) error {
			if c.NArg() > 1 {
				return usageErrorf("status: expected at most one datacenter, got %d arguments", c.NArg())
			}

			if c.NArg() == 1 {
				_, err := datacenterArg(c.Args().First())

				return err
			}

			return nil
		},
		Action: o.run(func(ctx context.Context, c *cli.Context, s *session) error {
			if c.NArg() == 0 {
				st, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.Status, error) {
					return s.status.GetStatus(ctx, &dcctlv1.GetStatusRequest{})
				})
				if err != nil {
					return err
				}

				return s.show(st, func() { printStatus(s.out, st) })
			}

			dc, _ := datacenterArg(c.Args().First())

			d, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.Datacenter, error) {
				return s.datacenters.GetDatacenter(ctx, &dcctlv1.GetDatacenterRequest{Name: "datacenters/" + dc})
			})
			if err != nil {
				return err
			}

			return s.show(d, func() { printDatacenter(s.out, d) })
		}),
	})
}

// show prints m as JSON for -o json, else what table prints.
func (s *session) show(m proto.Message, table func()) error {
	if s.asJSON {
		return s.printJSON(m)
	}

	table()

	return nil
}

func datacentersCommand(o Options) *cli.Command {
	return leaf(&cli.Command{
		Name:    "datacenters",
		Aliases: []string{"dcs"},
		Usage:   "List the datacenters and their states",
		Flags:   connFlags(),
		Before:  noArgs,
		Action: o.run(func(ctx context.Context, _ *cli.Context, s *session) error {
			resp, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.ListDatacentersResponse, error) {
				return s.datacenters.ListDatacenters(ctx, &dcctlv1.ListDatacentersRequest{})
			})
			if err != nil {
				return err
			}

			return s.show(resp, func() { printDatacenters(s.out, resp.GetDatacenters()) })
		}),
	})
}

func verifyCommand(o Options) *cli.Command {
	return leaf(&cli.Command{
		Name:   "verify",
		Usage:  "Check the conditions of a finished switch of the active datacenter",
		Flags:  connFlags(),
		Before: noArgs,
		Action: o.run(func(ctx context.Context, _ *cli.Context, s *session) error {
			resp, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.VerifyStatusResponse, error) {
				return s.status.VerifyStatus(ctx, &dcctlv1.VerifyStatusRequest{})
			})
			if err != nil {
				return err
			}

			if err := s.show(resp, func() { printChecks(s.out, resp.GetChecks()) }); err != nil {
				return err
			}

			if slices.ContainsFunc(resp.GetChecks(), func(c *dcctlv1.Check) bool { return c.GetResult() == dcctlv1.Check_FAIL }) {
				return exit(ExitFailed)
			}

			return nil
		}),
	})
}

func opsCommand(o Options) *cli.Command {
	return leaf(&cli.Command{
		Name:      "ops",
		Aliases:   []string{"operations"},
		Usage:     "List the operations, newest first; get shows one, watch follows one",
		ArgsUsage: "[get <op> | watch <op>]",
		Flags:     append(connFlags(), limitFlag(20)),
		Before: func(c *cli.Context) error {
			switch args := c.Args().Slice(); {
			case len(args) == 0:
				_, err := limitOf(c)

				return err
			case args[0] != "get" && args[0] != "watch":
				return usageErrorf("unknown command %q", args[0])
			case len(args) != 2:
				return usageErrorf("ops %s: expected an operation, got %d argument(s)", args[0], len(args)-1)
			}

			_, err := operationArg(c.Args().Get(1))

			return err
		},
		Action: o.run(func(ctx context.Context, c *cli.Context, s *session) error {
			switch c.Args().First() {
			case "":
				return listOperations(ctx, c, s)
			case "watch":
				id, _ := operationArg(c.Args().Get(1))

				return s.watchOperation(ctx, id)
			}

			return getOperation(ctx, c, s)
		}),
	})
}

func listOperations(ctx context.Context, c *cli.Context, s *session) error {
	limit, _ := limitOf(c)

	var ops []*dcctlv1.Operation

	token := ""

	for len(ops) < limit {
		resp, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.ListOperationsResponse, error) {
			return s.operations.ListOperations(ctx, &dcctlv1.ListOperationsRequest{
				PageSize:  int32(min(limit-len(ops), maxPageSize)), // #nosec G115 -- at most maxPageSize.
				PageToken: token,
			})
		})
		if err != nil {
			return err
		}

		ops = append(ops, resp.GetOperations()...)

		// A page without entries or with the token of the last one would repeat.
		next := resp.GetNextPageToken()
		if next == "" || next == token || len(resp.GetOperations()) == 0 {
			break
		}

		token = next
	}

	ops = ops[:min(len(ops), limit)]

	if s.asJSON {
		return s.printJSON(&dcctlv1.ListOperationsResponse{Operations: ops})
	}

	printOperations(s.out, ops)

	return nil
}

func getOperation(ctx context.Context, c *cli.Context, s *session) error {
	id, _ := operationArg(c.Args().Get(1))

	op, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.Operation, error) {
		return s.operations.GetOperation(ctx, &dcctlv1.GetOperationRequest{Name: "operations/" + id})
	})
	if err != nil {
		return err
	}

	return s.show(op, func() { printOperation(s.out, op) })
}

func journalCommand(o Options) *cli.Command {
	return leaf(&cli.Command{
		Name:  "journal",
		Usage: "Show the journal, newest first",
		Flags: append(connFlags(), limitFlag(50),
			&cli.StringFlag{Name: "op", Usage: "only entries of this operation"},
			&cli.StringFlag{Name: "type", Usage: "only entries of this type: OPERATION, STEP, SYSTEM or ALARM"},
			&cli.StringFlag{Name: "dc", Usage: "only entries of this datacenter"},
		),
		Before: func(c *cli.Context) error {
			if err := noArgs(c); err != nil {
				return err
			}

			if _, err := limitOf(c); err != nil {
				return err
			}

			_, err := journalFilter(c)

			return err
		},
		Action: o.run(func(ctx context.Context, c *cli.Context, s *session) error {
			limit, _ := limitOf(c)
			filter, _ := journalFilter(c)

			var entries []*dcctlv1.JournalEntry

			token := ""

			var unreachable []string

			for len(entries) < limit {
				resp, err := call(ctx, s, func(ctx context.Context) (*dcctlv1.ListJournalEntriesResponse, error) {
					return s.journal.ListJournalEntries(ctx, &dcctlv1.ListJournalEntriesRequest{
						PageSize:  int32(min(limit-len(entries), maxPageSize)), // #nosec G115 -- at most maxPageSize.
						PageToken: token,
						Filter:    filter,
					})
				})
				if err != nil {
					return err
				}

				entries = append(entries, resp.GetJournalEntries()...)
				unreachable = appendMissing(unreachable, resp.GetUnreachable())

				next := resp.GetNextPageToken()
				if next == "" || next == token || len(resp.GetJournalEntries()) == 0 {
					break
				}

				token = next
			}

			entries = entries[:min(len(entries), limit)]

			if len(unreachable) > 0 {
				s.warnf("the journal may be incomplete: no answer from %s", strings.Join(unreachable, ", "))
			}

			if s.asJSON {
				return s.printJSON(&dcctlv1.ListJournalEntriesResponse{JournalEntries: entries, Unreachable: unreachable})
			}

			printJournal(s.out, entries)

			return nil
		}),
	})
}

// journalFilter builds the filter of ListJournalEntries from the flags. The
// values are checked against what they may be, so none can add to the filter.
func journalFilter(c *cli.Context) (string, error) {
	var terms []string

	if v := c.String("type"); v != "" {
		name := strings.ToUpper(v)
		if _, ok := dcctlv1.JournalEntry_Type_value[name]; !ok || name == "TYPE_UNSPECIFIED" {
			return "", usageErrorf("--type %q: expected OPERATION, STEP, SYSTEM or ALARM", v)
		}

		terms = append(terms, fmt.Sprintf("type = %q", name))
	}

	if v := c.String("dc"); v != "" {
		id, err := datacenterArg(v)
		if err != nil {
			return "", err
		}

		terms = append(terms, fmt.Sprintf("datacenter = %q", "datacenters/"+id))
	}

	if v := c.String("op"); v != "" {
		id, err := operationArg(v)
		if err != nil {
			return "", err
		}

		terms = append(terms, fmt.Sprintf("operation = %q", "operations/"+id))
	}

	return strings.Join(terms, " AND "), nil
}

// noArgs rejects arguments for a command that takes none.
func noArgs(c *cli.Context) error {
	if c.Args().Present() {
		return usageErrorf("%s takes no arguments, got %q", c.Command.Name, c.Args().First())
	}

	return nil
}

// appendMissing appends the values of add that dst does not have.
func appendMissing(dst, add []string) []string {
	for _, v := range add {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}

	return dst
}
