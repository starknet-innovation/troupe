package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/starknet-innovation/troupe/internal/help"
	"os"
	"path/filepath"
	"strings"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/published"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/secret"
)

var upgradeHelp = help.Render(help.Command{
	Summary: "Move an agent onto the newest version of its kit, keeping everything it has.",
	Usage:   []string{"upgrade AGENT [flags]"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to upgrade, one created from a published kit."}},
	Flags:   []help.Flag{{Name: "force", Desc: "Upgrade even over the machine's resource budget.", Default: "ask"}},
	Notes: []string{
		"Takes only a newer version of the same MAJOR; a new MAJOR means creating the agent again.",
		"Replaces the agent's edits to seeded skills and scripts; copies go to ~/.local/state/live-apply/.",
		"Asks for sudo, then for any key the new version needs.",
		"If the move stops partway, troupe resize AGENT resumes it; then run troupe upgrade AGENT again.",
	},
})

func upgradeCommand(e env, args []string) int {
	force := false
	rest, code, ok := subcommand(e, "upgrade", upgradeHelp, args, func(set *flag.FlagSet) {
		set.BoolVar(&force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, upgradeHelp, "upgrade: name one agent")
	}
	name := rest[0]
	if err := known(e, name); err != nil {
		return fail(e, err)
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	folder, err := reg.Folder(name)
	if err != nil {
		return fail(e, err)
	}
	if filepath.Dir(folder) != filepath.Join(reg.Data, "agents") {
		return fail(e, fmt.Errorf("%s was not created from a published kit: its kit is its own folder (%s)", name, folder))
	}
	if agent.Open(name) {
		return fail(e, fmt.Errorf("an earlier run on %s stopped partway; finish it first (troupe resize %s resumes a move)", name, name))
	}
	current, err := readSource(folder)
	if err != nil {
		return fail(e, err)
	}
	a, err := lifecycleAgent(e, "upgrade", name, true)
	if err != nil {
		return fail(e, err)
	}
	// The budget first (the agent keeps its sizes), then sudo: a declined confirmation asks
	// for nothing.
	kit, err := agent.ReadKit(reg, name)
	if err != nil {
		return fail(e, err)
	}
	if err := checkBudget(e, a.Runner, a.Printer, budget.Want{Name: name, CPUs: kit.CPUs, MemoryGiB: kit.MemoryGiB}, askBudget(force)); err != nil {
		return a.Report(budgetFailure(err))
	}
	stopSudo, err := askSudo(e.ctx)
	if err != nil {
		return fail(e, err)
	}
	defer stopSudo()

	logPath := agent.LogFile(name, "upgrade.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fail(e, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(e, err)
	}
	defer logFile.Close()
	docker := published.Docker{Log: sbx.LogWriter(logFile)}
	runner := sbx.Runner{Paths: guard.Default(), Log: logPath}
	pending := agent.LogFile(name, "upgrade-seeds-pending")
	repository := current.Reference[:strings.LastIndex(current.Reference, ":")]
	host, _, _ := strings.Cut(repository, "/")
	a.Printer.Print("kit", progress.Started, "Finding the latest "+repository+" kit")
	img, err := resolve(e, docker, runner, repository, host)
	if err != nil {
		return a.Report(agent.Failure{Step: "kit", Class: err.Error()})
	}
	on := current.Reference[strings.LastIndex(current.Reference, ":")+1:]
	same, err := upgradeStep(img.Type, img.Version, on, name)
	switch {
	case err != nil:
		return a.Report(agent.Failure{Step: "kit", Class: err.Error()})
	case same:
		a.Printer.Print("upgrade", progress.Info, fmt.Sprintf("%s is already on %s %s, the latest", a.Printer.Agent, img.Type, on))
		return alreadyLatest(e, a, name, pending)
	}
	newKit, err := docker.Extract(e.ctx, reg.Data, img)
	if err != nil {
		return a.Report(agent.Failure{Step: "kit", Class: err.Error()})
	}
	oldKit := registry.KitFolder(reg.Data, current.Reference)
	a.Printer.Print("kit", progress.OK, fmt.Sprintf("%s %s, from %s", img.Type, img.Version, on))
	// The old version's files stay: another agent may still be on it, or move back to it.
	if err := sameSlackApp(oldKit, newKit); err != nil {
		return a.Report(agent.Failure{Step: "kit", Class: err.Error()})
	}
	if err := upgradeKeys(e, a, folder, newKit, name, current, runner); err != nil {
		if errors.Is(err, secret.ErrInterrupted) {
			return a.Report(agent.ErrInterrupted)
		}
		return a.Report(agent.Failure{Step: "keys", Class: err.Error()})
	}

	restore, err := published.Repin(folder, current.Reference, img)
	if err != nil {
		return fail(e, err)
	}
	if err := writePending(pending); err != nil {
		restore()
		return fail(e, err)
	}
	a.Printer.Print("upgrade", progress.Info, fmt.Sprintf("%s pinned to %s %s", a.Printer.Agent, img.Type, img.Version))
	result := runTransaction(e, "upgrade", name, skipBudget, func(a *agent.Agent, o agent.RecreateOptions) error {
		o.Label = "upgrade"
		return a.Migrate(e.ctx, o)
	})
	if result != 0 {
		// A move that never began leaves the agent on its old version; one that began resumes
		// on the new one with troupe resize, and its seeds are applied by troupe upgrade again.
		if !agent.Open(name) {
			restore()
			os.Remove(pending)
		}
		return result
	}
	if code := finishUpgrade(e, a, name, pending); code != 0 {
		return code
	}
	a.Printer.Print("upgrade", progress.OK, fmt.Sprintf("%s is on %s %s", a.Printer.Agent, img.Type, img.Version))
	return 0
}

// alreadyLatest is an upgrade with no newer version: the agent is left as it is (its lessons
// cannot be in a version yet), unless the last upgrade stopped before its seed step (a move
// finished by resize, or a seed step that failed), which is finished now.
func alreadyLatest(e env, a *agent.Agent, name, pending string) int {
	if _, err := os.Stat(pending); err != nil {
		return 0
	}
	return finishUpgrade(e, a, name, pending)
}

// finishUpgrade is an upgrade's last step, after the move: the agent's seeded skills and
// scripts become the new version's. pending, written when the new version was pinned, is
// removed only once they match, so a rerun on the same version finishes a stopped upgrade
// and otherwise leaves the agent as it is.
func finishUpgrade(e env, a *agent.Agent, name, pending string) int {
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	kits, err := reg.Kits(name)
	if err != nil {
		return fail(e, err)
	}
	if code := a.Report(a.ApplySeeds(e.ctx, agent.Seeds{Kits: kits})); code != 0 {
		return code
	}
	if err := os.Remove(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(e, err)
	}
	return 0
}

// upgradeStep is the version rule: the latest equal to the agent's is nothing to move (same);
// newer within the same MAJOR is an upgrade; older, or another MAJOR, is refused.
func upgradeStep(typ, latest, on, name string) (same bool, err error) {
	order, err := published.Compare(latest, on)
	switch {
	case err != nil:
		return false, err
	case order == 0:
		return true, nil
	case order < 0:
		return false, fmt.Errorf("the latest %s is %s, older than the agent's %s", typ, latest, on)
	case published.Major(latest) != published.Major(on):
		return false, fmt.Errorf("%s %s is a new MAJOR version; recreate the agent instead (troupe delete %s, then troupe create %s)", typ, latest, name, typ)
	}
	return false, nil
}

// sameSlackApp refuses an upgrade whose kit asks for a different Slack app: new scopes or events
// need the operator to change the app in Slack, which a move cannot do or prove.
func sameSlackApp(oldKit, newKit string) error {
	before, errBefore := os.ReadFile(filepath.Join(oldKit, "slack-app-manifest.json"))
	after, errAfter := os.ReadFile(filepath.Join(newKit, "slack-app-manifest.json"))
	if os.IsNotExist(errBefore) && os.IsNotExist(errAfter) {
		return nil
	}
	if os.IsNotExist(errBefore) != os.IsNotExist(errAfter) {
		return errors.New("the new version adds or drops the Slack app; recreate the agent")
	}
	if errBefore != nil || errAfter != nil {
		return errors.New("slack_manifest_unreadable")
	}
	if !bytes.Equal(before, after) {
		return fmt.Errorf("the new version's Slack app manifest differs (%s); recreate the agent with a new Slack app", filepath.Join(newKit, "slack-app-manifest.json"))
	}
	return nil
}

// upgradeKeys asks for and stores every key the new version requires that the agent lacks, and
// records it. A missing native key (one its sbxenv.yaml would bind) or Slack token is refused:
// a move cannot add a binding or a Slack app.
func upgradeKeys(e env, a *agent.Agent, folder, newKit, name string, current source, runner sbx.Runner) error {
	declared, err := secret.KitServices(newKit)
	if err != nil {
		return err
	}
	natives, err := registry.SecretsIn(folder)
	if err != nil {
		return err
	}
	for _, s := range declared {
		if !s.Required || contains(current.Services, s.Name) || contains(natives, s.Name) {
			continue
		}
		if s.Delivery != "placeholder" || s.Slack {
			return fmt.Errorf("the new version requires a %s key %s cannot be given in place; recreate the agent", s.Name, a.Printer.Agent)
		}
		if s.Help != "" {
			a.Printer.Print("keys", progress.Info, s.Help)
		}
		value, err := secret.TerminalReader(e.ctx)(s.Prompt + " (hidden): ")
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(value))) == 0 {
			return fmt.Errorf("%s is required by the new version", s.Name)
		}
		svc := secret.Service{Name: s.Name, Placeholder: true, Env: s.Env, Hosts: s.Hosts,
			Value: strings.ReplaceAll(s.Placeholder, "{agent}", name)}
		store := &secret.Store{Agent: name, Services: []secret.Service{svc}, Root: secret.DefaultRoot(), Runner: runner,
			ReadSecret: func(string) ([]byte, error) { return value, nil }}
		if _, _, err := store.Set(e.ctx, s.Name); err != nil {
			return err
		}
		if err := addService(folder, s.Name); err != nil {
			return err
		}
		a.Printer.Print("keys", progress.OK, s.Name+" stored")
	}
	return nil
}

func writePending(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}
