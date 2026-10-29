package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// host is a fake machine: sudo, systemctl and sbx are scripts that record every call in
// calls and answer from files a test writes (mainpid, active, inventory.json, the guest job
// answers). Drain answers are a queue: drain-1.json, drain-2.json, ... then the last repeats.
type host struct {
	t        *testing.T
	dir      string
	agent    *Agent
	out, err bytes.Buffer
}

const fakeSystemctl = `#!/bin/sh
d=$(dirname "$0")/..
echo "systemctl $*" >> "$d/calls"
case "$1" in
  show) cat "$d/mainpid" ;;
  is-active) if [ -f "$d/active-$2" ]; then cat "$d/active-$2"; else cat "$d/active"; fi ;;
  enable|disable) exit "$(cat "$d/systemctl-exit" 2>/dev/null || echo 0)" ;;
esac
`

const fakeSudo = `#!/bin/sh
d=$(dirname "$0")/..
echo "sudo $*" >> "$d/calls"
if [ "$1" = "-v" ]; then exit "$(cat "$d/sudo-exit" 2>/dev/null || echo 0)"; fi
if [ "$1" = "-n" ] && [ "$2" = "-v" ]; then exit 0; fi
if [ "$1" = "-n" ] && [ "$2" = "true" ]; then [ -f "$d/sudo-nopasswd" ]; exit; fi
shift
exec "$@"
`

const fakeSbx = `#!/bin/sh
d=$(dirname "$0")/..
case "$1" in
  ls) echo "sbx ls" >> "$d/calls"; cat "$d/inventory.json" ;;
  stop) echo "sbx stop $2" >> "$d/calls"; [ -f "$d/stop-makes-stopped" ] && cp "$d/stopped.json" "$d/inventory.json"
        exit "$(cat "$d/stop-exit" 2>/dev/null || echo 0)" ;;
  exec)
    cat > /dev/null
    # The job and its arguments follow the run script's "sh PYTHON HERMES".
    while [ "$1" != "sh" ] || [ "$2" = "-c" ]; do shift; done
    shift 3
    echo "job $*" >> "$d/calls"
    case "$1" in
      drain)
        if [ "$2" = "cancel" ]; then cat "$d/cancel.json" 2>/dev/null || echo '{"running": true}'; exit "$(cat "$d/cancel-exit" 2>/dev/null || echo 0)"; fi
        n=$(( $(cat "$d/drain-count" 2>/dev/null || echo 0) + 1 )); echo $n > "$d/drain-count"
        f="$d/drain-$n.json"; [ -f "$f" ] || f=$(ls "$d"/drain-*.json | sort -V | tail -1)
        cat "$f"; exit "$(cat "$f.exit" 2>/dev/null || echo 0)" ;;
      ready) cat "$d/ready.json"; exit "$(cat "$d/ready-exit" 2>/dev/null || echo 0)" ;;
      stop-gateway) cat "$d/stop.json"; exit "$(cat "$d/stop-gateway-exit" 2>/dev/null || echo 0)" ;;
    esac ;;
esac
`

