//go:build !dev

package cli

import (
	"flag"

	"github.com/starknet-innovation/troupe/internal/config"
)

// A release build has only the end-user commands and no kits root.

var devEntries []entry

const devBuild = false

const globalUsage = ""

const createLocalUsage = ""

const createLocalNote = ""

const kitsNote = ""

func kitsOption(*flag.FlagSet) *string { return new(string) }

func resolveKits(string) (config.Kits, error) { return config.Kits{}, config.ErrNoKits }
