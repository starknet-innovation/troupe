//go:build !dev

package cli

import (
	"flag"

	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/help"
)

// A release build has only the end-user commands and no kits root.

var devEntries []entry

const devBuild = false

const globalUsage = ""

var globalFlags []help.Flag

func kitsOption(*flag.FlagSet) *string { return new(string) }

func resolveKits(string) (config.Kits, error) { return config.Kits{}, config.ErrNoKits }
