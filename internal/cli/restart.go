package cli

import "github.com/starknet-innovation/troupe/internal/help"

var restartHelp = help.Render(help.Command{
	Summary: "Restart an agent gracefully and wait until it is back.",
	Usage:   []string{"restart AGENT"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to restart."}},
	Notes: []string{
		"Current work finishes first; new messages wait. Waits up to 5 minutes for it to come back.",
		"Exit 3: still waiting on active work (the next restart confirms it). Exit 4: work was cut off.",
	},
})

func restartCommand(e env, args []string) int {
	rest, code, ok := subcommand(e, "restart", restartHelp, args, nil)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, restartHelp, "restart: name one agent")
	}
	a, err := lifecycleAgent(e, "restart", rest[0], true)
	if err != nil {
		return fail(e, err)
	}
	return a.Report(a.Restart(e.ctx))
}
