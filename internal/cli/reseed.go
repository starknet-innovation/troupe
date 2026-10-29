//go:build dev

package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/help"
)

var reseedHelp = help.Render(help.Command{
	Summary: "Make an agent's seeded skills, scripts or SOUL.md match its kits' seeds.",
	Usage:   []string{"reseed TARGET AGENT [flags]"},
	Args: []help.Arg{
		{Name: "TARGET", Desc: "What to update: skills, scripts or soul."},
		{Name: "AGENT", Desc: "An agent in the kits root, after a finished create or reset."},
	},
	Flags: []help.Flag{
		{Name: "dry-run", Desc: "Only list the changes.", Default: "off"},
		{Name: "yes", Desc: "Apply exactly what the last --dry-run listed, without asking.", Default: "ask"},
	},
	Notes: []string{
		"Overwrites the agent's own edits to these files; copies go to ~/.local/state/live-apply/AGENT/.",
		"Skills the agent created are kept; a file it added inside a seeded skill goes with that skill.",
		"Restarts the agent gracefully when its skill list changes.",
		"Exit 3: the restart still waits on active work. Exit 4: the restart cut work off.",
	},
})

func reseedCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, reseedHelp)
		return 0
	}
	if len(args) == 0 {
		return usageError(e, reseedHelp, "reseed needs skills, scripts or soul")
	}
	kind := agent.Kind(args[0])
	if kind != agent.Skills && kind != agent.Scripts && kind != agent.Soul {
		return usageError(e, reseedHelp, "reseed %s: choose skills, scripts or soul", args[0])
	}
	var opts agent.UpdateOptions
	rest, code, ok := subcommand(e, "reseed "+string(kind), reseedHelp, args[1:], func(set *flag.FlagSet) {
		set.BoolVar(&opts.DryRun, "dry-run", false, "only list the changes")
		set.BoolVar(&opts.Yes, "yes", false, "apply exactly what the last dry run listed, without asking")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 || (opts.DryRun && opts.Yes) {
		return usageError(e, reseedHelp, "reseed %s: name one agent; use --dry-run or --yes, not both", kind)
	}
	a, err := lifecycleAgent(e, "reseed", rest[0], true)
	if err != nil {
		return fail(e, err)
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	kits, err := reg.Kits(rest[0])
	if err != nil {
		return fail(e, err)
	}
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		opts.Confirm = agent.TerminalConfirm(os.Stdin, e.out)
	}
	return a.Report(a.Update(e.ctx, agent.Seeds{Kits: kits}, kind, opts))
}
