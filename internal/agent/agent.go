// Package agent starts, stops and drains one Hermes agent on this host.
//
// Every step mirrors the operator procedure it replaces: one privileged session (sudo asked
// once, kept alive), the host-wide and per-agent maintenance locks, the honest sandboxd probe
// before every sbx call, and guest jobs that call Hermes's own gateway functions.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/sudo"
)

// Agent is one agent and the host it runs on.
type Agent struct {
	Name string
	// Command names the operator command for lines that belong to no step.
	Command string
	Paths   guard.Paths
	Runner  sbx.Runner
	Printer *progress.Printer
	// Slack means the agent's base kit uses Slack, so readiness requires it connected.
	Slack bool
	// Terminal is where sudo asks for the password.
	Terminal io.Reader
	Timing   Timing
	// Probe replaces the sandboxd probe in tests; nil means guard.Probe on Paths.
	Probe func(context.Context) error
	// Free replaces the free-space reading in tests (bytes available under a folder).
	Free func(path string) (int64, error)
	// State holds each agent's transaction receipts; "" means
	// ~/.local/state/sandbox-replacements.
	State string
	// afterDrain lets tests interrupt between a finished drain and the stop.
	afterDrain func()
}

func (a *Agent) probe(ctx context.Context) error {
	if a.Probe != nil {
		return a.Probe(ctx)
	}
	return guard.Probe(ctx, a.Paths)
}

// Timing holds every wait; tests shorten them.
type Timing struct {
	ReadyTries int
	ReadyEvery time.Duration
	StopWait   time.Duration
	StopEvery  time.Duration
	Keepalive  time.Duration
	Drain      DrainTiming
	Restart    RestartTiming
}

// RestartTiming is how long a graceful restart is awaited and confirmed.
type RestartTiming struct {
	// Wait is how long a command waits for its own restart; Outstanding, for an earlier one.
	Wait, Outstanding time.Duration
	Poll, Settle      time.Duration
	// Index is how long to wait for a new session whose skill index can be checked.
	Index, IndexPoll time.Duration
}

// DefaultTiming is the operator procedure's timing.
func DefaultTiming() Timing {
	return Timing{
		ReadyTries: 60, ReadyEvery: 2 * time.Second,
		StopWait: 120 * time.Second, StopEvery: 2 * time.Second,
		Keepalive: 240 * time.Second,
		Drain:     DefaultDrainTiming(),
		Restart: RestartTiming{Wait: 5 * time.Minute, Outstanding: time.Minute, Poll: 5 * time.Second,
			Settle: 30 * time.Second, Index: 3 * time.Minute, IndexPoll: 15 * time.Second},
	}
}

// Failure is a step that failed with a short machine-readable class, printed as
// "<step>: FAILED. <class>. Log: <log>".
type Failure struct {
	Step, Class string
}

func (f Failure) Error() string { return f.Step + ": " + f.Class }

// ErrInterrupted means the operator interrupted the command.
var ErrInterrupted = errors.New("interrupted")

// plainName is the only shape a word from inside the agent's VM may have to reach the terminal.
var plainName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Plain returns a failure word reported by the guest when it is a plain error name, else
// "unknown": the VM is the agent's, and a compromised agent could send terminal control codes.
func Plain(word any) string {
	s, _ := word.(string)
	if plainName.MatchString(s) {
		return s
	}
	return "unknown"
}

// Unit is the agent's gateway unit.
func (a *Agent) Unit() string { return "hermes-gateway@" + a.Name + ".service" }

// LogFile is a private file under troupe's state folder for this agent.
func LogFile(name, file string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "troupe", name, file)
}

// Report prints err as the operator's FAILED line and returns the exit code.
func (a *Agent) Report(err error) int {
	var failure Failure
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrInterrupted):
		a.Printer.Print(a.Command, progress.Failed, "Interrupted")
		return 130
	case errors.Is(err, ErrPending):
		return 3 // the waiting line is already printed
	case errors.As(err, &failure) && strings.HasPrefix(failure.Class, "interrupted"):
		a.Printer.Print(strings.ReplaceAll(failure.Step, "_", " "), progress.Failed, failure.Class)
		return 4
	case errors.As(err, &failure):
		a.Printer.Print(strings.ReplaceAll(failure.Step, "_", " "), progress.Failed,
			fmt.Sprintf("%s. Log: %s", failure.Class, a.Runner.Log))
	default:
		a.Printer.Print(a.Command, progress.Failed, err.Error())
	}
	return 1
}

