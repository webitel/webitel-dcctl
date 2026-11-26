package cli

import (
	"errors"
	"flag"
	"io"
	"slices"

	"github.com/urfave/cli/v2"
)

// Commands returns the CLI commands of webitel-dcctl. They call the API of a
// running dcctl; serve and arbiter are not among them.
func Commands(o Options) []*cli.Command {
	o = o.withDefaults()

	return []*cli.Command{
		statusCommand(o),
		datacentersCommand(o),
		verifyCommand(o),
		opsCommand(o),
		journalCommand(o),
	}
}

// leaf makes cmd take its flags anywhere among its arguments, as in
// "switch dc_b --dry-run": urfave/cli stops reading flags at the first
// argument. The flags stay in cmd.Flags, which the help lists.
func leaf(cmd *cli.Command) *cli.Command {
	flags, before, action := cmd.Flags, cmd.Before, cmd.Action

	cmd.SkipFlagParsing = true
	cmd.Before = nil
	cmd.Action = func(c *cli.Context) error {
		set := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
		set.SetOutput(io.Discard)

		for _, f := range flags {
			if err := f.Apply(set); err != nil {
				return err
			}
		}

		args, err := parseInterspersed(set, c.Args().Slice())

		switch {
		case errors.Is(err, flag.ErrHelp):
			cli.HelpPrinter(c.App.Writer, cli.CommandHelpTemplate, cmd)

			return nil
		case err != nil:
			return usageErrorf("%v", err)
		}

		syncAliases(set, flags)

		if err := set.Parse(append([]string{"--"}, args...)); err != nil {
			return usageErrorf("%v", err)
		}

		cc := cli.NewContext(c.App, set, c)
		cc.Command = cmd

		if before != nil {
			if err := before(cc); err != nil {
				return err
			}
		}

		return action(cc)
	}

	return cmd
}

// parseInterspersed parses the flags in args wherever they are and returns
// the other arguments in their order. After "--" every argument is one of
// those.
func parseInterspersed(set *flag.FlagSet, args []string) ([]string, error) {
	var rest []string

	for {
		if len(args) > 0 && args[0] == "--" {
			return append(rest, args[1:]...), nil
		}

		if err := set.Parse(args); err != nil {
			return nil, err
		}

		args = set.Args()
		if len(args) == 0 {
			return rest, nil
		}

		rest = append(rest, args[0])
		args = args[1:]
	}
}

// syncAliases gives every name of a flag the value of the name that was set,
// as urfave/cli does after it parses.
func syncAliases(set *flag.FlagSet, flags []cli.Flag) {
	given := make(map[string]bool)

	set.Visit(func(f *flag.Flag) { given[f.Name] = true })

	for _, f := range flags {
		names := f.Names()

		i := slices.IndexFunc(names, func(n string) bool { return given[n] })
		if i < 0 {
			continue
		}

		val := set.Lookup(names[i]).Value.String()

		for _, n := range names {
			if n != names[i] {
				_ = set.Set(n, val)
			}
		}
	}
}
