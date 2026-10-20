package cli

import (
	"flag"
	"fmt"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/progress"
)

var guardHelp = help.Render(help.Command{
	Summary: "Probe sandboxd before using sbx, or check an agent is not suspended.",
	Usage:   []string{"guard probe [flags]", "guard check NAME"},
	Commands: []help.Row{
		{Name: "probe", Desc: "Pass only when systemd owns the running sandboxd and it answers."},
		{Name: "check NAME", Desc: "Exit 1 while another tool holds a copy of NAME (gateway units' ExecCondition)."},
	},
	Args:  []help.Arg{{Name: "NAME", Desc: "A sandbox name."}},
	Flags: []help.Flag{{Name: "wait", Value: "DURATION", Desc: "Retry until it passes, for up to DURATION.", Default: "0, no retry", Only: "probe"}},
	Notes: []string{"Run probe before any sbx command of your own. Both print nothing when they pass."},
})

func guardCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, guardHelp)
		return 0
	}
	if len(args) == 0 {
		return usageError(e, guardHelp, "guard needs a subcommand")
	}
	var wait time.Duration
	action := args[0]
	help := guardHelp
	rest, code, ok := subcommand(e, "guard "+action, help, args[1:], func(set *flag.FlagSet) {
		if action == "probe" {
			set.DurationVar(&wait, "wait", 0, "retry until the probe passes or this much time passes")
		}
	})
	if !ok {
		return code
	}
	// Progress lines name the host here; the machine settings set the clock's zone.
	printer := progress.New("Host", hostLocation(e))
	printer.Out, printer.Err = e.out, e.err
	paths := guard.Default()
	switch {
	case action == "probe" && len(rest) == 0:
		if err := guard.ProbeWait(e.ctx, paths, wait); err != nil {
			printer.Print("guard probe", progress.Failed, err.Error())
			return 1
		}
		return 0
	case action == "check" && len(rest) == 1:
		if !sandboxName.MatchString(rest[0]) {
			return usageError(e, help, "malformed sandbox name: %s", rest[0])
		}
		if guard.Suspended(paths, rest[0]) {
			printer.Print("guard check", progress.Failed, rest[0]+" is suspended for its disposable copy")
			return 1
		}
		return 0
	}
	return usageError(e, help, "guard %s: wrong arguments", action)
}
