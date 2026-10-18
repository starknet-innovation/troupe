package cli

const restartHelp = `Usage:
  troupe restart AGENT

Restarts the agent gracefully: current work finishes first, new messages wait. Waits up to
5 minutes for it to come back; if work is still running then, the next restart confirms it.

Exit 3: still waiting on active work. Exit 4: the restart cut work off.
`

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