func newHost(t *testing.T) *host {
	t.Helper()
	dir := t.TempDir()
	h := &host{t: t, dir: dir}
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	h.write("bin/systemctl", fakeSystemctl)
	h.write("bin/sudo", fakeSudo)
	h.write("bin/sbx", fakeSbx)
	for _, name := range []string{"bin/systemctl", "bin/sudo", "bin/sbx"} {
		os.Chmod(filepath.Join(dir, name), 0o755)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	// containerd state with no task: the VM is gone unless a test adds a live shim.
	os.MkdirAll(filepath.Join(dir, "sandboxd/containerd/state/io.containerd.runtime.v2.task/docker"), 0o755)
	h.write("mainpid", "4242\n")
	h.write("active", "active\n")
	h.write("active-hermes-gateway@grace.service", "inactive\n")
	h.inventory("running")
	h.write("stopped.json", `[{"name":"ada","id":"1","status":"stopped"}]`)
	h.write("ready.json", `{"status": "passed"}`)
	h.write("stop.json", `{"status": "passed", "state": "stopped"}`)
	h.write("drain-1.json", idle)

	paths := guard.Paths{Pairs: filepath.Join(dir, "pairs"), Sandboxd: filepath.Join(dir, "sandboxd")}
	noProbe := func(context.Context) error { return nil }
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h.agent = &Agent{
		Name: "ada", Command: "test", Paths: paths, Slack: true,
		Terminal: strings.NewReader(""),
		Probe:    noProbe,
		Printer:  &progress.Printer{Agent: "Ada", Location: time.UTC, Out: &h.out, Err: &h.err, Now: func() time.Time { return clock }},
		Runner:   sbx.Runner{Paths: paths, Binary: filepath.Join(bin, "sbx"), Log: filepath.Join(dir, "native.log"), Probe: noProbe},
		Timing: Timing{
			ReadyTries: 3, ReadyEvery: time.Millisecond,
			StopWait: 50 * time.Millisecond, StopEvery: time.Millisecond, Keepalive: time.Hour,
			Drain: DrainTiming{Poll: time.Millisecond, Refresh: time.Hour, Note: time.Hour},
		},
	}
	return h
}

const idle = `{"running": true, "marker": true, "fresh": true, "state": "draining", "active": 0, "work": []}`

func busy(n int, work string) string {
	return fmt.Sprintf(`{"running": true, "marker": true, "fresh": true, "state": "draining", "active": %d, "work": [%s]}`, n, work)
}

func (h *host) write(name, text string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) inventory(status string) {
	h.write("inventory.json", fmt.Sprintf(`[{"name":"ada","id":"1","status":%q},{"name":"grace","id":"2","status":"stopped"}]`, status))
}

func (h *host) drains(answers ...string) {
	for i, answer := range answers {
		h.write(fmt.Sprintf("drain-%d.json", i+1), answer)
	}
}

func (h *host) calls() string {
	data, _ := os.ReadFile(filepath.Join(h.dir, "calls"))
	return string(data)
}

func (h *host) has(call string) bool { return strings.Contains(h.calls(), call+"\n") }

func (h *host) lines() string { return h.out.String() + h.err.String() }

func failureClass(err error) string {
	var f Failure
	if errors.As(err, &f) {
		return f.Class
	}
	return ""
}

// --- stop ---------------------------------------------------------------------------

func TestStopDrainsThenStopsAndDisablesThenStopsTheVM(t *testing.T) {
	h := newHost(t)
	h.write("stop-makes-stopped", "")
	if err := h.agent.Stop(context.Background(), 60); err != nil {
		t.Fatalf("%v\n%s", err, h.lines())
	}
	calls := h.calls()
	order := []string{"sudo -v", "job drain engage", "systemctl disable --now hermes-gateway@ada.service", "sbx stop ada"}
	last := -1
	for _, call := range order {
		i := strings.Index(calls, call)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order:\n%s", call, calls)
		}
		last = i
	}
	if h.has("job drain cancel") {
		t.Fatal("a successful stop cancelled its drain")
	}
	if !strings.Contains(h.out.String(), "Ada stop: ok in 0s. Stopped; boot will not start it.") {
		t.Fatalf("output:\n%s", h.lines())
	}
}

