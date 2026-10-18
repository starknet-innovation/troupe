package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/published"
)

const resetHelp = `Usage:
  troupe reset AGENT [--check] [--yes] [--force]

Factory reset: erases everything the agent learned (its memory, sessions, workspace and the
skills it changed) and starts it fresh from the kit it was created from, keeping the agent
and its keys. Lets its current work finish first. Asks you to confirm, then for sudo before
any work. If it stops partway, run it again: it resumes.

  --check  proves now that a reset would pass its preflight (sandboxd, keys, units, the
           machine's budget, and the kits: sbx env plan and a build into the host's build
           cache) and changes nothing but that cache: no prompt, no sudo, no exec into the VM.
           Exit 0 when it would pass.
  --yes    answers the reset's question, for scripts.
  --force  confirms an agent over the machine's resource budget (troupe asks otherwise).
`

const deleteHelp = `Usage:
  troupe delete AGENT

Removes the agent entirely: stops it, deletes its VM and the VM's image, its keys, receipts,
recovery backups and logs, and the folder troupe wrote for it. The name is then free for a
new create. Asks you to type its name as shown, then for sudo before any work. Refused while
a copy of the agent is being made. If it stops partway, run it again: it finishes. troupe
cannot revoke the agent's keys at their providers or delete its Slack app; it reminds you to.
`

const resizeHelp = `Usage:
  troupe resize AGENT [--cpu N] [--memory GiB] [--storage GiB] [--force]

Changes an agent's CPUs, memory or root disk, keeping everything it has: sbx cannot resize a
VM, so troupe records the new sizes and moves the agent into a new VM at them, with its
memories, sessions, workspace and learned skills. Only the sizes given change; the root disk
can only grow. With no sizes it is a plain move into a fresh VM at the sizes on record. Over
the machine's budget it shows by how much and asks you to confirm (--force confirms). If it
stops partway, run it again without sizes: it resumes.
`

const chatHelp = `Usage:
  troupe chat AGENT

Talks to a running agent from this terminal: Hermes's own chat inside the agent's VM. Quit it
the way Hermes says. The agent keeps running and serving its other channels.
`

func resetCommand(e env, args []string) int {
	check, yes, force := false, false, false
	rest, code, ok := subcommand(e, "reset", resetHelp, args, func(set *flag.FlagSet) {
		set.BoolVar(&check, "check", false, "")
		set.BoolVar(&yes, "yes", false, "")
		set.BoolVar(&force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, resetHelp, "reset: name one agent")
	}
	name := rest[0]
	if err := known(e, name); err != nil {
		return fail(e, err)
	}
	if check {
		return runTransaction(e, "reset", name, reportBudget, func(a *agent.Agent, o agent.RecreateOptions) error {
			return a.Check(e.ctx, o)
		})
	}
	if !yes {
		answer, err := readLine(e.ctx, fmt.Sprintf("Reset %s? This erases its memory, sessions, workspace and learned skills; "+
			"it keeps the agent and its keys. [y/N] ", name))
		if err != nil || (answer != "y" && answer != "yes") {
			fmt.Fprintf(e.err, "troupe: %s was not reset\n", name)
			return 1
		}
	}
	return runRecreate(e, name, false, askBudget(force))
}

func deleteCommand(e env, args []string) int {
	rest, code, ok := subcommand(e, "delete", deleteHelp, args, nil)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, deleteHelp, "delete: name one agent")
	}
	name := rest[0]
	if !sandboxName.MatchString(name) {
		return usageError(e, deleteHelp, "delete: malformed agent name: %s", name)
	}
	if strings.HasSuffix(name, "-lab") {
		return usageError(e, deleteHelp, "delete: %s is a Lab, never an agent", name)
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	names, err := reg.Names()
	if err != nil {
		return fail(e, err)
	}
	// An agent troupe knows, or what troupe kept of one (a create that failed partway): its own
	// receipts or the folder it wrote. Never a keys folder alone (other tools keep theirs in the
	// same place) and never a sandbox troupe has no record of.
	registered := contains(names, name)
	published := filepath.Join(reg.Data, "agents", name)
	if !registered && !agent.Existed(name) && !exists(published) {
		return fail(e, fmt.Errorf("troupe has no agent named %s", name))
	}
	if reservedName(name) {
		return fail(e, fmt.Errorf("%s is a name troupe reserves", name))
	}
	local := ""
	if registered {
		if folder, err := reg.Folder(name); err == nil && filepath.Dir(folder) != filepath.Join(reg.Data, "agents") {
			local = folder
		}
	}
	// Confirmed with the name people read (as created, such as "Mr. Doyle"), not the ID. Shown
	// between plain quotes exactly as it must be typed: agentNames holds it to printable text.
	display := displayName(reg, name)
	typed, err := readLine(e.ctx, fmt.Sprintf("Deleting %s removes its VM, memory, keys and records for good. Type \"%s\" to delete it: ", display, display))
	if err != nil || typed != display {
		fmt.Fprintf(e.err, "troupe: %s was not deleted\n", display)
		return 1
	}
	a, err := lifecycleAgent(e, "delete", name, false)
	if err != nil {
		return fail(e, err)
	}
	folder := published
	if local != "" {
		folder = ""
	}
	if err := a.Delete(e.ctx, folder); err != nil {
		return a.Report(err)
	}
	if local != "" {
		a.Printer.Print("delete", progress.Info, "Its own folder "+local+" stays: troupe did not write it")
	}
	return 0
}

