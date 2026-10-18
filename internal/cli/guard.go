package cli

import (
	"flag"
	"fmt"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
)

const guardHelp = `Usage:
  troupe guard probe [--wait DURATION]
  troupe guard check NAME

  probe  passes only when systemd owns the running sandboxd and it answers: the unit's main
         PID, the pidfile and the only holder of the control socket agree, and the
         Docker-compatible list call succeeds. Run it before anything that calls sbx
         yourself; sbx would otherwise spawn an unmanaged daemon. --wait retries until it
         passes (the sbx-daemon unit waits for readiness this way).
  check  exits 1 while NAME is suspended by an unresolved phase record (another tool on this
         host holds a disposable copy of it). Gateway units run it as ExecCondition, so a
         suspended agent is skipped, not failed; run it before execing into an agent yourself.

Both print nothing when they pass.
`

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
