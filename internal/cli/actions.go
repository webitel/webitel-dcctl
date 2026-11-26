package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v2"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// datacenterAction returns the command of an action on the datacenter in its
// only argument.
func (o Options) datacenterAction(name, usage string, flags []cli.Flag,
	call func(s *session, dc string) invoke,
) *cli.Command {
	return o.action(actionSpec{
		name:      name,
		usage:     usage,
		argsUsage: "<dc>",
		flags:     flags,
		before: func(c *cli.Context) error {
			if c.NArg() != 1 {
				return usageErrorf("%s: expected a datacenter, got %d argument(s)", name, c.NArg())
			}

			_, err := datacenterArg(c.Args().First())

			return err
		},
		plan: func(_ context.Context, c *cli.Context, s *session) (plan, error) {
			dc, _ := datacenterArg(c.Args().First())

			return plan{
				invoke:     call(s, dc),
				describe:   fmt.Sprintf("%s %s", name, dc),
				guard:      dc,
				idempotent: true,
			}, nil
		},
	})
}

func (o Options) switchCommand() *cli.Command {
	return o.datacenterAction("switch", "Switch the active datacenter to <dc>", nil, func(s *session, dc string) invoke {
		return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
			return s.datacenters.SwitchDatacenter(ctx, &dcctlv1.SwitchDatacenterRequest{
				Name: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
			})
		}
	})
}

func (o Options) failoverCommand() *cli.Command {
	return o.datacenterAction("failover", "Make <dc> active although the active datacenter does not answer",
		[]cli.Flag{isolationFlag(), notMaxLSNFlag(), maxDiscardFlag()},
		func(s *session, dc string) invoke {
			return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
				return s.datacenters.FailoverDatacenter(ctx, &dcctlv1.FailoverDatacenterRequest{
					Name: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
				})
			}
		})
}

func (o Options) demoteCommand() *cli.Command {
	return o.datacenterAction("demote", "Turn the database of <dc>, a former primary, into a standby",
		[]cli.Flag{maxDiscardFlag()},
		func(s *session, dc string) invoke {
			return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
				return s.datacenters.DemoteDatacenter(ctx, &dcctlv1.DemoteDatacenterRequest{
					Name: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
				})
			}
		})
}

func (o Options) repointCommand() *cli.Command {
	return o.datacenterAction("repoint", "Make the standby database of <dc> follow the active datacenter",
		[]cli.Flag{maxDiscardFlag()},
		func(s *session, dc string) invoke {
			return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
				return s.datacenters.RepointDatacenter(ctx, &dcctlv1.RepointDatacenterRequest{
					Name: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
				})
			}
		})
}

func (o Options) markRecoveredCommand() *cli.Command {
	return o.datacenterAction("mark-recovered", "Record that <dc> is recovered and may be a target again", nil,
		func(s *session, dc string) invoke {
			return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
				return s.datacenters.MarkDatacenterRecovered(ctx, &dcctlv1.MarkDatacenterRecoveredRequest{
					Name: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
				})
			}
		})
}

func (o Options) resolveCommand() *cli.Command {
	return o.action(actionSpec{
		name:      "resolve",
		usage:     "Settle a conflict of generation records: the record of <winner> wins",
		argsUsage: "<winner>",
		flags:     []cli.Flag{isolationFlag()},
		before: func(c *cli.Context) error {
			if c.NArg() != 1 {
				return usageErrorf("resolve: expected the winning datacenter, got %d argument(s)", c.NArg())
			}

			_, err := datacenterArg(c.Args().First())

			return err
		},
		plan: func(_ context.Context, c *cli.Context, s *session) (plan, error) {
			dc, _ := datacenterArg(c.Args().First())

			return plan{
				invoke: func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
					return s.datacenters.ResolveConflict(ctx, &dcctlv1.ResolveConflictRequest{
						Winner: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
					})
				},
				describe:   "resolve the conflict in favor of " + dc,
				guard:      dc,
				idempotent: true,
			}, nil
		},
	})
}

func (o Options) initCommand() *cli.Command {
	return o.action(actionSpec{
		name:  "init",
		usage: "Record the first state of the installation, with the active datacenter",
		flags: []cli.Flag{&cli.StringFlag{Name: "active", Usage: "datacenter that is active now (required)"}},
		before: func(c *cli.Context) error {
			if err := noArgs(c); err != nil {
				return err
			}

			if c.String("active") == "" {
				return usageErrorf("init: --active is required")
			}

			_, err := datacenterArg(c.String("active"))

			return err
		},
		plan: func(_ context.Context, c *cli.Context, s *session) (plan, error) {
			dc, _ := datacenterArg(c.String("active"))

			return plan{
				invoke: func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
					return s.datacenters.InitializeDatacenters(ctx, &dcctlv1.InitializeDatacentersRequest{
						ActiveDatacenter: "datacenters/" + dc, Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
					})
				},
				describe:   "initialize the installation with " + dc + " active",
				guard:      dc,
				idempotent: true,
			}, nil
		},
	})
}

