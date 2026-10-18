package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restartHost adds a gateway stand-in to an updateHost: a sleep process the real guest job
// signals, a systemctl that counts unit restarts, and a state.db with a session whose skill
// index the confirmation reads.
type restartHost struct {
	*updateHost
	stubs string
}

const fakeRestartSystemctl = `#!/bin/sh
d=$(dirname "$0")/..
case "$1" in
  is-active) echo active ;;
  list-units) ;;
  show)
    case "$*" in
      *NRestarts*) cat "$d/restarts" ;;
      *MainPID*--value*) cat "$d/mainpid" ;;
      *) echo "Id=sbx-daemon.service"; echo "MainPID=42"; echo "ActiveState=active" ;;
    esac ;;
esac
`

func newRestartHost(t *testing.T) *restartHost {
	t.Helper()
	h := &restartHost{updateHost: newUpdateHost(t), stubs: os.Getenv("TROUPE_TEST_STUBS")}
	h.writeAt(filepath.Join(h.dir, "sysbin/systemctl"), fakeRestartSystemctl)
	os.Chmod(filepath.Join(h.dir, "sysbin/systemctl"), 0o755)
	t.Setenv("PATH", filepath.Join(h.dir, "sysbin")+":"+os.Getenv("PATH"))
	h.writeAt(filepath.Join(h.dir, "restarts"), "0\n")
	h.writeAt(filepath.Join(h.dir, "mainpid"), "100\n")
	h.writeAt(filepath.Join(h.stubs, "runtime.json"), `{"gateway_state": "running", "platforms": {"slack": {"state": "connected"}}}`)
	h.agent.Paths.Pairs = filepath.Join(h.dir, "pairs")
	h.agent.Timing.Restart = RestartTiming{Wait: 5 * time.Second, Outstanding: 200 * time.Millisecond,
		Poll: 20 * time.Millisecond, Settle: 20 * time.Millisecond, Index: 2 * time.Second, IndexPoll: 20 * time.Millisecond}
	h.session("- review: Review PRs.")
	return h
}

// gateway starts a stand-in gateway; when the guest job's SIGUSR1 ends it, systemd's restart
// is simulated: a new process, a counted restart and a new main PID.
func (h *restartHost) gateway(restartOnSignal bool) {
	h.t.Helper()
	old := exec.Command("sleep", "300")
	if err := old.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.writeAt(filepath.Join(h.stubs, "pid"), fmt.Sprint(old.Process.Pid))
	stopped, done := make(chan struct{}), make(chan struct{})
	var replacement *exec.Cmd
	go func() {
		defer close(done)
		old.Wait()
		select {
		case <-stopped: // the test ended; write nothing into its removed folders
			return
		default:
		}
		if !restartOnSignal {
			return
		}
		replacement = exec.Command("sleep", "300")
		replacement.Start()
		h.writeAt(filepath.Join(h.stubs, "pid"), fmt.Sprint(replacement.Process.Pid))
		h.writeAt(filepath.Join(h.dir, "mainpid"), "101\n")
		h.writeAt(filepath.Join(h.dir, "restarts"), "1\n")
	}()
	h.t.Cleanup(func() {
		close(stopped)
		old.Process.Kill()
		<-done
		if replacement != nil && replacement.Process != nil {
			replacement.Process.Kill()
			replacement.Wait()
		}
	})
}

// session writes a state.db whose newest session (started in the future, so after any
// signal) has a skill index with these lines.
func (h *restartHost) session(index string) {
	h.t.Helper()
	db := filepath.Join(h.live, "state.db")
	os.Remove(db)
	script := `import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
c.execute('create table sessions (id text, source text, started_at real, system_prompt_hash text)')
c.execute('create table system_prompts (hash text, prompt text)')
c.execute("insert into system_prompts values ('h', ?)", (sys.argv[2],))
c.execute("insert into sessions values ('s1', 'cron', 4102444800, 'h')")
c.commit()`
	if out, err := exec.Command("python3", "-c", script, db, "Skills:\n"+index+"\n").CombinedOutput(); err != nil {
		h.t.Fatalf("state.db: %v %s", err, out)
	}
}

