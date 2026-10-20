package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/help"
)

func configHelp() string {
	var settings []help.Row
	for _, s := range config.Settings {
		settings = append(settings, help.Row{Name: s.Key, Desc: s.Help + " Default: " + s.Default + "."})
	}
	return help.Render(help.Command{
		Summary: "Show and change this machine's settings.",
		Usage:   []string{"config list", "config get KEY", "config set KEY VALUE", "config unset KEY"},
		Commands: []help.Row{
			{Name: "list", Desc: "Show every setting."},
			{Name: "get KEY", Desc: "Show one setting."},
			{Name: "set KEY VALUE", Desc: "Change one setting."},
			{Name: "unset KEY", Desc: "Return one setting to its default."},
		},
		Args: []help.Arg{
			{Name: "KEY", Desc: "A setting, from the Settings below."},
			{Name: "VALUE", Desc: "Its new value."},
		},
		Sections: []help.Section{{Title: "Settings", Rows: settings}},
		Notes:    []string{"Kept in " + config.MachinePath() + " (XDG_CONFIG_HOME moves it)."},
	})
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
