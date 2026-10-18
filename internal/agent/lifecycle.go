package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// Start enables the agent's gateway unit, whose attached sbx exec boots the VM, and waits
// until the gateway runs (and, for Slack kits, Slack is connected). Boot starts it again.
func (a *Agent) Start(ctx context.Context) error {
	s, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	records, err := a.Runner.Inventory(ctx)
	if err != nil {
		return Failure{"preflight", "inventory_unreadable"}
	}
	// A unit for a sandbox that does not exist would restart forever.
	if _, exists := records[a.Name]; !exists {
		return Failure{"preflight", "sandbox_missing; create the agent's sandbox first"}
	}
	if err := a.systemctl("enable", "--now", a.Unit()); err != nil {
		return Failure{"start", "gateway_start_failed"}
	}
	waiting := "Waiting for the gateway"
	if a.Slack {
		waiting = "Waiting for the gateway and Slack"
	}
	a.Printer.Print("start", progress.Started, waiting)
	if err := a.waitReady(ctx); err != nil {
		return err
	}
	a.Printer.Print("start", progress.OK, "Running; boot will start it")
	return nil
}

// waitReady polls until the unit is active and the guest reports the gateway ready.
func (a *Agent) waitReady(ctx context.Context) error {
	args := []string{}
	if a.Slack {
		args = []string{"--platform", "slack"}
	}
	for try := 0; try < a.Timing.ReadyTries; try++ {
		if a.active() {
			result, err := a.call(ctx, 120*time.Second, "ready", args...)
			if errors.Is(err, ErrInterrupted) {
				return err
			}
			if err == nil && result != nil && result["status"] == "passed" {
				return nil
			}
		}
		if err := sleep(ctx, a.Timing.ReadyEvery); err != nil {
			return err
		}
	}
	return Failure{"start", "gateway_not_ready"}
}

// Stop lets the current work finish, stops and disables the gateway unit (its ExecStop stops
// Hermes cleanly), then stops the VM and waits until it is gone, freeing its memory at once.
// A failed drain leaves the agent running untouched.
func (a *Agent) Stop(ctx context.Context, capMinutes int) (err error) {
	s, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	if a.mainPID() == "0" && a.vmStopped(ctx) {
		if err := a.systemctl("disable", a.Unit()); err != nil {
			return Failure{"stop", "gateway_disable_failed"}
		}
		a.Printer.Print("stop", progress.OK, "Already stopped; boot will not start it")
		return nil
	}
	a.Printer.Print("stop", progress.Started, "Letting the current work finish, then stopping")
	// Drained but never stopped: the agent must not stay refusing work.
	draining := true
	defer func() {
		if err != nil && draining {
			a.quietCancel()
		}
	}()
	if err := a.Drain(ctx, capMinutes); err != nil {
		draining = false // the drain cleared its own marker
		if errors.Is(err, ErrInterrupted) {
			return err
		}
		return Failure{"drain", "drain_failed; the agent is still running"}
	}
	if a.afterDrain != nil {
		a.afterDrain()
	}
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	// Stopping runs ExecStop, which stops Hermes cleanly and clears the drain marker.
	if err := a.systemctl("disable", "--now", a.Unit()); err != nil {
		return Failure{"stop", "gateway_stop_failed"}
	}
	draining = false
	if err := a.probe(ctx); err != nil {
		return Failure{"stop", "sandboxd_unhealthy"}
	}
	// An already stopped VM may make sbx stop fail; the wait below is the proof.
	a.Runner.Output(ctx, sbx.Call{Args: []string{"stop", a.Name}, Timeout: 120 * time.Second})
	deadline := time.Now().Add(a.Timing.StopWait)
	for !a.vmStopped(ctx) {
		if time.Now().After(deadline) {
			return Failure{"stop", "vm_still_running"}
		}
		if err := sleep(ctx, a.Timing.StopEvery); err != nil {
			return err
		}
	}
	a.Printer.Print("stop", progress.OK, "Stopped; boot will not start it")
	return nil
}

