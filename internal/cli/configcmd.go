package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/starknet-innovation/troupe/internal/config"
)

func configHelp() string {
	var b strings.Builder
	b.WriteString(`Usage:
  troupe config list
  troupe config get KEY
  troupe config set KEY VALUE
  troupe config unset KEY

This machine's settings, kept in ` + config.MachinePath() + `
(XDG_CONFIG_HOME moves it). They belong to the machine, not the kits, so the same kits
can be used on machines of different sizes.

Settings:
`)
	for _, s := range config.Settings {
		fmt.Fprintf(&b, "  %-19s %s\n", s.Key, s.Help)
	}
	return b.String()
}

func configCommand(e env, args []string) int {
	help := configHelp()
	if helpRequested(args) {
		fmt.Fprint(e.out, help)
		return 0
	}
	if len(args) == 0 {
		return usageError(e, help, "config needs a subcommand")
	}
	action := args[0]
	rest, code, ok := subcommand(e, "config "+action, help, args[1:], nil)
	if !ok {
		return code
	}
	switch {
	case action == "list" && len(rest) == 0:
		table := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
		for _, s := range config.Settings {
			value, set, err := config.Get(s.Key)
			if err != nil {
				return fail(e, err)
			}
			source := config.MachinePath()
			if !set {
				value, source = "(unset)", "default"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\n", s.Key, value, source)
		}
		table.Flush()
		return 0
	case action == "get" && len(rest) == 1:
		value, set, err := config.Get(rest[0])
		if err != nil {
			return fail(e, err)
		}
		if set {
			fmt.Fprintln(e.out, value)
		}
		return 0
	case action == "set" && len(rest) == 2:
		if err := config.Set(rest[0], rest[1]); err != nil {
			return fail(e, err)
		}
		return 0
	case action == "unset" && len(rest) == 1:
		if err := config.Unset(rest[0]); err != nil {
			return fail(e, err)
		}
		return 0
	}
	return usageError(e, help, "config %s: wrong arguments", action)
}