func (o Options) initArbiterCommand() *cli.Command {
	return o.action(actionSpec{
		name:   "init-arbiter",
		usage:  "Record the state of the installation in the arbiter",
		before: noArgs,
		plan: func(_ context.Context, _ *cli.Context, s *session) (plan, error) {
			return plan{
				invoke: func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
					return s.datacenters.InitializeArbiter(ctx, &dcctlv1.InitializeArbiterRequest{
						Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
					})
				},
				describe:   "initialize the arbiter",
				guard:      "arbiter",
				idempotent: true,
			}, nil
		},
	})
}

// appsCommand is "apps sync": the only subcommand of apps.
func (o Options) appsCommand() *cli.Command {
	return o.action(actionSpec{
		name:  "apps",
		usage: "Sync the applications: sync stops them in the passive datacenters and starts them in the active one",
		before: func(c *cli.Context) error {
			if c.NArg() == 0 || c.Args().First() != "sync" {
				return usageErrorf("apps: expected the command sync")
			}

			if c.NArg() > 1 {
				return usageErrorf("apps sync takes no arguments, got %q", c.Args().Get(1))
			}

			return nil
		},
		argsUsage: "sync",
		plan: func(_ context.Context, _ *cli.Context, s *session) (plan, error) {
			return plan{
				invoke: func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, requestID string) (*dcctlv1.Operation, error) {
					return s.datacenters.SyncApps(ctx, &dcctlv1.SyncAppsRequest{
						Confirmation: conf, ValidateOnly: validateOnly, RequestId: requestID,
					})
				},
				describe:      "sync the applications with the active datacenter",
				fallbackGuard: "apps",
				idempotent:    true,
			}, nil
		},
	})
}

// operationAction returns the command of an action on the operation in its
// only argument. The request of such an action has no request_id.
func (o Options) operationAction(name, usage string, abandoning bool, flags []cli.Flag,
	call func(s *session, id string) invoke,
) *cli.Command {
	return o.action(actionSpec{
		name:      name,
		usage:     usage,
		argsUsage: "<op>",
		flags:     flags,
		before: func(c *cli.Context) error {
			if c.NArg() != 1 {
				return usageErrorf("%s: expected an operation, got %d argument(s)", name, c.NArg())
			}

			if c.IsSet(flagRequestID) {
				return usageErrorf("%s has no request_id: --%s is not for it", name, flagRequestID)
			}

			_, err := operationArg(c.Args().First())

			return err
		},
		plan: func(ctx context.Context, c *cli.Context, s *session) (plan, error) {
			id, _ := operationArg(c.Args().First())

			// The operator types the datacenter the operation is about, as for any
			// other action; its ID is the fallback.
			op, err := readOperation(ctx, s, id)
			if err != nil {
				return plan{}, err
			}

			return plan{
				invoke:        call(s, id),
				describe:      fmt.Sprintf("%s operation %s (%s %s)", name, id, op.GetType(), op.GetState()),
				guard:         shortName(op.GetTarget()),
				fallbackGuard: id,
				resuming:      !abandoning,
				abandoning:    abandoning,
			}, nil
		},
	})
}

func readOperation(ctx context.Context, s *session, id string) (*dcctlv1.Operation, error) {
	return call(ctx, s, func(ctx context.Context) (*dcctlv1.Operation, error) {
		return s.operations.GetOperation(ctx, &dcctlv1.GetOperationRequest{Name: "operations/" + id})
	})
}

func (o Options) resumeCommand() *cli.Command {
	return o.operationAction("resume", "Continue an interrupted operation", false, []cli.Flag{&cli.BoolFlag{
		Name: flagConfirmIsolation,
		Usage: "the old primary is isolated; it is sent only when the check shows it is needed, " +
			"but with --yes it is sent as given and puts the operation in FORCED mode",
	}}, func(s *session, id string) invoke {
		return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, _ string) (*dcctlv1.Operation, error) {
			return s.operations.ResumeOperation(ctx, &dcctlv1.ResumeOperationRequest{
				Name: "operations/" + id, Confirmation: conf, ValidateOnly: validateOnly,
			})
		}
	})
}

func (o Options) abandonCommand() *cli.Command {
	return o.operationAction("abandon", "Give up an interrupted operation", true, []cli.Flag{isolationFlag()}, func(s *session, id string) invoke {
		return func(ctx context.Context, validateOnly bool, conf *dcctlv1.Confirmation, _ string) (*dcctlv1.Operation, error) {
			return s.operations.AbandonOperation(ctx, &dcctlv1.AbandonOperationRequest{
				Name: "operations/" + id, Confirmation: conf, ValidateOnly: validateOnly,
			})
		}
	})
}
