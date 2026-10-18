//go:build dev

package cli

import (
	"flag"

	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/harvest"
)

// The dev build is troupe with a maintainer's commands and a kits root: built from the
// checkout with -tags dev, never released.

// devEntries are the maintainer commands.
var devEntries = []entry{
	{"agents", "list the agents in the kits root and show their kits and seeds", agents, false},
	{"harvest", "collect an agent's own skill edits before a reset, update or upgrade replaces them (read only)", harvestCommand, false},
	{"storage", "check a sandbox's disks and mounts against its declared storage", storageCommand, false},
	{"update", "make an agent's skills, scripts or SOUL.md match the seeds", updateCommand, false},
}

const devBuild = true

const globalUsage = "[--kits DIR] "

const createLocalUsage = "  troupe create AGENT [--force]\n"

const createLocalNote = `
troupe create AGENT creates the first VM of an agent in the kits root.
`

const kitsNote = `
The kits root is the folder holding a maintainer's agent kits: --kits, else the kits machine
setting (troupe config).
`

func kitsOption(set *flag.FlagSet) *string { return set.String("kits", "", "kits root") }

func resolveKits(flagDir string) (config.Kits, error) { return config.Resolve(flagDir) }

func harvestCommand(e env, args []string) int {
	return harvest.Run(e.ctx, e.out, e.err, e.kitsFlag, args)
}
