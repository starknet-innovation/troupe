//go:build dev

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/starknet-innovation/troupe/internal/help"
	"regexp"
	"time"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

var storageHelp = help.Render(help.Command{
	Summary:  "Check a running sandbox's disks and mounts against its declared storage.",
	Usage:    []string{"storage check NAME ROOT_GIB DOCKER_GIB [HOST_WORKSPACE]"},
	Commands: []help.Row{{Name: "check", Desc: "Check the disk sizes, and that nothing undeclared is mounted or forwarded."}},
	Args: []help.Arg{
		{Name: "NAME", Desc: "The sandbox."},
		{Name: "ROOT_GIB", Desc: "Its declared root disk, in GiB."},
		{Name: "DOCKER_GIB", Desc: "Its declared Docker disk, in GiB; 0 means none."},
		{Name: "HOST_WORKSPACE", Desc: "The one host folder it may mount.", Optional: true, Default: "none"},
	},
	Notes: []string{"The checks inside the VM catch mistakes, not a compromised agent; the host side ones do."},
})

var (
	positive    = regexp.MustCompile(`^[1-9][0-9]*$`)
	nonNegative = regexp.MustCompile(`^[0-9]+$`)
)

func storageCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, storageHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "check" {
		return usageError(e, storageHelp, "storage needs the check subcommand")
	}
	rest, code, ok := subcommand(e, "storage check", storageHelp, args[1:], nil)
	if !ok {
		return code
	}
	if len(rest) < 3 || len(rest) > 4 || !sandboxName.MatchString(rest[0]) ||
		!positive.MatchString(rest[1]) || !nonNegative.MatchString(rest[2]) {
		return usageError(e, storageHelp, "storage check: wrong arguments")
	}
	name, rootGiB, dockerGiB, workspace := rest[0], rest[1], rest[2], ""
	if len(rest) == 4 {
		workspace = rest[3]
	}

	printer := progress.New(label(name), hostLocation(e))
	printer.Out, printer.Err = e.out, e.err
	runner := sbx.Runner{Paths: guard.Default(), Log: stateFile(name, "storage.log")}
	printer.Print("storage", progress.Started, "")
	class, err := checkStorage(e, runner, name, rootGiB, dockerGiB, workspace)
	if err != nil {
		printer.Print("storage", progress.Failed, fmt.Sprintf("%s. Log: %s", class, runner.Log))
		return 1
	}
	printer.Print("storage", progress.OK, "Disks, mounts and workspace match the declaration")
	return 0
}

// checkStorage returns a failure class for the operator line and the underlying error.
func checkStorage(e env, runner sbx.Runner, name, rootGiB, dockerGiB, workspace string) (string, error) {
	inventory, err := runner.Output(e.ctx, sbx.Call{Args: []string{"ls", "--json"}, Timeout: 120 * time.Second})
	if err != nil {
		return failureClass(err, "inventory_failed"), err
	}
	if class := checkInventory(inventory, name); class != "" {
		return class, errors.New(class)
	}
	guestWorkspace := workspace
	if guestWorkspace == "" {
		guestWorkspace = "-"
	}
	out, err := guest.Run(e.ctx, runner, name, "storage", rootGiB, dockerGiB, guestWorkspace)
	var result struct {
		Status string `json:"status"`
		Class  string `json:"class"`
	}
	if decodeErr := json.Unmarshal(bytes.TrimSpace(lastLine(out)), &result); decodeErr == nil && result.Status == "failed" {
		class := agent.Plain(result.Class)
		return class, errors.New(class)
	}
	if err != nil {
		return failureClass(err, "guest_check_failed"), err
	}
	if result.Status != "passed" {
		return "guest_result_invalid", errors.New("guest_result_invalid")
	}
	return "", nil
}

// checkInventory checks sbx ls --json: exactly one sandbox has the name, and it runs. Host
// mounts are the guest check's job: sbx 0.48 no longer lists workspaces.
func checkInventory(data []byte, name string) string {
	// A bare array (sbx 0.48); anything else, null included, is not an inventory.
	var inventory []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &inventory); err != nil || inventory == nil {
		return "inventory_invalid"
	}
	matches := 0
	running := false
	for _, s := range inventory {
		if s.Name == name {
			matches++
			running = s.Status == "running"
		}
	}
	if matches != 1 {
		return "sandbox_missing_or_ambiguous"
	}
	// The check execs into the VM, and an exec boots a stopped one: never beside another agent.
	if !running {
		return "sandbox_not_running"
	}
	return ""
}

func failureClass(err error, fallback string) string {
	switch {
	case errors.Is(err, sbx.ErrTimeout):
		return "sbx_timeout"
	case errors.Is(err, sbx.ErrProbe):
		return "sandboxd_probe_failed"
	}
	return fallback
}

func lastLine(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	return lines[len(lines)-1]
}

// stateFile is NAME's private file under troupe's state folder.
func stateFile(name, file string) string {
	return agent.LogFile(name, file)
}
