//go:build dev

package cli

import (
	"errors"
	"flag"
	"fmt"

	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

var budgetHelp = help.Render(help.Command{
	Summary:  "Check this machine has room for a new sandbox before another tool creates it.",
	Usage:    []string{"budget check --name NAME --cpu N --memory GiB --disk GiB"},
	Commands: []help.Row{{Name: "check", Desc: "Whether it fits, by the exit code below."}},
	Flags: []help.Flag{
		{Name: "name", Value: "NAME", Desc: "The sandbox, as the messages name it.", Required: true},
		{Name: "cpu", Value: "N", Desc: "Its CPUs.", Required: true},
		{Name: "memory", Value: "GiB", Desc: "Its memory in GiB.", Required: true},
		{Name: "disk", Value: "GiB", Desc: "The disk it adds in GiB: its root and Docker disks.", Required: true},
	},
	Notes: []string{
		"Exit 0: it fits. Exit 3: over budget, by the amounts shown; the caller decides. Exit 1: it can never",
		"fit (larger than the machine), or the check could not run.",
		"Counts every running sandbox, troupe's or not, as create does. Never asks; changes no sandbox.",
	},
})

func budgetCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, budgetHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "check" {
		return usageError(e, budgetHelp, "budget needs the check subcommand")
	}
	var w budget.Want
	rest, code, ok := subcommand(e, "budget check", budgetHelp, args[1:], func(set *flag.FlagSet) {
		set.StringVar(&w.Name, "name", "", "")
		set.IntVar(&w.CPUs, "cpu", 0, "")
		set.IntVar(&w.MemoryGiB, "memory", 0, "")
		set.IntVar(&w.DiskGrowthGiB, "disk", -1, "") // required: 0 is a size, so unset is -1
	})
	if !ok {
		return code
	}
	if len(rest) != 0 || !sandboxName.MatchString(w.Name) || w.CPUs < 1 || w.MemoryGiB < 1 || w.DiskGrowthGiB < 0 {
		return usageError(e, budgetHelp, "budget check needs --name, and --cpu, --memory and --disk as whole numbers")
	}
	printer := progress.New(label(w.Name), hostLocation(e))
	printer.Out, printer.Err = e.out, e.err
	return budgetCheck(e, sbx.Runner{Paths: guard.Default(), Log: stateFile(w.Name, "budget.log")}, printer, w)
}

// budgetCheck reports whether a new sandbox of w's size fits, as create's check does for a new
// agent, without asking: 0 fits, 3 over budget (the caller decides), 1 never fits or unknown.
func budgetCheck(e env, runner sbx.Runner, printer *progress.Printer, w budget.Want) int {
	w.NewVM = true
	err := checkBudget(e, runner, printer, w, reportBudget)
	switch {
	case err == nil:
		printer.Print("budget", progress.OK, "Fits this machine")
		return 0
	case errors.Is(err, budget.ErrOver):
		printer.Print("budget", progress.Info, "Over this machine's budget")
		return 3
	default:
		printer.Print("budget", progress.Failed, err.Error())
	}
	return 1
}
