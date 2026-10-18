package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
)

// liveChecks holds the host lock and makes sure the agent may be changed now: sandboxd is
// healthy, the agent is not suspended, its gateway runs, and every earlier graceful restart
// has been confirmed. The returned function releases the lock.
func (a *Agent) liveChecks(ctx context.Context) (func(), error) {
	lock, err := guard.Acquire(a.Paths)
	if err != nil {
		return nil, Failure{"preflight", "pair_maintenance_busy"}
	}
	release := func() { lock.Release() }
	fail := func(class string) (func(), error) {
		release()
		return nil, Failure{"preflight", class}
	}
	if err := a.probe(ctx); err != nil {
		return fail("sandboxd_unhealthy")
	}
	if guard.Suspended(a.Paths, a.Name) {
		return fail("suspended; " + a.Name + " may not run now")
	}
	if !a.active() {
		return fail(a.Unit() + " is not running; start it with: troupe start " + a.Name)
	}
	if err := a.confirmOutstanding(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Restart requests Hermes's graceful restart with no file change and waits until it is confirmed.
func (a *Agent) Restart(ctx context.Context) error {
	release, err := a.liveChecks(ctx)
	if err != nil {
		return err
	}
	defer release()
	receipt, err := newReceipt(a.Name)
	if err != nil {
		return err
	}
	a.Runner.Log = filepath.Join(receipt, "native.log")
	plan := &Plan{ID: "restart", Agent: a.Name, Restart: true, Files: map[string]*fileChange{},
		Removals: []string{}, AddedSkills: []string{}, RemovedSkills: []string{}}
	if err := writeJSON(filepath.Join(receipt, "plan.json"), plan); err != nil {
		return err
	}
	if _, err := a.gatewayPID(ctx); err != nil {
		writeSummary(receipt, "failed", "preflight", 1)
		return err
	}
	return a.restartAndConfirm(ctx, receipt, plan)
}

// gatewayPID is the running gateway's PID; it refuses a gateway that is not running or is
// already restarting.
func (a *Agent) gatewayPID(ctx context.Context) (int, error) {
	state, err := a.guestJSON(ctx, 120*time.Second, "state", map[string]any{"runtime": true})
	if errors.Is(err, ErrInterrupted) {
		return 0, err
	}
	runtime, _ := state["runtime"].(map[string]any)
	pid, _ := runtime["pid"].(float64)
	if err != nil || pid == 0 || runtime["gateway_state"] != "running" {
		return 0, Failure{"restart", "gateway_not_running; nothing changed"}
	}
	if requested, _ := runtime["restart_requested"].(bool); requested {
		return 0, Failure{"restart", "restart_already_pending; nothing changed"}
	}
	return int(pid), nil
}

// restartAndConfirm records what runs now, sends Hermes's graceful restart (active turns finish,
// new messages are queued, the process exits and its unit starts it again), then waits for it.
func (a *Agent) restartAndConfirm(ctx context.Context, receipt string, plan *Plan) error {
	oldPID, err := a.gatewayPID(ctx)
	if err != nil {
		writeSummary(receipt, "failed", "restart", 1)
		return err
	}
	restarts := a.unitProperty("NRestarts")
	if err := writePrivate(filepath.Join(receipt, "others-before.txt"), []byte(othersSnapshot(a.Unit()))); err != nil {
		return err
	}
	// What runs now must finish rather than be cut off; the confirmation checks each one.
	read, err := a.guestRead(ctx, map[string]any{})
	if err != nil {
		writeSummary(receipt, "failed", "restart", 1)
		return Failure{"restart", "guest_read_failed; nothing restarted"}
	}
	ids := append([]string{}, read.Running...)
	writeJSON(filepath.Join(receipt, "running.json"), ids)
	env := fmt.Sprintf("restarts=%s\nold_pid=%d\nstart_epoch=%d\n", restarts, oldPID, time.Now().Unix())
	answer, err := a.guestJSON(ctx, 120*time.Second, "signal", map[string]any{"pid": oldPID})
	writeJSON(filepath.Join(receipt, "signal.json"), answer)
	if err != nil || answer["status"] != "passed" {
		writeSummary(receipt, "failed", "restart", 1)
		return Failure{"restart", "signal_refused; nothing restarted"}
	}
	// Only a restart that was really requested is tracked, so a refused signal never leaves a
	// receipt that later commands would wait on.
	if err := writePrivate(filepath.Join(receipt, "restart.env"), []byte(env)); err != nil {
		return err
	}
	writeSummary(receipt, "signalled", "signalled", 0)
	a.Printer.Print("restart", progress.Started, fmt.Sprintf("%s finishes active turns first; new messages wait", a.Printer.Agent))
	return a.confirm(ctx, receipt, a.Timing.Restart.Wait, "restart")
}

// ErrPending means the graceful restart was requested but active work is still finishing; it
// happens on its own and the next update or restart confirms it.
var ErrPending = errors.New("restart pending")

// confirmOutstanding confirms every earlier graceful restart that was never confirmed. One still
// pending or failed blocks new changes; one that cut work off is reported once.
func (a *Agent) confirmOutstanding(ctx context.Context) error {
	dirs, _ := filepath.Glob(filepath.Join(receiptsDir(a.Name), "*", "restart.env"))
	sort.Strings(dirs)
	for _, env := range dirs {
		receipt := filepath.Dir(env)
		status := readSummary(receipt)["status"]
		if status == "passed" || status == "interrupted" {
			continue
		}
		a.Printer.Print("outstanding", progress.Started, "Confirming the earlier graceful restart "+filepath.Base(receipt))
		err := a.confirm(ctx, receipt, a.Timing.Restart.Outstanding, "outstanding")
		var f Failure
		switch {
		case err == nil:
		case errors.Is(err, ErrPending):
			return Failure{"outstanding", "pending; the earlier restart " + filepath.Base(receipt) + " is still finishing active turns. Nothing changed; try again later"}
		case errors.As(err, &f) && strings.HasPrefix(f.Class, "interrupted"):
			a.Printer.Print("outstanding", "", "The earlier restart "+filepath.Base(receipt)+" cut off work (recorded in its receipt); continuing")
		case errors.As(err, &f):
			return Failure{"outstanding", fmt.Sprintf("%s; the earlier restart %s failed. Nothing changed; its receipt is %s", f.Class, filepath.Base(receipt), receipt)}
		default:
			return err
		}
	}
	return nil
}

// confirm waits up to wait for the restart a receipt requested, then checks it: a new gateway
// process, connected and stable for the settle time; right after the restart also sandboxd and
// other agents' holders unchanged and the planned files in place; no running work cut off; and,
// when skills were added or removed, a new session's skill index. A restart still waiting on
// active turns is pending (ErrPending), not failed.
func (a *Agent) confirm(ctx context.Context, receipt string, wait time.Duration, step string) error {
	env := readSummaryFile(filepath.Join(receipt, "restart.env"))
	oldPID, _ := strconv.Atoi(env["old_pid"])
	startEpoch, _ := strconv.ParseInt(env["start_epoch"], 10, 64)
	var plan Plan
	if data, err := os.ReadFile(filepath.Join(receipt, "plan.json")); err == nil {
		json.Unmarshal(data, &plan)
	}
	timing := a.Timing.Restart
	finish := func(status, stage string, code int, err error) error {
		writeSummary(receipt, status, stage, code)
		return err
	}

	// The evidence is a new gateway process: systemd's restart count resets when a unit is
	// stopped and started, which is a restart too.
	deadline := time.Now().Add(wait)
	for {
		if a.active() && a.restarted(ctx, oldPID) {
			break
		}
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		if time.Now().After(deadline) {
			if a.active() && a.gatewayStill(ctx, oldPID) {
				a.Printer.Print(step, progress.Waiting, "Active turns are still finishing before the restart. The next troupe update or restart confirms it")
				return finish("pending", "pending", 3, ErrPending)
			}
			return finish("failed", "verify", 1, Failure{step, "gateway_did_not_return; check " + a.Unit()})
		}
		if err := sleep(ctx, timing.Poll); err != nil {
			return err
		}
	}
	pid := a.unitProperty("MainPID")
	if err := sleep(ctx, timing.Settle); err != nil {
		return err
	}
	if !a.active() || a.unitProperty("MainPID") != pid || !a.restarted(ctx, oldPID) {
		return finish("failed", "verify", 1, Failure{step, "gateway_not_stable"})
	}
	// Right after the restart, nothing else may have changed and the planned files must be in
	// place. A restart confirmed later skips both: much may rightly have changed since.
	if step == "restart" {
		before, _ := os.ReadFile(filepath.Join(receipt, "others-before.txt"))
		after := othersSnapshot(a.Unit())
		writePrivate(filepath.Join(receipt, "others-after.txt"), []byte(after))
		if string(before) != after {
			return finish("failed", "verify", 1, Failure{step, "sandboxd_or_another_holder_changed"})
		}
		if len(plan.Files) > 0 {
			if err := a.verifyFiles(ctx, &plan); err != nil {
				return finish("failed", "verify", 1, err)
			}
		}
	}

	var ids []string
	if data, err := os.ReadFile(filepath.Join(receipt, "running.json")); err == nil {
		json.Unmarshal(data, &ids)
	}
	drain, err := a.guestJSON(ctx, 120*time.Second, "state", map[string]any{"executions": ids, "log_since": startEpoch})
	if err != nil {
		return finish("failed", "verify", 1, Failure{step, "guest_state_failed"})
	}
	statuses, _ := drain["executions"].(map[string]any)
	var cut []string
	for _, id := range ids {
		if s := statuses[id]; s != "completed" && s != "failed" {
			cut = append(cut, id)
		}
	}
	forced, _ := drain["forced_drains"].(float64)
	if len(cut) > 0 || forced > 0 {
		writeJSON(filepath.Join(receipt, "interrupted.json"), map[string]any{"executions": cut, "forced_drain_log_lines": forced})
		message := fmt.Sprintf("interrupted; the restart cut off work: %d running execution(s), %d forced drain log line(s). Hermes recovers interrupted cron runs as unknown; check the affected work item", len(cut), int(forced))
		return finish("interrupted", "interrupted", 4, Failure{step, message})
	}

	// The skill index check only proves added skills appear and removed ones disappear; with
	// neither, a new session would prove nothing, and an idle agent may start none for hours.
	if len(plan.AddedSkills) == 0 && len(plan.RemovedSkills) == 0 {
		a.Printer.Print(step, progress.OK, fmt.Sprintf("Restarted once, connected, stable for %s; %d running turn(s) finished",
			progress.Duration(timing.Settle), len(ids)))
		return finish("passed", "done", 0, nil)
	}
	indexDeadline := time.Now().Add(timing.Index)
	var session map[string]any
	for {
		state, err := a.guestJSON(ctx, 120*time.Second, "state", map[string]any{"since": startEpoch})
		if err == nil {
			session, _ = state["session"].(map[string]any)
		}
		if session != nil {
			break
		}
		if time.Now().After(indexDeadline) {
			return finish("failed", "verify", 1, Failure{step, "no_new_session_for_index_check"})
		}
		if err := sleep(ctx, timing.IndexPoll); err != nil {
			return err
		}
	}
	prompt := fmt.Sprint(session["prompt"])
	for _, name := range plan.AddedSkills {
		if !indexed(prompt, name) {
			return finish("failed", "verify", 1, Failure{step, "skill_not_indexed " + name})
		}
	}
	for _, name := range plan.RemovedSkills {
		if indexed(prompt, name) {
			return finish("failed", "verify", 1, Failure{step, "skill_still_indexed " + name})
		}
	}
	// The session source is written inside the agent's VM: only a plain word reaches the terminal.
	source, _ := session["source"].(string)
	if !plainWord.MatchString(source) {
		source = "new"
	}
	a.Printer.Print(step, progress.OK, fmt.Sprintf("Restarted once, connected, stable for %s; %d running turn(s) finished; skill index checked in a %s session",
		progress.Duration(timing.Settle), len(ids), source))
	return finish("passed", "done", 0, nil)
}

// gatewayStill: the gateway that was asked to restart is still the one running.
func (a *Agent) gatewayStill(ctx context.Context, oldPID int) bool {
	state, err := a.guestJSON(ctx, 120*time.Second, "state", map[string]any{"runtime": true})
	if err != nil {
		return false
	}
	runtime, _ := state["runtime"].(map[string]any)
	pid, _ := runtime["pid"].(float64)
	return int(pid) == oldPID
}

// restarted: a different gateway process runs with every platform connected.
func (a *Agent) restarted(ctx context.Context, oldPID int) bool {
	state, err := a.guestJSON(ctx, 120*time.Second, "state", map[string]any{"runtime": true})
	if err != nil {
		return false
	}
	runtime, _ := state["runtime"].(map[string]any)
	pid, _ := runtime["pid"].(float64)
	platforms, _ := runtime["platforms"].(map[string]any)
	if pid == 0 || int(pid) == oldPID || runtime["gateway_state"] != "running" || len(platforms) == 0 {
		return false
	}
	for _, s := range platforms {
		if s != "connected" {
			return false
		}
	}
	return true
}

var plainWord = regexp.MustCompile(`^[a-z][a-z_-]{0,19}$`)

func indexed(prompt, skill string) bool {
	return regexp.MustCompile(`(?m)^\s*- ` + regexp.QuoteMeta(skill) + `:`).MatchString(prompt)
}

func (a *Agent) unitProperty(name string) string {
	out, _ := exec.Command("systemctl", "show", a.Unit(), "-p", name, "--value").Output()
	return strings.TrimSpace(string(out))
}

// othersSnapshot is sandboxd and every other holder unit's identity: an update must not touch
// them. A holder, from any agent tool on this host, is a PREFIX@NAME.service whose start is
// gated by an ExecCondition ending in `check NAME`, as troupe's own gateways are; self, the
// agent's own gateway, restarts by design.
func othersSnapshot(self string) string {
	units := []string{"sbx-daemon.service"}
	out, _ := exec.Command("systemctl", "list-units", "--all", "--type=service", "--plain", "--no-legend", "*@*.service").Output()
	var templated []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] != self {
			templated = append(templated, fields[0])
		}
	}
	if len(templated) > 0 {
		show, _ := exec.Command("systemctl", append(append([]string{"show"}, templated...), "-p", "Id", "-p", "ExecCondition")...).Output()
		for _, block := range strings.Split(string(show), "\n\n") {
			id, condition := "", ""
			for _, line := range strings.Split(block, "\n") {
				if v, ok := strings.CutPrefix(line, "Id="); ok {
					id = v
				} else if v, ok := strings.CutPrefix(line, "ExecCondition="); ok {
					condition = v
				}
			}
			if id != self && guardedHolder(id, condition) {
				units = append(units, id)
			}
		}
	}
	show, _ := exec.Command("systemctl", append([]string{"show"}, append(units, "-p", "Id", "-p", "MainPID", "-p", "ActiveState")...)...).Output()
	return string(show)
}

// guardedHolder reports whether unit, PREFIX@NAME.service, is gated by a condition whose
// command line, as systemctl show prints it ({ path=... ; argv[]=PROGRAM ARGS ; ... }), ends
// in `check NAME`.
func guardedHolder(unit, condition string) bool {
	_, instance, templated := strings.Cut(unit, "@")
	name, service := strings.CutSuffix(instance, ".service")
	if !templated || !service || name == "" {
		return false
	}
	_, argv, found := strings.Cut(condition, "argv[]=")
	if !found {
		return false
	}
	argv, _, _ = strings.Cut(argv, " ;")
	return strings.HasSuffix(argv, " check "+name)
}

func readSummary(receipt string) map[string]string {
	return readSummaryFile(filepath.Join(receipt, "summary.txt"))
}

// readSummaryFile reads KEY=VALUE lines (summary.txt, restart.env).
func readSummaryFile(file string) map[string]string {
	values := map[string]string{}
	data, err := os.ReadFile(file)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `'"`)
		}
	}
	return values
}
