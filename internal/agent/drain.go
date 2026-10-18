package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
)

// DrainTiming is how often a drain polls, rewrites its marker and repeats an unchanged note.
// Until the gateway's status proves the drain (it postdates the marker), the drain polls every
// First, at most FirstTries times: an idle gateway then answers within seconds, not a Poll.
type DrainTiming struct {
	Poll, Refresh, Note, First time.Duration
	FirstTries                 int
}

// DefaultDrainTiming: Hermes ignores a marker older than an hour, so rewriting it every 20
// minutes keeps it alive.
func DefaultDrainTiming() DrainTiming {
	return DrainTiming{Poll: 30 * time.Second, Refresh: 20 * time.Minute, Note: 5 * time.Minute,
		First: 2 * time.Second, FirstTries: 10}
}

// ErrDrainFailed means the drain printed why it failed and left the agent accepting work.
var ErrDrainFailed = errors.New("drain failed")

const drainCall = 60 * time.Second

// since is time.Since; tests replace it to reach the cap without waiting an hour.
var since = time.Since

// workLabel is the only shape of work label that may reach the terminal: the status file is
// written inside the agent's VM.
var workLabel = regexp.MustCompile(`^[a-z]{1,16}( [0-9]{1,5}m)?$`)

// precheck probes sandboxd, refuses a suspended agent and reports whether its VM runs; a
// drain never execs into a stopped VM, which would boot it.
func (a *Agent) precheck(ctx context.Context) (running bool, err error) {
	if err := a.probe(ctx); err != nil {
		if ctx.Err() != nil {
			return false, ErrInterrupted
		}
		a.Printer.Print("drain", progress.Failed, "The sandbox daemon is not healthy")
		return false, ErrDrainFailed
	}
	if guard.Suspended(a.Paths, a.Name) {
		a.Printer.Print("drain", progress.Failed, a.Name+" may not run now")
		return false, ErrDrainFailed
	}
	records, err := a.Runner.Inventory(ctx)
	if err != nil {
		a.Printer.Print("drain", progress.Failed, "The sandbox inventory is unreadable")
		return false, ErrDrainFailed
	}
	return records[a.Name].Status == "running", nil
}