// quietCancel clears a drain this stop engaged, printing only if that fails. It runs detached
// from the terminal, so a second Ctrl+C cannot cut it short.
func (a *Agent) quietCancel() {
	quiet := *a
	quiet.Runner.Detached = true
	quiet.Printer = &progress.Printer{Agent: a.Printer.Agent, Location: a.Printer.Location,
		Out: discard{}, Err: discard{}, Now: a.Printer.Now}
	if err := quiet.CancelDrain(context.Background()); err != nil {
		a.Printer.Print("drain", progress.Failed, "Could not clear the drain marker. Run: troupe drain "+a.Name+" --cancel")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// GatewayStop is the gateway unit's ExecStop: stopping the host-side sbx exec client does not
// signal the guest, so this stops the HERMES_HOME-scoped gateway explicitly and waits for its
// bounded clean exit. It takes no lock and no sudo: systemd runs it as the host user.
func (a *Agent) GatewayStop(ctx context.Context) error {
	if err := a.probe(ctx); err != nil {
		return Failure{"gateway_stop", "sandboxd_unhealthy"}
	}
	result, err := a.call(ctx, 80*time.Second, "stop-gateway")
	if errors.Is(err, ErrInterrupted) {
		return err
	}
	if err != nil || result == nil || result["status"] != "passed" {
		return Failure{"gateway_stop", "guest_stop_failed"}
	}
	if result["state"] == "already_stopped" {
		a.Printer.Print("gateway stop", progress.OK, "The guest gateway was already stopped")
	} else {
		a.Printer.Print("gateway stop", progress.OK, "The guest gateway stopped cleanly")
	}
	return nil
}

// SelfTest sends a new agent one model turn through its own Hermes and provider (the selftest
// guest job, judged on transport only) and shows the reply. A failure names the likely cause;
// the agent stays as it is.
func (a *Agent) SelfTest(ctx context.Context) error {
	a.Printer.Print("self-test", progress.Started, "Sending one message through the agent's model provider")
	result, err := a.call(ctx, 12*time.Minute, "selftest")
	if errors.Is(err, ErrInterrupted) {
		return err
	}
	if result["status"] == "passed" {
		reply, _ := result["reply"].(string)
		a.Printer.Print("self-test", progress.OK, a.Printer.Agent+" answered: "+Printable(reply))
		// A Slack agent is checked end to end only by a person writing to it there.
		if a.Slack {
			a.Printer.Print("self-test", progress.Info, "Say hi in Slack: its manager sends @"+a.Printer.Agent+
				" a DM; it should answer there (in this terminal: troupe chat "+a.Name+")")
		} else {
			a.Printer.Print("self-test", progress.Info, "Talk to it: troupe chat "+a.Name)
		}
		return nil
	}
	class := "selftest_failed"
	if result != nil {
		class = Plain(result["class"])
	}
	hint := map[string]string{
		"key_rejected":         "the provider refused the agent's key; set it again with troupe secret set " + a.Name + " SERVICE",
		"network_unreachable":  "the agent's VM could not reach its provider",
		"timeout":              "the provider did not answer in time",
		"provider_failed":      "the provider failed the turn",
		"usage_record_invalid": "the turn did not complete with the configured model",
	}[class]
	if hint == "" {
		hint = "the turn failed"
	}
	return Failure{"self-test", class + "; " + hint + ". The agent is created; troupe chat " + a.Name + " once it is fixed"}
}

// Printable is text from inside a VM made safe for a terminal: one line, printable ASCII only,
// everything else escaped, as the logs are (a compromised agent could otherwise send control
// codes, and UTF-8 continuation bytes are C1 controls to an 8-bit terminal).
func Printable(text string) string {
	var b strings.Builder
	for _, r := range strings.Join(strings.Fields(text), " ") {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "\\u%04x", r)
		}
	}
	return b.String()
}

// Chat opens Hermes's own chat in the running agent's VM, attached to the operator's terminal.
// Unlike every other exec, its bytes reach the terminal raw: it is the operator's own session
// with the agent. A stopped agent is refused, because sbx exec would boot its VM behind troupe.
func (a *Agent) Chat(ctx context.Context, stdin, stdout, stderr *os.File) error {
	if err := a.probe(ctx); err != nil {
		return Failure{"chat", "sandboxd_unhealthy"}
	}
	records, err := a.Runner.Inventory(ctx)
	if err != nil {
		return Failure{"chat", "inventory_unreadable"}
	}
	if records[a.Name].Status != "running" || !unitUp(a.Name) {
		return Failure{"chat", "not_running; start it first: troupe start " + a.Name}
	}
	binary := a.Runner.Binary
	if binary == "" {
		binary = "sbx"
	}
	// No context: the operator's Ctrl+C belongs to the chat (the terminal is raw), and troupe
	// waits for the chat to end however it ends.
	cmd := exec.Command(binary, "exec", "-it", "-w", "/home/agent/workspace", a.Name, "/home/agent/.local/bin/hermes", "--tui")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return Failure{"chat", "chat_ended_with_an_error"}
	}
	return nil
}
