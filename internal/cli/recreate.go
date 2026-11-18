package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/published"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// createHelp is create's help for any type: troupe knows no type itself, so the sizes and the
// questions are the kit's (createTypeHelp shows one type's).
var createHelp = renderCreateHelp("TYPE", "", nil, nil)

// anyTypeNotes say where the types are, since troupe lists none.
var anyTypeNotes = []string{
	"TYPE is a kit in this machine's registry (troupe config get registry), such as hermes.",
	"troupe create TYPE --help shows that type's sizes and what it asks for, read from the registry.",
}

// renderCreateHelp is create's help: for one type when kit is its label, with its sizes as the
// defaults, what create asks for and the kit's own description (summary); otherwise for any type.
func renderCreateHelp(typ, summary string, kit *published.Kit, notes []string) string {
	cpus, memory, root, dockerDisk := "the kit's", "the kit's", "the kit's", "the kit's"
	c := help.Command{
		Summary: "Create a new agent from the latest published kit of its type, and start it.",
		Usage:   []string{"create " + typ + " --name NAME [flags]"},
	}
	if kit == nil {
		c.Args = []help.Arg{{Name: "TYPE", Desc: "The agent type: the name of a kit in the registry."}}
		notes = append(notes, anyTypeNotes...)
	} else {
		d := kit.Defaults
		cpus, memory, root, dockerDisk = strconv.Itoa(d.CPUs), strconv.Itoa(d.MemoryGiB), strconv.Itoa(d.RootGiB), strconv.Itoa(d.DockerGiB)
		c.Summary = "Create a new " + typ + " agent from the latest " + typ + " kit, and start it."
		if summary != "" {
			notes = append([]string{"The kit: " + summary}, notes...)
		}
		c.Sections = []help.Section{{Title: "Asks for, in order", Rows: askRows(*kit)}}
	}
	c.Flags = []help.Flag{
		{Name: "name", Value: "NAME", Desc: "The agent's name, kept as typed (at most 35 characters).", Required: true},
		{Name: "cpu", Value: "N|max", Desc: "CPUs; max is all this machine has.", Default: cpus},
		{Name: "memory", Value: "GiB|max", Desc: "Memory in GiB; max is all that is left after the host's reserve and the VM's overhead.", Default: memory},
		{Name: "storage", Value: "GiB|max", Desc: "Root disk in GiB; max is the free disk left (after 10% kept free) once the Docker disk is set.", Default: root},
		{Name: "docker", Value: "GiB|max", Desc: "Docker disk (images, build cache) in GiB; with --storage max too, the free disk is split in the default sizes' ratio.", Default: dockerDisk},
		{Name: "soul", Value: "FILE", Desc: "The agent's SOUL.md.", Default: "the kit's, or Hermes's own when the kit has none"},
		{Name: "avatar", Value: "PATH|URL", Desc: "Profile picture: a PNG or JPEG file, or an https address.", Default: "none"},
		{Name: "force", Desc: "Create even over the machine's resource budget.", Default: "ask"},
	}
	c.Notes = append(append([]string{}, notes...),
		"Asks for sudo first.",
		"The first time, also asks for a registry token when the registry needs one to read the kits.",
		"If it stops partway, run it again: it resumes.",
	)
	return help.Render(c)
}

// askRows is what create asks for, in order, as the kit words it: its settings, its services,
// and the Slack manager after its last Slack service.
func askRows(kit published.Kit) []help.Row {
	sentence := func(text, state string) string {
		text = strings.TrimSpace(text)
		if !strings.HasSuffix(text, ".") {
			text += "."
		}
		return text + " " + state
	}
	var rows []help.Row
	for _, s := range kit.Settings {
		rows = append(rows, help.Row{Name: s.Env, Desc: sentence(s.Prompt, "Optional.")})
	}
	lastSlack := -1
	for i, s := range kit.Services {
		if s.Slack {
			lastSlack = i
		}
	}
	for i, s := range kit.Services {
		state := "Required."
		if !s.Required {
			state = "Optional."
		}
		rows = append(rows, help.Row{Name: s.Name, Desc: sentence(s.Prompt, state)})
		if i == lastSlack {
			rows = append(rows, help.Row{Name: "manager", Desc: "The manager's Slack member ID (U…), usually yours. Required."})
		}
	}
	return rows
}

// createTypeHelp is one type's create help, from its latest kit's label in the registry, read
// without pulling the kit. A registry or a kit that cannot be read leaves create's own help,
// with the reason: help never fails.
func createTypeHelp(e env, typ string) string {
	unread := func(why string) string {
		return renderCreateHelp("TYPE", "", nil, []string{typ + "'s own sizes and questions are not shown: " + why + "."})
	}
	machine, err := config.LoadMachine()
	if err != nil {
		return unread("this machine's settings could not be read")
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	repository := machine.Registry + "/" + typ
	kit, description, err := published.Docker{Log: io.Discard}.Describe(ctx, repository+":latest")
	if err != nil {
		return unread(repository + " could not be read (" + err.Error() + ")")
	}
	return renderCreateHelp(typ, description, &kit, nil)
}

// typeHelpRequested is the type whose help "create TYPE --help" asks for: the first positional
// argument, never a flag's value.
func typeHelpRequested(args []string) (string, bool) {
	asked, first := false, ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help" || a == "-help" || a == "-h":
			asked = true
		case strings.HasPrefix(a, "-"):
			if name := strings.TrimLeft(a, "-"); !strings.Contains(name, "=") && name != "force" {
				i++ // its value
			}
		case first == "":
			first = a
		}
	}
	return first, asked && kitType(first)
}

