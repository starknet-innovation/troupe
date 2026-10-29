//go:build dev

// Package harvest is "troupe harvest", a maintainer command of troupe's dev build: it collects
// an agent's own skill edits for review before a reset erases them.
package harvest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// env is what every command gets: streams, the kits root flag, a cancelable context and the
// tools it calls (tests replace them).
type env struct {
	ctx      context.Context
	out, err io.Writer
	kitsFlag string
	tools    tools
}

// tools are the programs and places a command touches.
type tools struct {
	runner sbx.Runner
	// scratch holds the harvest collections.
	scratch string
	// state holds troupe's recreate and migrate receipts; updates its update receipts.
	state, updates string
	now            func() time.Time
}

func defaultTools() tools {
	home, _ := os.UserHomeDir()
	paths := guard.Default()
	return tools{
		runner:  sbx.Runner{Paths: paths, Log: logFile("harvest.log")},
		scratch: home + "/.cache/hermes-skill-harvest",
		state:   home + "/.local/state/sandbox-replacements",
		updates: home + "/.local/state/live-apply",
		now:     time.Now,
	}
}

// logFile is a private log under troupe's state folder.
func logFile(name string) string {
	home, _ := os.UserHomeDir()
	return home + "/.local/state/troupe/" + name
}

// Run runs "troupe harvest" with args (those after the command name) on the kits root kitsFlag.
func Run(ctx context.Context, stdout, stderr io.Writer, kitsFlag string, args []string) int {
	return harvestCommand(env{ctx: ctx, out: stdout, err: stderr, kitsFlag: kitsFlag, tools: defaultTools()}, args)
}

// registry is every agent troupe knows: the kits root's, if one is named, and the ones it
// created from a published kit (whose base is the pinned kit's files in troupe's data folder).
func (e env) registry() (registry.Registry, error) {
	kits, err := config.Resolve(e.kitsFlag)
	if errors.Is(err, config.ErrNoKits) {
		err = nil
	}
	return registry.Registry{Root: kits.Root, Data: config.DataDir()}, err
}

// printer prints progress lines for one subject (an agent, a demo).
func (e env) printer(subject string) *progress.Printer {
	p := progress.New(subject)
	p.Out, p.Err = e.out, e.err
	if e.tools.now != nil {
		p.Now = e.tools.now
	}
	return p
}

// helpRequested reports whether a command was called with --help or -h anywhere.
func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-help" || a == "-h" {
			return true
		}
	}
	return false
}

// usageError prints the command's help to stderr and returns the usage exit code.
func usageError(e env, help, format string, a ...any) int {
	fmt.Fprintf(e.err, "troupe: "+format+"\n\n%s", append(a, help)...)
	return 2
}

func fail(e env, err error) int {
	fmt.Fprintf(e.err, "troupe: %v\n", err)
	return 1
}

// label is how progress lines name a sandbox or an image: capitalized.
func label(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
