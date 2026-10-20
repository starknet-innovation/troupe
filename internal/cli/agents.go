//go:build dev

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/registry"
	"strings"
)

var agentsHelp = help.Render(help.Command{
	Summary: "List the agents in the kits root and show their kits and seeds.",
	Usage: []string{"agents list [flags]", "agents show AGENT [flags]", "agents slack-manifest AGENT",
		"agents materialize AGENT DIR"},
	Commands: []help.Row{
		{Name: "list", Desc: "The agents, one per line."},
		{Name: "show AGENT", Desc: "The agent's base kit, kit folders and native secrets."},
		{Name: "slack-manifest AGENT", Desc: "The base kit's Slack app manifest named after the agent."},
		{Name: "materialize AGENT DIR", Desc: "Write the agent's composed seed home (~/.hermes) into DIR."},
	},
	Args: []help.Arg{
		{Name: "AGENT", Desc: "An agent in the kits root."},
		{Name: "DIR", Desc: "The folder to write into."},
	},
	Flags: []help.Flag{{Name: "json", Desc: "Print JSON.", Default: "text", Only: "list and show"}},
	Notes: []string{"A Lab (named NAME-lab) is never an agent."},
})

func agents(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, agentsHelp)
		return 0
	}
	if len(args) == 0 {
		return usageError(e, agentsHelp, "agents needs a subcommand")
	}
	var asJSON bool
	action := args[0]
	rest, code, ok := subcommand(e, "agents "+action, agentsHelp, args[1:], func(set *flag.FlagSet) {
		if action == "list" || action == "show" {
			set.BoolVar(&asJSON, "json", false, "print JSON")
		}
	})
	if !ok {
		return code
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	switch {
	case action == "list" && len(rest) == 0:
		names, err := reg.Names()
		if err != nil {
			return fail(e, err)
		}
		if asJSON {
			return printJSON(e, names)
		}
		for _, name := range names {
			fmt.Fprintln(e.out, name)
		}
		return 0
	case action == "show" && len(rest) == 1:
		kits, err := reg.Kits(rest[0])
		if err != nil {
			return fail(e, err)
		}
		secrets, err := reg.Secrets(rest[0])
		if err != nil {
			return fail(e, err)
		}
		if asJSON {
			return printJSON(e, map[string]any{"name": rest[0], "base": kits[0], "kits": kits, "secrets": secrets})
		}
		fmt.Fprintf(e.out, "name: %s\nbase: %s\nkits: %s\nsecrets: %s\n",
			rest[0], kits[0], strings.Join(kits, ", "), strings.Join(secrets, " "))
		return 0
	case action == "slack-manifest" && len(rest) == 1:
		manifest, err := slackManifestFor(reg, rest[0])
		if err != nil {
			return fail(e, err)
		}
		fmt.Fprintf(e.out, "%s\n", manifest)
		return 0
	case action == "materialize" && len(rest) == 2:
		if err := reg.Materialize(rest[0], rest[1]); err != nil {
			return fail(e, err)
		}
		return 0
	}
	return usageError(e, agentsHelp, "agents %s: wrong arguments", action)
}

func printJSON(e env, value any) int {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.out, "%s\n", data)
	return 0
}

// slackManifestFor is the agent's Slack app manifest, named as people see it: a published agent's
// display name, else its ID capitalized.
func slackManifestFor(reg registry.Registry, name string) ([]byte, error) {
	base, err := reg.Base(name)
	if err != nil {
		return nil, err
	}
	if registry.IsPublished(base) {
		return registry.SlackManifestIn(base, displayName(reg, name))
	}
	return reg.SlackManifest(name)
}