// session is the privileged, locked context start and stop run in.
type session struct {
	locks []*guard.Lock
	stop  chan struct{}
}

func (s *session) close() {
	if s.stop != nil {
		close(s.stop)
	}
	for i := len(s.locks) - 1; i >= 0; i-- {
		s.locks[i].Release()
	}
}

// begin asks for sudo once (the only interactive privileged step), takes the host-wide and
// per-agent locks shared with every other maintenance tool, keeps sudo alive and probes.
func (a *Agent) begin(ctx context.Context) (*session, error) {
	if err := sudo.Ask(ctx, a.Terminal); err != nil {
		return nil, Failure{"preflight", "sudo_required"}
	}
	s := &session{}
	pairs, err := guard.Acquire(a.Paths)
	if err != nil {
		return nil, Failure{"preflight", "pair_maintenance_busy"}
	}
	s.locks = append(s.locks, pairs)
	own, err := guard.AcquireFile(filepath.Join(a.stateRoot(), a.Name, "maintenance.lock"))
	if err != nil {
		s.close()
		return nil, Failure{"preflight", "maintenance_already_running"}
	}
	s.locks = append(s.locks, own)
	s.stop = make(chan struct{})
	go func(stop chan struct{}, every time.Duration) {
		for {
			select {
			case <-stop:
				return
			case <-time.After(every):
				exec.Command("sudo", "-n", "-v").Run()
			}
		}
	}(s.stop, a.Timing.Keepalive)
	if err := a.probe(ctx); err != nil {
		s.close()
		return nil, Failure{"preflight", "sandboxd_unhealthy"}
	}
	return s, nil
}

// systemctl runs a privileged systemctl call without prompting; its chatter goes to the log.
func (a *Agent) systemctl(args ...string) error {
	log, err := openLog(a.Runner.Log)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command("sudo", append([]string{"-n", "systemctl"}, args...)...)
	cmd.Stdout, cmd.Stderr = log, log
	fmt.Fprintf(log, "$ sudo -n systemctl %s\n", strings.Join(args, " "))
	return cmd.Run()
}

// mainPID is the unit's main PID; 0 means it is not running.
func (a *Agent) mainPID() string {
	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", a.Unit()).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (a *Agent) active() bool {
	out, _ := exec.Command("systemctl", "is-active", a.Unit()).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// unitUp reports whether an agent's gateway unit is active or starting.
// UnitUp reports whether an agent's gateway unit is up (active, activating or reloading): with
// Restart=always it boots the agent's VM again within seconds even while that VM is stopped.
func UnitUp(name string) bool { return unitUp(name) }

func unitUp(name string) bool {
	out, _ := exec.Command("systemctl", "is-active", "hermes-gateway@"+name+".service").Output()
	state := strings.TrimSpace(string(out))
	return state == "active" || state == "activating" || state == "reloading"
}

// vmStopped: the sandbox is absent or stopped, and no VM process is left. No guest exec.
func (a *Agent) vmStopped(ctx context.Context) bool {
	records, err := a.Runner.Inventory(ctx)
	if err != nil {
		return false
	}
	record, exists := records[a.Name]
	return (!exists || record.Status == "stopped") && guard.VMGone(a.Paths, a.Name)
}

// call runs a guest job and decodes its last output line as JSON. The result is nil when that
// line is not a JSON object; the error is the run's own (nonzero exit, timeout, probe), or
// ErrInterrupted when the operator interrupted it.
func (a *Agent) call(ctx context.Context, timeout time.Duration, job string, args ...string) (map[string]any, error) {
	out, err := guest.RunWithin(ctx, a.Runner, timeout, a.Name, job, args...)
	if ctx.Err() != nil {
		return nil, ErrInterrupted
	}
	var result map[string]any
	if json.Unmarshal(lastLine(out), &result) != nil {
		result = nil
	}
	return result, err
}

// lastLine is the last non-empty line, found without copying the whole answer.
func lastLine(out []byte) []byte {
	out = bytes.TrimRight(out, " \t\r\n")
	return out[bytes.LastIndexByte(out, '\n')+1:]
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ErrInterrupted
	case <-time.After(d):
		return nil
	}
}
