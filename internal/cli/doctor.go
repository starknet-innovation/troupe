package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/doctor"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/progress"
)

var doctorHelp = help.Render(help.Command{
	Summary: "Check whether this machine can host agents, one line per requirement.",
	Usage:   []string{"doctor"},
	Notes: []string{
		"Fails without the hardware: an x86_64 CPU, KVM, room for one coder agent.",
		"Says how keys are kept: sealed by a TPM 2.0, else the host key only (systemd-creds, systemd 256+), else plain files.",
		"Asks for your action on software: systemd, Docker buildx, sbx 0.48+, sbx signed in to Docker (sbx login), sudo, troupe's units.",
		"Exit 1 on any failure. create runs the same checks first.",
	},
})

// machineCheck runs the doctor's checks on this machine; tests replace it.
var machineCheck = func() []doctor.Line {
	return doctor.Check(doctor.Default(guard.Default().Sandboxd, unitsCurrent))
}

func doctorCommand(e env, args []string) int {
	rest, code, ok := subcommand(e, "doctor", doctorHelp, args, nil)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		return usageError(e, doctorHelp, "doctor takes no arguments")
	}
	printer := progress.New("This machine", hostLocation(e))
	printer.Out, printer.Err = e.out, e.err
	// The release check is the doctor's own, never create's: a network call create could not show.
	lines := append(machineCheck(), releaseLine(e.ctx))
	printLines(printer, lines, true)
	if doctor.Failed(lines) {
		return 1
	}
	return 0
}

// printLines prints the doctor's lines: every one, or only the problems.
func printLines(printer *progress.Printer, lines []doctor.Line, all bool) {
	for _, l := range lines {
		switch l.Level {
		case doctor.Fail:
			printer.Print(l.Name, progress.Failed, l.Message)
		case doctor.Warn:
			printer.Print(l.Name, progress.Action, l.Message)
		case doctor.Note:
			printer.Print(l.Name, progress.Info, l.Message)
		default:
			if all {
				printer.Print(l.Name, progress.OK, l.Message)
			}
		}
	}
}

// machineReady refuses a create on a machine with any problem the doctor finds: each would
// break the creation partway.
func machineReady(printer *progress.Printer) error {
	lines := machineCheck()
	for _, l := range lines {
		if l.Level == doctor.Warn || l.Level == doctor.Fail {
			printLines(printer, lines, false)
			return errors.New("this machine is not ready for agents (above; troupe doctor shows every check)")
		}
	}
	return nil
}

// unitDir is where troupe's systemd units are installed.
var unitDir = "/etc/systemd/system"

// unitsCurrent fails unless every unit troupe ships is installed exactly as this binary
// renders it, and the sandbox daemon runs as sbx-daemon.service, enabled at boot. It reads
// only files and systemd: never sbx, which could start a daemon of its own.
func unitsCurrent() error {
	names, err := unitNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		want, err := renderUnit(name)
		if err != nil {
			return err
		}
		installed, err := os.ReadFile(filepath.Join(unitDir, name))
		if err != nil {
			return fmt.Errorf("%s is not installed: run the installer again", name)
		}
		if string(installed) != want {
			return fmt.Errorf("%s differs from this troupe's: run the installer again", name)
		}
	}
	return daemonOwned(guard.Default().Sandboxd)
}

// daemonOwned fails unless sbx-daemon.service is enabled and active, and a running sandbox
// daemon is the unit's own process.
func daemonOwned(sandboxd string) error {
	show := func(property string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, "systemctl", "show", "sbx-daemon.service", "-p", property, "--value").Output()
		return strings.TrimSpace(string(out))
	}
	if state := show("UnitFileState"); state != "enabled" {
		return fmt.Errorf("sbx-daemon.service is not enabled (%s): run the installer again", state)
	}
	if state := show("ActiveState"); state != "active" {
		return fmt.Errorf("sbx-daemon.service is not active (%s): run the installer again", state)
	}
	pid, err := os.ReadFile(filepath.Join(sandboxd, "sandboxd.pid"))
	if err == nil && strings.TrimSpace(string(pid)) != show("MainPID") {
		return fmt.Errorf("the sandbox daemon (pid %s) runs outside sbx-daemon.service: stop it (sbx daemon stop), then sudo systemctl restart sbx-daemon.service", strings.TrimSpace(string(pid)))
	}
	return nil
}
