package cli

import (
	"errors"
	"flag"
	"fmt"
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

const startHelp = `Usage:
  troupe start AGENT [--force]

Starts the agent and keeps it starting at boot: enables and starts its gateway unit (whose
attached sbx exec boots the VM) and waits until the gateway runs, with Slack connected when
the agent's kit uses Slack. Asks for sudo once.

Before any work it checks the machine's budget: every running sandbox's memory and CPUs, with
this one's, against what the machine has. Over budget, it shows by how much and asks you to
confirm; --force confirms. One agent larger than the machine is refused.
`

const stopHelp = `Usage:
  troupe stop AGENT [--cap-minutes N]

Stops the agent and keeps it stopped across reboots. Its current work finishes first: the
gateway refuses new work and waits up to N minutes (default 60) for running work. Then the
gateway unit is stopped and disabled, and the VM is stopped and its memory freed. If the
current work does not finish in time, the agent keeps running untouched. Asks for sudo once.
`

const drainHelp = `Usage:
  troupe drain AGENT [--cap-minutes N]
  troupe drain AGENT --cancel

Called by stop and other procedures before they stop a gateway. Makes the gateway refuse new
work and waits until its current work is done (up to N minutes, default 60); the gateway
stays drained until it is stopped. --cancel clears a drain left behind, so the agent accepts
work again. Skips a stopped VM rather than booting it.
`

const gatewayHelp = `Usage:
  troupe gateway stop AGENT

The gateway unit's ExecStop. Stopping the host-side sbx exec client does not signal the
gateway inside the VM, so this writes Hermes's planned-stop marker, sends it SIGTERM and
waits up to 60 seconds for it to exit, then clears any drain.
`

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