// Drain makes the gateway refuse new work and waits until its current work is done, up to
// capMinutes. On success the marker stays, so the stop that follows finds the gateway idle
// (the stop clears it). On any failure or interruption the marker is cleared, so the agent
// resumes work.
func (a *Agent) Drain(ctx context.Context, capMinutes int) (err error) {
	running, err := a.precheck(ctx)
	if err != nil {
		return err
	}
	if !running {
		a.Printer.Print("drain", progress.Skipped, "The sandbox is not running, so nothing is in progress")
		return nil
	}
	// Engaged before the first call: the call writes the marker before it reads the status,
	// so a failure after the write must still clear it.
	engaged := true
	defer func() {
		if err != nil && engaged {
			a.releaseDrain()
		}
	}()
	status, callErr := a.call(ctx, drainCall, "drain", "engage")
	if callErr != nil {
		if errors.Is(callErr, ErrInterrupted) {
			return ErrInterrupted
		}
		a.Printer.Print("drain", progress.Failed, "Could not write the drain marker")
		return ErrDrainFailed
	}
	// Only an explicit "not running" skips; unreadable output fails in the loop below.
	if status["running"] == false {
		a.releaseDrain() // a later gateway start in this boot would otherwise begin drained
		engaged = false
		a.Printer.Print("drain", progress.Skipped, "The gateway is not running, so nothing is in progress")
		return nil
	}

	timing := a.Timing.Drain
	limit := time.Duration(capMinutes) * time.Minute
	a.Printer.Print("drain", progress.Started,
		fmt.Sprintf("Refusing new work and waiting for current work to finish (up to %d minutes)", capMinutes))
	started, refreshed, noted := time.Now(), time.Now(), time.Now()
	lastActive, quick := -1, timing.FirstTries
	for {
		if active, ok := drained(status); ok {
			if active == 0 {
				a.Printer.Print("drain", progress.OK, "Nothing is running. The gateway stays drained until it is stopped")
				engaged = false
				return nil
			}
			if active != lastActive || since(noted) >= timing.Note {
				detail := ""
				if lastActive < 0 {
					shown := labels(status)
					if len(shown) > 10 {
						shown = append(shown[:10], fmt.Sprintf("%d more", len(shown)-10))
					}
					detail = " (" + strings.Join(shown, ", ") + ")"
				}
				left := capMinutes - int(since(started)/time.Minute)
				a.Printer.Print("drain", progress.Waiting, fmt.Sprintf("%d unit(s) still running%s, %d minutes left", active, detail, left))
				lastActive, noted = active, time.Now()
			}
		} else if status["running"] == false {
			a.Printer.Print("drain", progress.Failed, "The gateway stopped while draining")
			return ErrDrainFailed
		} else if status == nil {
			a.Printer.Print("drain", progress.Failed, "The gateway status was unreadable")
			return ErrDrainFailed
		}
		if since(started) >= limit {
			a.Printer.Print("drain", progress.Failed,
				fmt.Sprintf("Work is still running after %d minutes. The gateway resumed accepting work", capMinutes))
			return ErrDrainFailed
		}
		wait := timing.Poll
		if _, proven := drained(status); !proven && quick > 0 && timing.First > 0 {
			wait, quick = timing.First, quick-1
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
		action, message := "status", "Could not read the gateway status"
		if since(refreshed) >= timing.Refresh {
			action, message = "engage", "Could not refresh the drain marker"
		}
		next, callErr := a.call(ctx, drainCall, "drain", action)
		if callErr != nil {
			if errors.Is(callErr, ErrInterrupted) {
				return ErrInterrupted
			}
			a.Printer.Print("drain", progress.Failed, message)
			return ErrDrainFailed
		}
		if action == "engage" {
			refreshed = time.Now()
		}
		// An unreadable answer is a nil status, which fails the next round with its own message.
		status = next
	}
}

// drained reads a status that proves the drain: the gateway runs with the marker, the status
// postdates the marker, says draining, and counts active work as a whole number.
func drained(status map[string]any) (int, bool) {
	if status == nil || status["running"] != true || status["marker"] != true || status["fresh"] != true ||
		status["state"] != "draining" {
		return 0, false
	}
	active, ok := status["active"].(float64)
	if !ok || active < 0 || active != float64(int(active)) {
		return 0, false
	}
	return int(active), true
}

func labels(status map[string]any) []string {
	work, _ := status["work"].([]any)
	found := []string{}
	for _, w := range work {
		if label, ok := w.(string); ok && workLabel.MatchString(label) {
			found = append(found, label)
		}
	}
	return found
}

// CancelDrain clears the marker so the gateway accepts work again; it skips a stopped VM.
func (a *Agent) CancelDrain(ctx context.Context) error {
	running, err := a.precheck(ctx)
	if err != nil {
		return err
	}
	if !running {
		a.Printer.Print("drain", progress.Skipped, "The sandbox is not running, so nothing is in progress")
		return nil
	}
	if _, err := a.call(ctx, drainCall, "drain", "cancel"); err != nil {
		if errors.Is(err, ErrInterrupted) {
			return err
		}
		a.Printer.Print("drain", progress.Failed, "Could not clear the drain marker")
		return ErrDrainFailed
	}
	a.Printer.Print("drain", progress.OK, "The gateway accepts new work again")
	return nil
}

// releaseDrain clears a marker this command wrote, even after an interruption: it runs
// detached from the terminal, so a second Ctrl+C cannot cut it short.
func (a *Agent) releaseDrain() {
	cleanup := *a
	cleanup.Runner.Detached = true
	if _, err := cleanup.call(context.Background(), drainCall, "drain", "cancel"); err != nil {
		a.Printer.Print("drain", progress.Failed, "Could not clear the drain marker. Run: troupe drain "+a.Name+" --cancel")
	}
}
