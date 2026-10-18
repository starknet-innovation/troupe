//go:build dev

package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/starknet-innovation/troupe/internal/agent"
)

const updateHelp = `Usage:
  troupe update skills|scripts|soul AGENT [--dry-run] [--yes]

Makes the agent's seeded skills, scripts or SOUL.md match the seeds in its kits, without a
reseed. Lists the changes and asks first; restarts the agent gracefully if its skill list
changes. Skills and scripts the agent created itself are never touched; a file it added
inside a seeded skill's folder is removed with that skill's update, a copy kept. The agent's
edits are told from older seed versions by what troupe last installed (its own receipts), so
the agent needs a finished reset (or create).

WARNING: overwrites the agent's own edits to these files; its copies are saved in
~/.local/state/live-apply/AGENT/.

  --dry-run  only list the changes
  --yes      apply exactly what the last --dry-run listed, without asking

Exit 3: the restart is still waiting on active work. Exit 4: the restart cut work off.
`

func updateCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, updateHelp)
		return 0
	}
	if len(args) == 0 {
		return usageError(e, updateHelp, "update needs skills, scripts or soul")
	}
	kind := agent.Kind(args[0])
	if kind != agent.Skills && kind != agent.Scripts && kind != agent.Soul {
		return usageError(e, updateHelp, "update %s: choose skills, scripts or soul", args[0])
	}
	var opts agent.UpdateOptions
	rest, code, ok := subcommand(e, "update "+string(kind), updateHelp, args[1:], func(set *flag.FlagSet) {
		set.BoolVar(&opts.DryRun, "dry-run", false, "only list the changes")
		set.BoolVar(&opts.Yes, "yes", false, "apply exactly what the last dry run listed, without asking")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 || (opts.DryRun && opts.Yes) {
		return usageError(e, updateHelp, "update %s: name one agent; use --dry-run or --yes, not both", kind)
	}
	a, err := lifecycleAgent(e, "update", rest[0], true)
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