func chatCommand(e env, args []string) int {
	rest, code, ok := subcommand(e, "chat", chatHelp, args, nil)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, chatHelp, "chat: name one agent")
	}
	a, err := lifecycleAgent(e, "chat", rest[0], true)
	if err != nil {
		return fail(e, err)
	}
	if !terminal(os.Stdin) || !terminal(os.Stdout) {
		return fail(e, errors.New("chat needs a terminal"))
	}
	return a.Report(a.Chat(e.ctx, os.Stdin, os.Stdout, os.Stderr))
}

// reservedName is a name whose records would collide with troupe's own shared ones
// (sandbox-pairs/collector.json is the collector's pause record).
func reservedName(name string) bool { return name == "collector" }

// known fails unless troupe knows the agent (in the kits root or published).
func known(e env, name string) error {
	if !sandboxName.MatchString(name) {
		return fmt.Errorf("malformed agent name: %s", name)
	}
	reg, _, err := e.registry()
	if err != nil {
		return err
	}
	_, err = reg.Folder(name)
	return err
}

// terminal reports whether f is a terminal.
func terminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func resizeCommand(e env, args []string) int {
	var cpus, memoryGiB, storageGiB int
	force := false
	rest, code, ok := subcommand(e, "resize", resizeHelp, args, func(set *flag.FlagSet) {
		set.IntVar(&cpus, "cpu", 0, "")
		set.IntVar(&memoryGiB, "memory", 0, "")
		set.IntVar(&storageGiB, "storage", 0, "")
		set.BoolVar(&force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, resizeHelp, "resize: name one agent")
	}
	if err := sizesInRange(cpus, memoryGiB, storageGiB); err != nil {
		return usageError(e, resizeHelp, "resize: %v", err)
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
	// A local agent's sizes are its own folder's files, which troupe never edits: it moves
	// at them, and is refused new ones.
	if filepath.Dir(folder) != filepath.Join(reg.Data, "agents") && (cpus > 0 || memoryGiB > 0 || storageGiB > 0) {
		return fail(e, fmt.Errorf("%s's sizes are in its own folder (%s): edit cpus and memory in sbxenv.yaml or the disks in storage.env, then run troupe resize %s", name, folder, name))
	}
	kit, err := agent.ReadKit(reg, name)
	if err != nil {
		return fail(e, err)
	}
	next := budget.Want{Name: name, CPUs: kit.CPUs, MemoryGiB: kit.MemoryGiB}
	root := kit.RootGiB
	if cpus > 0 {
		next.CPUs = cpus
	}
	if memoryGiB > 0 {
		next.MemoryGiB = memoryGiB
	}
	if storageGiB > 0 {
		if storageGiB < kit.RootGiB {
			return fail(e, fmt.Errorf("the root disk can only grow: %s has %d GiB", name, kit.RootGiB))
		}
		root = storageGiB
	}
	changed := next.CPUs != kit.CPUs || next.MemoryGiB != kit.MemoryGiB || root != kit.RootGiB
	if changed && agent.Open(name) {
		return fail(e, fmt.Errorf("an earlier run on %s stopped partway; finish it first (run it again: troupe resize %s without sizes resumes a move)", name, name))
	}
	a, err := lifecycleAgent(e, "resize", name, true)
	if err != nil {
		return fail(e, err)
	}
	next.DiskGrowthGiB = root - kit.RootGiB
	if err := checkBudget(e, a.Runner, a.Printer, next, askBudget(force)); err != nil {
		return a.Report(budgetFailure(err))
	}
	// sudo before anything changes: the confirmation above is the only prompt before it.
	stopSudo, err := askSudo(e.ctx)
	if err != nil {
		return fail(e, err)
	}
	defer stopSudo()
	restore := func() {}
	if changed {
		if restore, err = published.Resize(folder, next.CPUs, next.MemoryGiB, root); err != nil {
			return fail(e, err)
		}
		a.Printer.Print("resize", progress.Info, fmt.Sprintf("New sizes recorded: %d CPUs, %d GiB memory, %d GiB root disk", next.CPUs, next.MemoryGiB, root))
	}
	result := runTransaction(e, "resize", name, skipBudget, func(a *agent.Agent, o agent.RecreateOptions) error {
		return a.Migrate(e.ctx, o)
	})
	// A move that failed before it began (its preflight refused, or it was interrupted) leaves
	// the agent as it was: the old sizes go back. One that began resumes at the new sizes.
	if result != 0 && !agent.Open(name) {
		restore()
	}
	return result
}

// sizesInRange refuses a negative or absurd size before it reaches any sum.
func sizesInRange(cpus, memoryGiB, storageGiB int) error {
	switch {
	case cpus < 0 || memoryGiB < 0 || storageGiB < 0:
		return errors.New("sizes must be positive whole numbers")
	case cpus > 1024 || memoryGiB > 1<<16 || storageGiB > 1<<20:
		return errors.New("sizes must be at most 1024 CPUs, 65536 GiB of memory and 1048576 GiB of disk")
	}
	return nil
}