func TestFailedDrainLeavesTheAgentRunningUntouched(t *testing.T) {
	h := newHost(t)
	h.write("drain-1.json.exit", "1")
	err := h.agent.Stop(context.Background(), 60)
	if failureClass(err) != "drain_failed; the agent is still running" {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(h.calls(), "systemctl disable") || strings.Contains(h.calls(), "sbx stop") {
		t.Fatalf("stopped after a failed drain:\n%s", h.calls())
	}
	if !h.has("job drain cancel") {
		t.Fatal("the failed drain did not clear its marker")
	}
}

func TestFailedStopCancelsTheDrain(t *testing.T) {
	h := newHost(t)
	h.write("systemctl-exit", "1")
	err := h.agent.Stop(context.Background(), 60)
	if failureClass(err) != "gateway_stop_failed" || !h.has("job drain cancel") {
		t.Fatalf("err %v calls:\n%s", err, h.calls())
	}
}

func TestInterruptBetweenDrainAndStopCancelsTheDrain(t *testing.T) {
	h := newHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.agent.afterDrain = cancel
	err := h.agent.Stop(ctx, 60)
	if !errors.Is(err, ErrInterrupted) || !h.has("job drain cancel") || strings.Contains(h.calls(), "systemctl disable") {
		t.Fatalf("err %v calls:\n%s", err, h.calls())
	}
}

func TestAlreadyStoppedAgentIsOnlyDisabled(t *testing.T) {
	h := newHost(t)
	h.write("mainpid", "0\n")
	h.inventory("stopped")
	if err := h.agent.Stop(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	if !h.has("systemctl disable hermes-gateway@ada.service") || strings.Contains(h.calls(), "job drain") {
		t.Fatalf("calls:\n%s", h.calls())
	}
}

func TestSbxStopErrorIsFineWhenTheVMProvesStopped(t *testing.T) {
	h := newHost(t)
	h.write("stop-makes-stopped", "")
	h.write("stop-exit", "1")
	if err := h.agent.Stop(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
}

func TestVMThatNeverStopsFails(t *testing.T) {
	h := newHost(t)
	err := h.agent.Stop(context.Background(), 60)
	if failureClass(err) != "vm_still_running" {
		t.Fatalf("err %v", err)
	}
}

// A live containerd shim for the agent's hostname means the VM is not gone.
func TestLiveShimKeepsTheVMRunning(t *testing.T) {
	h := newHost(t)
	h.write("stop-makes-stopped", "")
	task := filepath.Join(h.dir, "sandboxd/containerd/state/io.containerd.runtime.v2.task/docker/x")
	os.MkdirAll(task, 0o755)
	os.WriteFile(filepath.Join(task, "config.json"), []byte(`{"hostname":"ada"}`), 0o644)
	os.WriteFile(filepath.Join(task, "shim.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644)
	if err := h.agent.Stop(context.Background(), 60); failureClass(err) != "vm_still_running" {
		t.Fatalf("err %v", err)
	}
}

// --- start --------------------------------------------------------------------------

func TestStartEnablesAndWaitsUntilReady(t *testing.T) {
	h := newHost(t)
	if err := h.agent.Start(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, h.lines())
	}
	if !h.has("systemctl enable --now hermes-gateway@ada.service") || !h.has("job ready --platform slack") {
		t.Fatalf("calls:\n%s", h.calls())
	}
	if !strings.Contains(h.out.String(), "Ada start: started. Waiting for the gateway and Slack.") {
		t.Fatalf("output:\n%s", h.lines())
	}
}

func TestStartWithoutSlackDoesNotRequireIt(t *testing.T) {
	h := newHost(t)
	h.agent.Slack = false
	if err := h.agent.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.has("job ready") {
		t.Fatalf("calls:\n%s", h.calls())
	}
}

func TestStartReportsAGatewayThatNeverGetsReady(t *testing.T) {
	h := newHost(t)
	h.write("ready.json", `{"status": "failed", "class": "platform_not_connected"}`)
	h.write("ready-exit", "1")
	if err := h.agent.Start(context.Background()); failureClass(err) != "gateway_not_ready" {
		t.Fatalf("err %v", err)
	}
	h2 := newHost(t)
	h2.write("active", "activating\n")
	if err := h2.agent.Start(context.Background()); failureClass(err) != "gateway_not_ready" || h2.has("job ready --platform slack") {
		t.Fatalf("inactive unit: err %v calls:\n%s", err, h2.calls())
	}
}

func TestStartRefusesBeforeAnyChange(t *testing.T) {
	cases := map[string]struct {
		setup func(h *host)
		class string
	}{
		"missing sandbox":      {func(h *host) { h.write("inventory.json", `[]`) }, "sandbox_missing; create the agent's sandbox first"},
		"unreadable inventory": {func(h *host) { h.write("inventory.json", `nope`) }, "inventory_unreadable"},
		"sudo refused":         {func(h *host) { h.write("sudo-exit", "1") }, "sudo_required"},
		"daemon unhealthy": {func(h *host) {
			h.agent.Probe = func(context.Context) error { return errors.New("down") }
		}, "sandboxd_unhealthy"},
	}
	for title, c := range cases {
		t.Run(title, func(t *testing.T) {
			h := newHost(t)
			c.setup(h)
			err := h.agent.Start(context.Background())
			if failureClass(err) != c.class {
				t.Fatalf("err %v", err)
			}
			if strings.Contains(h.calls(), "systemctl enable") {
				t.Fatalf("changed the unit:\n%s", h.calls())
			}
		})
	}
}

// Passwordless sudo is enough: sudo -v, which sudo-rs answers with a password prompt even under
// NOPASSWD, is never run.
func TestPasswordlessSudoIsNeverVerified(t *testing.T) {
	h := newHost(t)
	h.write("stop-makes-stopped", "")
	h.write("sudo-nopasswd", "")
	h.write("sudo-exit", "1")
	if err := h.agent.Stop(context.Background(), 60); err != nil {
		t.Fatalf("%v\n%s", err, h.lines())
	}
	if h.has("sudo -v") {
		t.Fatalf("verified a passwordless sudo:\n%s", h.calls())
	}
}

func TestLocksAreSharedWithOtherTools(t *testing.T) {
	h := newHost(t)
	held, err := guard.Acquire(h.agent.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.agent.Start(context.Background()); failureClass(err) != "pair_maintenance_busy" {
		t.Fatalf("host lock: %v", err)
	}
	held.Release()
	home, _ := os.UserHomeDir()
	own, err := guard.AcquireFile(filepath.Join(home, ".local/state/sandbox-replacements/ada/maintenance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Release()
	if err := h.agent.Start(context.Background()); failureClass(err) != "maintenance_already_running" {
		t.Fatalf("agent lock: %v", err)
	}
}

// --- drain --------------------------------------------------------------------------

func TestDrainWaitsUntilNothingRunsAndKeepsTheMarker(t *testing.T) {
	h := newHost(t)
	h.drains(busy(2, `"cron 3m", "chat"`), busy(1, `"cron 4m"`), idle)
	if err := h.agent.Drain(context.Background(), 60); err != nil {
		t.Fatalf("%v\n%s", err, h.lines())
	}
	if h.has("job drain cancel") {
		t.Fatal("a finished drain cleared its marker")
	}
	out := h.out.String()
	for _, want := range []string{
		"Ada drain: started. Refusing new work and waiting for current work to finish (up to 60 minutes).",
		"Ada drain: waiting. 2 unit(s) still running (cron 3m, chat), 60 minutes left.",
		"Ada drain: waiting. 1 unit(s) still running, 60 minutes left.",
		"Ada drain: ok in 0s. Nothing is running. The gateway stays drained until it is stopped.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// An idle gateway whose first status predates the marker is proven within the quick polls,
// never after a full Poll.
func TestDrainPollsQuicklyUntilTheStatusIsFresh(t *testing.T) {
	h := newHost(t)
	h.agent.Timing.Drain = DrainTiming{Poll: time.Hour, Refresh: time.Hour, Note: time.Hour, First: time.Millisecond, FirstTries: 3}
	h.drains(`{"running": true, "marker": true, "fresh": false, "state": "draining", "active": 0}`, idle)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.agent.Drain(ctx, 60); err != nil {
		t.Fatalf("waited a full poll for an idle gateway: %v\n%s", err, h.lines())
	}
}

func TestDrainNeverCountsAStaleOrUnreadableStatus(t *testing.T) {
	cases := map[string]string{
		"older than the marker": `{"running": true, "marker": true, "fresh": false, "state": "draining", "active": 0}`,
		"not yet draining":      `{"running": true, "marker": true, "fresh": true, "state": "running", "active": 0}`,
		"fractional count":      `{"running": true, "marker": true, "fresh": true, "state": "draining", "active": 0.5}`,
		"negative count":        `{"running": true, "marker": true, "fresh": true, "state": "draining", "active": -1}`,
		"running unknown":       `{"marker": true, "fresh": true, "state": "draining", "active": 0}`,
	}
	for title, status := range cases {
		t.Run(title, func(t *testing.T) {
			h := newHost(t)
			h.drains(status)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := h.agent.Drain(ctx, 60); err == nil {
				t.Fatal("drained on a status that proves nothing")
			}
			if !h.has("job drain cancel") {
				t.Fatal("the marker was left behind")
			}
		})
	}
}

func TestDrainFailures(t *testing.T) {
	cases := map[string]struct {
		setup   func(h *host)
		message string
	}{
		"cap reached":       {func(h *host) { h.drains(busy(1, `"cron 61m"`)) }, "Work is still running after 1 minutes. The gateway resumed accepting work."},
		"status unreadable": {func(h *host) { h.drains(busy(1, ""), "garbage") }, "The gateway status was unreadable."},
		"gateway stopped while draining": {func(h *host) {
			h.drains(busy(1, ""), `{"running": false}`)
		}, "The gateway stopped while draining."},
		"first call failed": {func(h *host) { h.write("drain-1.json.exit", "1") }, "Could not write the drain marker."},
		"later call failed": {func(h *host) {
			h.drains(busy(1, ""), busy(1, ""))
			h.write("drain-2.json.exit", "1")
		}, "Could not read the gateway status."},
	}
	for title, c := range cases {
		t.Run(title, func(t *testing.T) {
			h := newHost(t)
			c.setup(h)
			var err error
			if title == "cap reached" {
				err = drainWithElapsedCap(h)
			} else {
				err = h.agent.Drain(context.Background(), 60)
			}
			if !errors.Is(err, ErrDrainFailed) {
				t.Fatalf("err %v", err)
			}
			if !strings.Contains(h.err.String(), c.message) {
				t.Fatalf("stderr %q, want %q", h.err.String(), c.message)
			}
			if !h.has("job drain cancel") {
				t.Fatal("the marker was left behind")
			}
		})
	}
}

// drainWithElapsedCap runs a one-minute drain whose clock has already run out.
func drainWithElapsedCap(h *host) error {
	old := since
	since = func(t time.Time) time.Duration { return time.Hour }
	defer func() { since = old }()
	return h.agent.Drain(context.Background(), 1)
}

func TestAgentWrittenStatusCannotReachTheTerminalRaw(t *testing.T) {
	h := newHost(t)
	h.drains(busy(2, `"cron 3m", "\u001b[2Jx", "rm -rf /", "verylongkindofworkname"`), idle)
	if err := h.agent.Drain(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "2 unit(s) still running (cron 3m), 60 minutes left.") || strings.Contains(h.lines(), "\x1b") {
		t.Fatalf("output:\n%q", h.lines())
	}
}

func TestLongDrainRefreshesTheMarker(t *testing.T) {
	h := newHost(t)
	h.agent.Timing.Drain.Refresh = 0
	h.drains(busy(1, ""), busy(1, ""), idle)
	if err := h.agent.Drain(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	if strings.Count(h.calls(), "job drain engage") != 3 || strings.Contains(h.calls(), "job drain status") {
		t.Fatalf("calls:\n%s", h.calls())
	}
}

func TestDrainNeverBootsAStoppedVM(t *testing.T) {
	h := newHost(t)
	h.inventory("stopped")
	if err := h.agent.Drain(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.calls(), "job ") || !strings.Contains(h.out.String(), "drain: skipped. The sandbox is not running") {
		t.Fatalf("calls:\n%s\noutput:\n%s", h.calls(), h.lines())
	}
}

func TestStoppedGatewayClearsTheMarkerItJustWrote(t *testing.T) {
	h := newHost(t)
	h.drains(`{"running": false}`)
	if err := h.agent.Drain(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	if !h.has("job drain cancel") || !strings.Contains(h.out.String(), "drain: skipped. The gateway is not running") {
		t.Fatalf("calls:\n%s", h.calls())
	}
}

func TestInterruptedDrainClearsTheMarker(t *testing.T) {
	h := newHost(t)
	h.agent.Timing.Drain.Poll = time.Hour
	h.drains(busy(1, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.agent.Drain(ctx, 60); !errors.Is(err, ErrInterrupted) || !h.has("job drain cancel") {
		t.Fatalf("err %v calls:\n%s", err, h.calls())
	}
}

func TestCancelOnlyClears(t *testing.T) {
	h := newHost(t)
	if err := h.agent.CancelDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.calls(), "job drain engage") || !h.has("job drain cancel") {
		t.Fatalf("calls:\n%s", h.calls())
	}
}

func TestSuspendedAgentIsNotDrained(t *testing.T) {
	h := newHost(t)
	os.MkdirAll(h.agent.Paths.Pairs, 0o700)
	os.WriteFile(filepath.Join(h.agent.Paths.Pairs, "ada.json"), []byte(`{"phase":"copying"}`), 0o600)
	if err := h.agent.Drain(context.Background(), 60); !errors.Is(err, ErrDrainFailed) || strings.Contains(h.calls(), "job ") {
		t.Fatalf("err %v calls:\n%s", err, h.calls())
	}
}

// --- gateway stop -------------------------------------------------------------------

func TestGatewayStop(t *testing.T) {
	h := newHost(t)
	if err := h.agent.GatewayStop(context.Background()); err != nil || !strings.Contains(h.out.String(), "stopped cleanly") {
		t.Fatalf("err %v output %s", err, h.lines())
	}
	h = newHost(t)
	h.write("stop.json", `{"status": "passed", "state": "already_stopped"}`)
	if err := h.agent.GatewayStop(context.Background()); err != nil || !strings.Contains(h.out.String(), "already stopped") {
		t.Fatalf("err %v output %s", err, h.lines())
	}
	h = newHost(t)
	h.write("stop.json", `{"status": "failed", "class": "gateway_still_running"}`)
	h.write("stop-gateway-exit", "1")
	if err := h.agent.GatewayStop(context.Background()); failureClass(err) != "guest_stop_failed" {
		t.Fatalf("err %v", err)
	}
	h = newHost(t)
	h.agent.Probe = func(context.Context) error { return errors.New("orphan daemon") }
	if err := h.agent.GatewayStop(context.Background()); failureClass(err) != "sandboxd_unhealthy" || strings.Contains(h.calls(), "job ") {
		t.Fatalf("err %v calls:\n%s", err, h.calls())
	}
}
