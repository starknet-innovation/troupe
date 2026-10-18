//go:build dev

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/starknet-innovation/troupe/internal/registry"
	"strings"
)

const agentsHelp = `Usage:
  troupe agents list [--json]
  troupe agents show AGENT [--json]
  troupe agents slack-manifest AGENT
  troupe agents materialize AGENT DIR

An agent is a folder in the kits root whose sbxenv.yaml is named after the folder and
composes a base kit followed by itself (.): the base is a relative ../ path to a Docker
Sandbox v3 workload kit, a folder holding FOLDER.yaml that says kind: workload (such as
kits: ["../../kits/coder", "."]). A Lab (named NAME-lab) is never an agent.

  list            the agents, one per line
  show            the agent's base kit, kit folders and native secrets
  slack-manifest  the base kit's Slack app manifest named after the agent, to paste into Slack
  materialize     write the agent's composed seed home (~/.hermes) into DIR; refuses a
                  path that two kits both provide
`

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