func createCommand(e env, args []string) int {
	if typ, ok := typeHelpRequested(args); ok {
		fmt.Fprint(e.out, createTypeHelp(e, typ))
		return 0
	}
	var f createFlags
	rest, code, ok := subcommand(e, "create", createHelp, args, func(set *flag.FlagSet) {
		set.StringVar(&f.name, "name", "", "")
		set.StringVar(&f.soul, "soul", "", "")
		set.StringVar(&f.avatar, "avatar", "", "")
		f.sizes.register(set)
		set.BoolVar(&f.force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, createHelp, "create: name one agent type")
	}
	if f.name == "" {
		return usageError(e, createHelp, "create: give the new agent a name with --name")
	}
	if err := f.sizes.inRange(); err != nil {
		return usageError(e, createHelp, "create: %v", err)
	}
	return createPublished(e, rest[0], f)
}

// budgetMode is whether a transaction checks the machine's budget first: not at all (a read
// only check, or one its caller already made), asking to confirm, or confirmed (--force).
type budgetMode int

const (
	skipBudget budgetMode = iota
	confirmBudget
	forcedBudget
	reportBudget // a check that changes nothing: over budget fails it, without asking
)

func askBudget(force bool) budgetMode {
	if force {
		return forcedBudget
	}
	return confirmBudget
}

func runRecreate(e env, name string, create bool, mode budgetMode) int {
	command := "reset"
	if create {
		command = "create"
	}
	code := runTransaction(e, command, name, mode, func(a *agent.Agent, o agent.RecreateOptions) error {
		o.Create = create
		return a.Recreate(e.ctx, o)
	})
	if code != 0 || !create {
		return code
	}
	// A new agent ends with one model turn, so its user knows it works.
	a, err := lifecycleAgent(e, command, name, true)
	if err != nil {
		return fail(e, err)
	}
	return a.Report(a.SelfTest(e.ctx))
}

// runTransaction reads the agent's kit and secrets, checks the machine's budget (every
// transaction may boot the agent's VM, and a create adds its disks), and runs one VM transaction.
func runTransaction(e env, command, name string, mode budgetMode, run func(*agent.Agent, agent.RecreateOptions) error) int {
	a, err := lifecycleAgent(e, command, name, true)
	if err != nil {
		return fail(e, err)
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	kit, err := agent.ReadKit(reg, name)
	if err != nil {
		return fail(e, err)
	}
	if mode != skipBudget {
		want := budget.Want{Name: name, CPUs: kit.CPUs, MemoryGiB: kit.MemoryGiB}
		if command == "create" {
			want.DiskGrowthGiB, want.NewVM = kit.RootGiB+kit.DockerGiB, true
		}
		if err := checkBudget(e, a.Runner, a.Printer, want, mode); err != nil {
			return a.Report(budgetFailure(err))
		}
	}
	store, err := agentStore(e, name, a)
	if err != nil {
		return fail(e, err)
	}
	machine, _ := config.LoadMachine()
	err = run(a, agent.RecreateOptions{Kit: kit, Secrets: store,
		Units: func() error { return unitsLoaded(reg.Names) }, CollectorTimer: machine.CollectorTimer})
	var f secret.Failure
	if errors.As(err, &f) {
		err = agent.Failure{Step: command, Class: f.Class}
	}
	return a.Report(err)
}

// agentStore is the agent's secrets: his sbxenv.yaml services and his base kit's placeholders.
func agentStore(e env, name string, a *agent.Agent) (*secret.Store, error) {
	reg, _, err := e.registry()
	if err != nil {
		return nil, err
	}
	base, err := reg.Base(name)
	if err != nil {
		return nil, err
	}
	native, err := reg.Secrets(name)
	if err != nil {
		return nil, err
	}
	services, err := secret.Services(name, native, base)
	if err != nil {
		return nil, err
	}
	// A published agent's kit declares every placeholder service its type may use; the agent has
	// only those it was given keys for, recorded in its source.json.
	if registry.IsPublished(base) {
		folder, err := reg.Folder(name)
		if err != nil {
			return nil, err
		}
		source, err := readSource(folder)
		if err != nil {
			return nil, err
		}
		kept := services[:0]
		for _, s := range services {
			if !s.Placeholder || contains(source.Services, s.Name) {
				kept = append(kept, s)
			}
		}
		services = kept
	}
	return &secret.Store{Agent: name, Services: services, Root: secret.DefaultRoot(), Runner: a.Runner,
		ReadSecret: secret.TerminalReader(e.ctx)}, nil
}

// gatewayUnit is where the gateway template must be installed.
var gatewayUnit = "/etc/systemd/system/hermes-gateway@.service"

// unitsLoaded fails unless the installed gateway template is exactly troupe's render and every
// agent's gateway unit has loaded troupe's start guard.
func unitsLoaded(names func() ([]string, error)) error {
	want, err := renderUnit("hermes-gateway@.service")
	if err != nil {
		return err
	}
	installed, err := os.ReadFile(gatewayUnit)
	if err != nil || string(installed) != want {
		return fmt.Errorf("%s differs from this troupe's: run the installer again", gatewayUnit)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	agents, err := names()
	if err != nil {
		return err
	}
	for _, name := range agents {
		out, err := exec.Command("systemctl", "show", "hermes-gateway@"+name+".service", "-p", "ExecCondition", "--value").Output()
		if err != nil || !strings.Contains(string(out), " guard check ") || !strings.Contains(string(out), resolved(self)) {
			return fmt.Errorf("the start guard is not loaded for %s; run sudo systemctl daemon-reload", name)
		}
	}
	return nil
}

func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}
