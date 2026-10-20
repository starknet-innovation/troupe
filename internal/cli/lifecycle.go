package cli

import (
	"errors"
	"flag"
	"fmt"
	"github.com/starknet-innovation/troupe/internal/help"
	"os"
	"path/filepath"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

var startHelp = help.Render(help.Command{
	Summary: "Start an agent and keep it starting at boot.",
	Usage:   []string{"start AGENT [flags]"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to start."}},
	Flags:   []help.Flag{{Name: "force", Desc: "Start even over the machine's resource budget.", Default: "ask"}},
	Notes: []string{
		"Waits until its gateway runs, with Slack connected when its kit uses Slack.",
		"Asks for sudo. An agent larger than the whole machine is refused.",
	},
})

var stopHelp = help.Render(help.Command{
	Summary: "Stop an agent once its current work finishes, and keep it stopped.",
	Usage:   []string{"stop AGENT [flags]"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to stop."}},
	Flags:   []help.Flag{{Name: "cap-minutes", Value: "N", Desc: "Minutes to wait for its current work.", Default: "60"}},
	Notes: []string{
		"New work is refused while it waits. If the work does not finish in time, the agent keeps running.",
		"Asks for sudo.",
	},
})

var drainHelp = help.Render(help.Command{
	Summary: "Let an agent finish its current work before a stop (stop calls it).",
	Usage:   []string{"drain AGENT [flags]"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to drain."}},
	Flags: []help.Flag{
		{Name: "cap-minutes", Value: "N", Desc: "Minutes to wait for its current work.", Default: "60"},
		{Name: "cancel", Desc: "Clear a drain left behind, so the agent accepts work again.", Default: "off"},
	},
	Notes: []string{"The gateway stays drained until it stops. A stopped VM is skipped, not booted."},
})

var gatewayHelp = help.Render(help.Command{
	Summary:  "Stop an agent's gateway inside its VM (the gateway unit's ExecStop).",
	Usage:    []string{"gateway stop AGENT"},
	Commands: []help.Row{{Name: "stop AGENT", Desc: "Stop the gateway: SIGTERM, then wait up to 60 seconds."}},
	Args:     []help.Arg{{Name: "AGENT", Desc: "The agent whose gateway stops."}},
})

// lifecycleAgent builds the agent a lifecycle command acts on. Start and stop need the kits
// root (the agent must be registered); drain and gateway stop run from units and other
// procedures, so they take any well-formed sandbox name.
func lifecycleAgent(e env, command, name string, registered bool) (*agent.Agent, error) {
	if !sandboxName.MatchString(name) {
		return nil, fmt.Errorf("malformed agent name: %s", name)
	}
	machine, err := config.LoadMachine()
	if err != nil {
		// Drain and gateway stop run from units and recovery; a broken settings file must not
		// stop them, and they use no setting but the clock's zone.
		if registered {
			return nil, err
		}
		machine = config.Machine{}
	}
	printer := progress.New(label(name), machine.Location())
	printer.Out, printer.Err = e.out, e.err
	a := &agent.Agent{
		Name: name, Command: command, Printer: printer,
		Paths:    guard.Default(),
		Terminal: os.Stdin,
		Timing:   agent.DefaultTiming(),
	}
	a.Runner = sbx.Runner{Paths: a.Paths, Log: agent.LogFile(name, command+".log")}
	if registered {
		reg, _, err := e.registry()
		if err != nil {
			return nil, err
		}
		base, err := reg.Base(name)
		if err != nil {
			return nil, err
		}
		printer.Agent = displayName(reg, name)
		// Slack settings: a local agent's base kit's, or a published agent's own.
		if _, err := os.Stat(filepath.Join(base, "slack.env")); err == nil {
			a.Slack = true
		}
		if folder, err := reg.Folder(name); err == nil && registry.IsPublished(base) {
			_, err := os.Stat(filepath.Join(folder, "slack.env"))
			a.Slack = err == nil
		}
	}
	return a, nil
}

func startCommand(e env, args []string) int {
	force := false
	rest, code, ok := subcommand(e, "start", startHelp, args, func(set *flag.FlagSet) {
		set.BoolVar(&force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, startHelp, "start: name one agent")
	}
	a, err := lifecycleAgent(e, "start", rest[0], true)
	if err != nil {
		return fail(e, err)
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	kit, err := agent.ReadKit(reg, rest[0])
	if err != nil {
		return fail(e, err)
	}
	// The budget before sudo: an agent already running counts once, at its own size.
	if err := checkBudget(e, a.Runner, a.Printer, budget.Want{Name: rest[0], CPUs: kit.CPUs, MemoryGiB: kit.MemoryGiB}, askBudget(force)); err != nil {
		return a.Report(budgetFailure(err))
	}
	return a.Report(a.Start(e.ctx))
}

func stopCommand(e env, args []string) int {
	capMinutes := 60
	rest, code, ok := subcommand(e, "stop", stopHelp, args, func(set *flag.FlagSet) {
		set.IntVar(&capMinutes, "cap-minutes", 60, "longest wait for current work, in minutes")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 || capMinutes < 1 {
		return usageError(e, stopHelp, "stop: name one agent; the cap is a whole number of minutes")
	}
	a, err := lifecycleAgent(e, "stop", rest[0], true)
	if err != nil {
		return fail(e, err)
	}
	return a.Report(a.Stop(e.ctx, capMinutes))
}

func drainCommand(e env, args []string) int {
	capMinutes, cancel := 60, false
	rest, code, ok := subcommand(e, "drain", drainHelp, args, func(set *flag.FlagSet) {
		set.IntVar(&capMinutes, "cap-minutes", 60, "longest wait for current work, in minutes")
		set.BoolVar(&cancel, "cancel", false, "clear a drain so the agent accepts work again")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 || capMinutes < 1 {
		return usageError(e, drainHelp, "drain: name one agent; the cap is a whole number of minutes")
	}
	a, err := lifecycleAgent(e, "drain", rest[0], false)
	if err != nil {
		return usageError(e, drainHelp, "%v", err)
	}
	if cancel {
		err = a.CancelDrain(e.ctx)
	} else {
		err = a.Drain(e.ctx, capMinutes)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, agent.ErrInterrupted):
		return 130
	case errors.Is(err, agent.ErrDrainFailed):
		return 1
	}
	return a.Report(err)
}

func gatewayCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, gatewayHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "stop" {
		return usageError(e, gatewayHelp, "gateway needs the stop subcommand")
	}
	rest, code, ok := subcommand(e, "gateway stop", gatewayHelp, args[1:], nil)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, gatewayHelp, "gateway stop: name one agent")
	}
	a, err := lifecycleAgent(e, "gateway", rest[0], false)
	if err != nil {
		return usageError(e, gatewayHelp, "%v", err)
	}
	return a.Report(a.GatewayStop(e.ctx))
}
