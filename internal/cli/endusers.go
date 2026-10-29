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
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/published"
)

var resetHelp = help.Render(help.Command{
	Summary: "Factory reset an agent: start it fresh from its kit, keeping the agent and its keys.",
	Usage:   []string{"reset AGENT [flags]"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to reset."}},
	Flags: []help.Flag{
		{Name: "check", Desc: "Only check that a reset would pass (exit 0); no prompt, no sudo, only the build cache changes.", Default: "off"},
		{Name: "yes", Desc: "Answer the confirmation, for scripts.", Default: "ask"},
		{Name: "force", Desc: "Reset even over the machine's resource budget.", Default: "ask"},
	},
	Notes: []string{
		"Erases what the agent learned: its memory, sessions, workspace and the skills it changed.",
		"Lets its current work finish first. Asks to confirm, then for sudo.",
		"If it stops partway, run it again: it resumes.",
	},
})

var deleteHelp = help.Render(help.Command{
	Summary: "Remove an agent entirely: its VM, keys, records and logs; the name is free again.",
	Usage:   []string{"delete AGENT"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to delete."}},
	Notes: []string{
		"Cannot be undone. Asks you to type the agent's name as shown, then for sudo.",
		"Does not revoke its keys at their providers or delete its Slack app: it reminds you to.",
		"If it stops partway, run it again: it finishes.",
	},
})

var resizeHelp = help.Render(help.Command{
	Summary: "Change an agent's CPUs, memory or disks, keeping everything it has.",
	Usage:   []string{"resize AGENT [flags]"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to resize."}},
	Flags: []help.Flag{
		{Name: "cpu", Value: "N|max", Desc: "CPUs; max is all this machine has.", Default: "unchanged"},
		{Name: "memory", Value: "GiB|max", Desc: "Memory in GiB; max is all that is left after the host's reserve and the VM's overhead.", Default: "unchanged"},
		{Name: "storage", Value: "GiB|max", Desc: "Root disk in GiB; it can only grow. max adds all the free disk left (after 10% kept free).", Default: "unchanged"},
		{Name: "docker", Value: "GiB|max", Desc: "Docker disk in GiB; the new VM's starts empty, as on every resize (images and cache are rebuilt). With --storage max too, the free disk is split in the current sizes' ratio.", Default: "unchanged"},
		{Name: "force", Desc: "Resize even over the machine's resource budget.", Default: "ask"},
	},
	Notes: []string{
		"Moves the agent into a new VM at the new sizes, with its memory, sessions, workspace and skills.",
		"With no sizes, moves it into a fresh VM at the sizes on record.",
		"If it stops partway, run it again without sizes: it resumes.",
	},
})

var chatHelp = help.Render(help.Command{
	Summary: "Talk to a running agent from this terminal, in Hermes's own chat.",
	Usage:   []string{"chat AGENT"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "The agent to talk to."}},
	Notes:   []string{"The agent keeps running and serving its other channels."},
})

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

// terminal reports whether f is a terminal. It goes through SyscallConn, never Fd, as
// secret's termios does: Fd would switch f to blocking mode.
func terminal(f *os.File) bool {
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var t syscall.Termios
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	}); err != nil {
		return false
	}
	return errno == 0
}

func resizeCommand(e env, args []string) int {
	var f sizeFlags
	force := false
	rest, code, ok := subcommand(e, "resize", resizeHelp, args, func(set *flag.FlagSet) {
		f.register(set)
		set.BoolVar(&force, "force", false, "")
	})
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, resizeHelp, "resize: name one agent")
	}
	if err := f.inRange(); err != nil {
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
	if filepath.Dir(folder) != filepath.Join(reg.Data, "agents") && f.given() {
		return fail(e, fmt.Errorf("%s's sizes are in its own folder (%s): edit cpus and memory in sbxenv.yaml or the disks in storage.env, then run troupe resize %s", name, folder, name))
	}
	kit, err := agent.ReadKit(reg, name)
	if err != nil {
		return fail(e, err)
	}
	if f.storage.n > 0 && f.storage.n < kit.RootGiB {
		return fail(e, fmt.Errorf("the root disk can only grow: %s has %d GiB", name, kit.RootGiB))
	}
	if f.docker.given() && kit.DockerGiB == 0 {
		return fail(e, fmt.Errorf("%s has no Docker disk to resize", name))
	}
	current := published.Defaults{CPUs: kit.CPUs, MemoryGiB: kit.MemoryGiB, RootGiB: kit.RootGiB, DockerGiB: kit.DockerGiB}
	a, err := lifecycleAgent(e, "resize", name, true)
	if err != nil {
		return fail(e, err)
	}
	room, err := machineRoom(e, a.Runner, f, name)
	if err != nil {
		return a.Report(budgetFailure(err))
	}
	sizes, err := f.resolve(current, room, true)
	if err != nil {
		return a.Report(budgetFailure(err))
	}
	changed := sizes != current
	if changed && agent.Open(name) {
		return fail(e, fmt.Errorf("an earlier run on %s stopped partway; finish it first (run it again: troupe resize %s without sizes resumes a move)", name, name))
	}
	root := sizes.RootGiB
	next := budget.Want{Name: name, CPUs: sizes.CPUs, MemoryGiB: sizes.MemoryGiB,
		DiskGrowthGiB: max(root-kit.RootGiB+sizes.DockerGiB-kit.DockerGiB, 0)}
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
		if restore, err = published.Resize(folder, sizes.CPUs, sizes.MemoryGiB, root, sizes.DockerGiB); err != nil {
			return fail(e, err)
		}
		a.Printer.Print("resize", progress.Info, "New sizes recorded: "+f.describe(sizes))
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
