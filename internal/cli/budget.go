package cli

import (
	"errors"
	"fmt"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// budgetPaths are where the budget reads this machine's figures; tests replace them.
var budgetPaths = func() budget.Paths {
	return budget.Paths{Meminfo: "/proc/meminfo", Proc: "/proc", Sandboxd: guard.Default().Sandboxd}
}

// budgetMeasure reads this machine's figures (budget.Measure); tests give fixed cores.
var budgetMeasure = budget.Measure

// unitUp is whether an agent's gateway unit is up; tests replace it.
var unitUp = agent.UnitUp

// sandboxes lists every sandbox sbx has with the sizes sbx ls reports. A sandbox counts as
// running while its VM runs or its gateway unit is up (it boots the VM again within seconds). A
// running sandbox whose size cannot be read is refused: the budget never counts it as nothing.
func sandboxes(e env, runner sbx.Runner) ([]budget.Sandbox, error) {
	records, err := runner.Inventory(e.ctx)
	if err != nil {
		return nil, err
	}
	var found []budget.Sandbox
	for name, r := range records {
		s := budget.Sandbox{Name: name, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, Running: r.Status == "running" || unitUp(name)}
		if s.Running && (s.CPUs < 1 || s.MemoryMiB < 1) {
			return nil, fmt.Errorf("the size of the running sandbox %s could not be read, so the budget cannot be checked", name)
		}
		found = append(found, s)
	}
	return found, nil
}

// checkBudget refuses what cannot fit this machine at all, and over budget asks the operator to
// confirm (forcedBudget confirms; reportBudget, for a check that changes nothing, refuses with the
// figures instead of asking). Figures it cannot read are noted, not refused.
func checkBudget(e env, runner sbx.Runner, printer *progress.Printer, w budget.Want, mode budgetMode) error {
	listed, err := sandboxes(e, runner)
	if err != nil {
		return err
	}
	result := budget.Check(budgetMeasure(budgetPaths()), listed, w)
	for _, note := range result.Notes {
		printer.Print("budget", progress.Info, note)
	}
	if result.Impossible != "" {
		return errors.New(result.Impossible)
	}
	if len(result.Over) == 0 {
		return nil
	}
	for _, line := range result.Over {
		printer.Print("budget", progress.Info, "Over budget. "+line.String())
	}
	switch mode {
	case forcedBudget:
		printer.Print("budget", progress.Info, "Continuing anyway (--force)")
		return nil
	case reportBudget:
		return budget.ErrOver
	}
	answer, err := readLine(e.ctx, fmt.Sprintf("Running %s at this size overcommits this machine. Continue anyway? [y/N] ", w.Name))
	if errors.Is(err, secret.ErrInterrupted) {
		return agent.ErrInterrupted
	}
	if err != nil || (answer != "y" && answer != "yes") {
		return budget.ErrDeclined
	}
	return nil
}

// budgetFailure is a budget refusal as the command reports it; an interrupt stays one.
func budgetFailure(err error) error {
	if errors.Is(err, agent.ErrInterrupted) {
		return err
	}
	return agent.Failure{Step: "budget", Class: err.Error()}
}