func receipts(t *testing.T) []string {
	found, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/*/summary.txt"))
	return found
}

func TestRestartWaitsAndConfirms(t *testing.T) {
	h := newRestartHost(t)
	h.gateway(true)
	if err := h.agent.Restart(context.Background()); err != nil {
		t.Fatalf("%v\n%s%s", err, h.out.String(), h.err.String())
	}
	if !strings.Contains(h.out.String(), "Ada restart: ok") {
		t.Fatalf("output:\n%s", h.out.String())
	}
	if summary := readSummaryFile(receipts(t)[0]); summary["status"] != "passed" {
		t.Fatalf("summary %v", summary)
	}
}

// A restart still finishing work is pending, not failed; the next command confirms it first,
// and refuses while it is still pending.
func TestPendingRestartIsConfirmedByTheNextCommand(t *testing.T) {
	h := newRestartHost(t)
	h.gateway(false) // the gateway ends but its unit never counts a restart
	h.agent.Timing.Restart.Wait = 200 * time.Millisecond
	if err := h.agent.Restart(context.Background()); !errors.Is(err, ErrPending) || h.agent.Report(err) != 3 {
		t.Fatalf("a pending restart is ErrPending and exits 3: %v", err)
	}
	if !strings.Contains(h.out.String(), "restart: waiting") || readSummaryFile(receipts(t)[0])["status"] != "pending" {
		t.Fatalf("output:\n%s", h.out.String())
	}
	err := h.agent.Restart(context.Background())
	if !strings.HasPrefix(failureClass(err), "pending") {
		t.Fatalf("next command while pending: %v", err)
	}
	// The restart completes, here through a stop and start that reset systemd's restart count:
	// a new gateway process is the evidence, and the next command confirms it first.
	h.writeAt(filepath.Join(h.dir, "restarts"), "0\n")
	h.writeAt(filepath.Join(h.dir, "mainpid"), "101\n")
	h.writeAt(filepath.Join(h.stubs, "pid"), fmt.Sprint(os.Getpid())) // a new, running gateway
	h.out.Reset()
	if err := h.agent.confirmOutstanding(context.Background()); err != nil || !strings.Contains(h.out.String(), "outstanding: ok") {
		t.Fatalf("earlier restart not confirmed: %v\n%s%s", err, h.out.String(), h.err.String())
	}
	if readSummaryFile(receipts(t)[0])["status"] != "passed" {
		t.Fatal("the earlier receipt was not marked passed")
	}
}

func TestCutOffWorkIsReportedAsInterrupted(t *testing.T) {
	h := newRestartHost(t)
	h.writeAt(filepath.Join(h.stubs, "executions.json"), `[{"id": "e1", "job_id": "watch", "status": "running"}]`)
	h.writeAt(filepath.Join(h.stubs, "statuses.json"), `{"e1": "unknown"}`)
	h.gateway(true)
	err := h.agent.Restart(context.Background())
	if !strings.HasPrefix(failureClass(err), "interrupted") || readSummaryFile(receipts(t)[0])["status"] != "interrupted" {
		t.Fatalf("err %v", err)
	}
	// Reported once: the next command does not block on it.
	h.writeAt(filepath.Join(h.stubs, "executions.json"), `[]`)
	h.gateway(true)
	h.writeAt(filepath.Join(h.dir, "restarts"), "1\n")
	if err := h.agent.confirmOutstanding(context.Background()); err != nil {
		t.Fatalf("an interrupted earlier restart blocked: %v", err)
	}
}

func TestIndexMustShowAddedSkills(t *testing.T) {
	h := newRestartHost(t)
	h.installAll()
	h.seed("base", "skills/triage/SKILL.md", "---\nname: triage\ndescription: Triage.\n---\n")
	h.gateway(true)
	err := h.agent.applyPlan(context.Background(), h.plan(Skills))
	if failureClass(err) != "skill_not_indexed triage" {
		t.Fatalf("err %v", err)
	}
	h2 := newRestartHost(t)
	h2.installAll()
	h2.seed("base", "skills/triage/SKILL.md", "---\nname: triage\ndescription: Triage.\n---\n")
	h2.session("- review: Review PRs.\n- triage: Triage.")
	h2.gateway(true)
	if err := h2.agent.applyPlan(context.Background(), h2.plan(Skills)); err != nil {
		t.Fatalf("%v\n%s", err, h2.err.String())
	}
}

// A receipt the earlier bash operator left (its plan.json and restart.env formats) is still
// confirmed before new changes.
func TestEarlierOperatorReceiptIsConfirmed(t *testing.T) {
	h := newRestartHost(t)
	receipt := filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/20261001T120000Z")
	h.writeAt(filepath.Join(receipt, "plan.json"), `{"id": "a1b2c3d4e5f6", "agent": "ada", "commit": "abc", "restart": true,
  "files": {}, "removals": [], "added_skills": [], "removed_skills": [], "notes": [], "running": [], "harvest": {}}`)
	h.writeAt(filepath.Join(receipt, "restart.env"), "restarts=0\nold_pid=100\nstart_epoch=1759320000\n")
	h.writeAt(filepath.Join(receipt, "summary.txt"), "status=signalled\nstage=signalled\nexit_status=0\n")
	h.writeAt(filepath.Join(receipt, "others-before.txt"), "Id=sbx-daemon.service\nMainPID=42\nActiveState=active\n")
	h.writeAt(filepath.Join(receipt, "running.json"), "[]")
	h.writeAt(filepath.Join(h.dir, "restarts"), "1\n")
	h.writeAt(filepath.Join(h.stubs, "pid"), fmt.Sprint(os.Getpid()))
	if err := h.agent.confirmOutstanding(context.Background()); err != nil {
		t.Fatalf("%v\n%s%s", err, h.out.String(), h.err.String())
	}
	if readSummaryFile(filepath.Join(receipt, "summary.txt"))["status"] != "passed" {
		t.Fatal("the earlier receipt was not confirmed")
	}
}

// A restart that adds or removes no skill needs no new session: an idle agent starts none.
func TestPlainRestartNeedsNoNewSession(t *testing.T) {
	h := newRestartHost(t)
	os.Remove(filepath.Join(h.live, "state.db"))
	h.gateway(true)
	if err := h.agent.Restart(context.Background()); err != nil {
		t.Fatalf("%v\n%s%s", err, h.out.String(), h.err.String())
	}
}

// A refused signal restarts nothing and leaves nothing for later commands to wait on.
func TestRefusedSignalLeavesNothingOutstanding(t *testing.T) {
	h := newRestartHost(t)
	h.gateway(true)
	t.Setenv("TROUPE_TEST_REFUSE_SIGNAL", "1")
	if err := h.agent.Restart(context.Background()); !strings.HasPrefix(failureClass(err), "signal_refused") {
		t.Fatalf("err %v", err)
	}
	if found, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/*/restart.env")); len(found) != 0 {
		t.Fatalf("a refused signal left a tracked restart: %v", found)
	}
	if err := h.agent.confirmOutstanding(context.Background()); err != nil {
		t.Fatalf("a refused signal blocks later commands: %v", err)
	}
}

