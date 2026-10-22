// Package cli is troupe's command line: global options, dispatch and help.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/published"
	"github.com/starknet-innovation/troupe/internal/registry"
)

// usage is troupe's own help: the visible commands, one line each.
func usage() string {
	var commands []help.Row
	for _, c := range commandList {
		if !c.hidden {
			commands = append(commands, help.Row{Name: c.name, Desc: strings.ToUpper(c.summary[:1]) + c.summary[1:] + "."})
		}
	}
	return help.Render(help.Command{
		Summary:  "Manage Hermes agents, each in its own Docker Sandbox.",
		Usage:    []string{globalUsage + "COMMAND [arguments]"},
		Commands: commands,
		Flags:    globalFlags,
		Notes:    []string{"Run \"troupe COMMAND --help\" for a command's help."},
	})
}

// env is what every command gets: streams, the kits root flag and a cancelable context.
type env struct {
	ctx      context.Context
	out, err io.Writer
	kitsFlag string
}

// kits resolves the kits root lazily, so commands that need none (version, units) work anywhere.
// A release build has none.
func (e env) kits() (config.Kits, error) {
	return resolveKits(e.kitsFlag)
}

// registry is every agent: the local kits root's, if one is named, and the published ones
// troupe created. No kits root is normal (an end user has none).
func (e env) registry() (registry.Registry, config.Kits, error) {
	kits, err := e.kits()
	if errors.Is(err, config.ErrNoKits) {
		err = nil
	}
	reg := registry.Registry{Root: kits.Root, Data: config.DataDir()}
	if machine, loadErr := config.LoadMachine(); loadErr == nil {
		reg.Boundary = [2]string{machine.BoundaryPublic, machine.BoundaryPrivate}
	}
	return reg, kits, err
}

// hostLocation is the machine's configured zone, else the host's.
func hostLocation(e env) *time.Location {
	machine, err := config.LoadMachine()
	if err != nil {
		return time.Local
	}
	return machine.Location()
}

type command func(e env, args []string) int

// entry is one top-level command. Hidden commands run like any other but are left out of
// help: units and other troupe commands call them, users do not.
type entry struct {
	name, summary string
	run           command
	hidden        bool
}

// commandList is every command of this build, sorted: the release's, plus the dev build's
// maintainer commands (devEntries).
var commandList = sorted(append([]entry{
	{"chat", "talk to a running agent from this terminal", chatCommand, false},
	{"config", "show and change this machine's settings", configCommand, false},
	{"create", "create a new agent from a published kit and start it", createCommand, false},
	{"doctor", "check whether this machine can host agents", doctorCommand, false},
	{"delete", "remove an agent entirely: its VM, keys and records; the name is free again", deleteCommand, false},
	{"drain", "let an agent finish its current work before a stop (called by stop)", drainCommand, true},
	{"gateway", "stop an agent's gateway inside its VM (the gateway unit's ExecStop)", gatewayCommand, true},
	{"guard", "probe sandboxd before using sbx", guardCommand, false},
	{"reset", "factory reset: start an agent fresh from its kit, keeping it and its keys", resetCommand, false},
	{"resize", "change an agent's CPUs, memory or disk, keeping everything it has", resizeCommand, false},
	{"restart", "restart an agent gracefully and wait until it is back", restartCommand, false},
	{"secret", "store an agent's API keys and give its sandbox access", secretCommand, false},
	{"start", "start an agent and keep it starting at boot", startCommand, false},
	{"stop", "let an agent finish its work, then stop it and keep it stopped", stopCommand, false},
	{"units", "render the systemd unit templates (the installer installs them)", units, true},
	{"upgrade", "move an agent onto the newest version of its kit, keeping everything it has", upgradeCommand, false},
	{"version", "print troupe's version", version, false},
}, devEntries...))

func sorted(entries []entry) []entry {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries
}

// commands finds a command by name, hidden or not.
var commands = func() map[string]command {
	m := map[string]command{}
	for _, c := range commandList {
		m[c.name] = c.run
	}
	return m
}()

// Main runs troupe with args (without the program name) and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	global := flag.NewFlagSet("troupe", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	kitsFlag := kitsOption(global)
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage())
			return 0
		}
		fmt.Fprintf(stderr, "troupe: %v\n\n%s", err, usage())
		return 2
	}
	rest := global.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage())
		return 2
	}
	if rest[0] == "help" {
		fmt.Fprint(stdout, usage())
		return 0
	}
	run, ok := commands[rest[0]]
	if !ok {
		fmt.Fprintf(stderr, "troupe: unknown command %q\n\n%s", rest[0], usage())
		return 2
	}
	return run(env{ctx: ctx, out: stdout, err: stderr, kitsFlag: *kitsFlag}, rest[1:])
}

// subcommand parses a command's flags; it returns ok=false with the exit code to use when
// parsing ends the command (help shown, or a usage error).
func subcommand(e env, name, help string, args []string, define func(*flag.FlagSet)) (rest []string, code int, ok bool) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	if define != nil {
		define(set)
	}
	if err := documented(name, help, set); err != nil {
		fmt.Fprintf(e.err, "troupe: %v\n", err)
		return nil, 1, false
	}
	// Flags may follow positional arguments ("show AGENT --json"): Parse stops at the first
	// positional, so take it and parse the rest again.
	for {
		if err := set.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(e.out, help)
				return nil, 0, false
			}
			fmt.Fprintf(e.err, "troupe %s: %v\n\n%s", name, err, help)
			return nil, 2, false
		}
		args = set.Args()
		if len(args) == 0 {
			return rest, 0, true
		}
		rest, args = append(rest, args[0]), args[1:]
	}
}

// flagHelp is the help that documents a command's flags when the help it shows does not: create
// shows a short help, and each type's help documents the flags.
var flagHelp = map[string]func() string{"create": func() string {
	var all string
	for _, t := range published.Types {
		all += createTypeHelp(t)
	}
	return all
}}

// documented refuses a flag the command's help does not describe on a line of its own, so help
// never drifts from the flags.
func documented(name, help string, set *flag.FlagSet) error {
	if more, ok := flagHelp[strings.Fields(name)[0]]; ok {
		help = more()
	}
	var missing []string
	set.VisitAll(func(f *flag.Flag) {
		if !regexp.MustCompile(`(?m)^  --` + regexp.QuoteMeta(f.Name) + `( |$)`).MatchString(help) {
			missing = append(missing, "--"+f.Name)
		}
	})
	if len(missing) > 0 {
		return fmt.Errorf("internal error: %s's help does not describe %s", name, strings.Join(missing, ", "))
	}
	return nil
}

// helpRequested reports whether a command was called as "COMMAND --help" or "COMMAND -h".
func helpRequested(args []string) bool {
	return len(args) > 0 && (args[0] == "--help" || args[0] == "-help" || args[0] == "-h")
}

// usageError prints the command's help to stderr and returns the usage exit code.
func usageError(e env, help, format string, a ...any) int {
	fmt.Fprintf(e.err, "troupe: "+format+"\n\n%s", append(a, help)...)
	return 2
}

// fail prints an error and returns exit code 1, or 2 for an agent the kits root does not hold.
func fail(e env, err error) int {
	fmt.Fprintf(e.err, "troupe: %v\n", err)
	var unknown registry.UnknownAgentError
	if errors.As(err, &unknown) {
		return 2
	}
	return 1
}

// sandboxName is the shape Docker Sandbox accepts; it also keeps names out of other paths.
var sandboxName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// label is how progress lines name an agent: capitalized.
func label(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
