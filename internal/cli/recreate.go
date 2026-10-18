package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/secret"
)

const createHelp = `Usage:
  troupe create TYPE --name NAME [--cpu N] [--memory GiB] [--storage GiB] [--soul FILE] [--avatar PATH|URL] [--force]
` + createLocalUsage + `
Creates a new agent from the latest published TYPE kit (the registry machine setting): it
pins that kit version, asks for the agent's keys (hidden; Enter skips an optional one) and
stores them with the TPM, writes the agent's environment under troupe's data folder, then
creates its VM, wires its keys and starts it. --cpu, --memory and --storage (the root disk)
change the kit's default sizes; --soul gives the agent's SOUL.md instead of the kit's default.
The first time, it asks for a registry token that can read the kits and stores it in Docker
and sbx.
` + createLocalNote + `
It refuses a name in use. If it stops partway, run it again: it resumes. It asks for sudo.

--force confirms an agent over the machine's resource budget (troupe asks otherwise).
`

func createCommand(e env, args []string) int {
	var f createFlags
	rest, code, ok := subcommand(e, "create", createHelp, args, func(set *flag.FlagSet) {
		set.StringVar(&f.name, "name", "", "")
		set.StringVar(&f.soul, "soul", "", "")
		set.StringVar(&f.avatar, "avatar", "", "")
		set.IntVar(&f.cpus, "cpu", 0, "")
		set.IntVar(&f.memoryGiB, "memory", 0, "")
		set.IntVar(&f.storageGiB, "storage", 0, "")
		set.BoolVar(&f.force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, createHelp, "create: name one agent type and --name, or one local agent")
	}
	if err := sizesInRange(f.cpus, f.memoryGiB, f.storageGiB); err != nil {
		return usageError(e, createHelp, "create: %v", err)
	}
	if f.name != "" {
		return createPublished(e, rest[0], f)
	}
	if !devBuild {
		return usageError(e, createHelp, "create: name an agent type and give the new agent a name with --name")
	}
	if f.soul != "" || f.avatar != "" || f.cpus != 0 || f.memoryGiB != 0 || f.storageGiB != 0 {
		return usageError(e, createHelp, "create: --cpu, --memory, --storage, --soul and --avatar need --name TYPE")
	}
	return runRecreate(e, rest[0], true, askBudget(f.force))
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