// A restart confirmed late does not fail because the agent or another holder changed since.
func TestLateConfirmationIgnoresLaterChanges(t *testing.T) {
	h := newRestartHost(t)
	h.gateway(false)
	h.agent.Timing.Restart.Wait = 100 * time.Millisecond
	if err := h.agent.Restart(context.Background()); !errors.Is(err, ErrPending) {
		t.Fatalf("err %v", err)
	}
	// Hours later: the restart happened, and sandboxd's identity changed meanwhile.
	h.writeAt(filepath.Join(h.stubs, "pid"), fmt.Sprint(os.Getpid()))
	h.writeAt(filepath.Join(h.dir, "sysbin/systemctl"), strings.Replace(fakeRestartSystemctl, "MainPID=42", "MainPID=43", 1))
	if err := h.agent.confirmOutstanding(context.Background()); err != nil {
		t.Fatalf("late confirmation failed on a later change: %v", err)
	}
}

// A holder from any agent tool counts when its ExecCondition ends in `check NAME`.
func TestGuardedHolderFollowsTheCheckNameConvention(t *testing.T) {
	for _, c := range []struct {
		unit, condition string
		want            bool
	}{
		{"keep-alive@bea.service", "{ path=/r/bin/sandbox-pair-guard.sh ; argv[]=/r/bin/sandbox-pair-guard.sh check bea ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }", true},
		{"hermes-gateway@cyd.service", "{ path=/h/.local/bin/troupe ; argv[]=/h/.local/bin/troupe guard check cyd ; ignore_errors=no ; status=0/0 }", true},
		{"lookalike@eve.service", "{ path=/x ; argv[]=/x check mallory ; ignore_errors=no }", false},
		{"plain.service", "{ path=/x ; argv[]=/x check plain ; ignore_errors=no }", false},
		{"getty@tty1.service", "", false},
		{"other@bob.service", "{ path=/x ; argv[]=/x check bob --strict ; ignore_errors=no }", false},
	} {
		if got := guardedHolder(c.unit, c.condition); got != c.want {
			t.Errorf("guardedHolder(%s) = %v", c.unit, got)
		}
	}
}

// The snapshot covers sandboxd and every other guarded holder, never the agent's own gateway.
func TestOthersSnapshotSkipsTheAgentsOwnGateway(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte(`#!/bin/sh
case "$1" in
  list-units) printf '%s loaded active running x\n' keep-alive@bea.service hermes-gateway@ada.service getty@tty1.service ;;
  show)
    case "$*" in
      *ExecCondition*) printf 'Id=keep-alive@bea.service\nExecCondition={ path=/r/g ; argv[]=/r/g check bea ; ignore_errors=no }\n\nId=hermes-gateway@ada.service\nExecCondition={ path=/t ; argv[]=/t guard check ada ; ignore_errors=no }\n\nId=getty@tty1.service\nExecCondition=\n' ;;
      *) shift; for u in "$@"; do case "$u" in -p|Id|MainPID|ActiveState) ;; *) echo "Id=$u";; esac; done ;;
    esac ;;
esac
`), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	got := othersSnapshot("hermes-gateway@ada.service")
	if got != "Id=sbx-daemon.service\nId=keep-alive@bea.service\n" {
		t.Fatalf("snapshot %q", got)
	}
}
