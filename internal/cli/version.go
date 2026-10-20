package cli

import (
	"fmt"
	"regexp"
	"runtime/debug"

	"github.com/starknet-innovation/troupe/internal/help"
)

var versionHelp = help.Render(help.Command{
	Summary: "Print troupe's version: the release, or for a build from a checkout its commit.",
	Usage:   []string{"version"},
	Notes:   []string{"A build with uncommitted changes says \"modified\"; a dev build says so."},
})

func version(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, versionHelp)
		return 0
	}
	if len(args) != 0 {
		return usageError(e, versionHelp, "version takes no arguments")
	}
	line := "troupe " + Version()
	if devBuild {
		line += " (dev build)"
	}
	fmt.Fprintln(e.out, line)
	return 0
}

// Version is a released build's version (1.0.0), else the commit a checkout build came from
// (6ddc673, "modified" when the checkout had uncommitted changes). Go stamps a checkout build
// with a pseudo-version (v0.0.0-20261014202553-6ddc673619e1), which is no release: the commit
// says the same, readably.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return versionOf(info.Main.Version, info.Settings)
}

// release is a tagged module version: vMAJOR.MINOR.PATCH, nothing after.
var release = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func versionOf(module string, settings []debug.BuildSetting) string {
	if release.MatchString(module) {
		return module[1:]
	}
	revision, modified := "", false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if modified {
		return revision + " modified"
	}
	return revision
}
