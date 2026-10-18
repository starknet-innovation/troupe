package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// updateHost is a kits folder (no version control), a live agent home with troupe's receipts,
// and an sbx that runs the real guest job locally against that home, with stand-ins for
// Hermes's gateway and cron modules.
type updateHost struct {
	t                *testing.T
	dir, repo, live  string
	base, kit        string
	agent            *Agent
	seeds            Seeds
	out, err         bytes.Buffer
	confirmed, asked int
}

const fakeGuestSbx = `#!/bin/sh
d=$(dirname "$0")/..
case "$1" in
  ls) cat "$d/inventory.json" ;;
  exec)
    shift; [ "$1" = -i ] && shift; [ "$1" = --workdir ] && shift 2; shift
    # sh -c SCRIPT sh PYTHON HERMES JOB ARGS...: run the job with a stand-in Hermes folder whose
    # package manager names this host's python3.
    script=$3; shift 6; hermes=$d/hermes
    if [ -n "$TROUPE_TEST_REFUSE_SIGNAL" ] && [ "$1 $2" = "update signal" ]; then
      cat > /dev/null; echo '{"status": "failed", "class": "gateway_pid_changed"}'; exit 1
    fi
    exec sh -c "$script" sh "$(command -v python3)" "$hermes" "$@" ;;
esac
`

// The gateway and cron modules the guest job imports, reading their answers from files.
const stubStatus = `import json, os
d = os.environ['TROUPE_TEST_STUBS']
def get_running_pid():
    v = open(os.path.join(d, 'pid')).read().strip()
    return int(v) if v else None
def read_runtime_status():
    return json.load(open(os.path.join(d, 'runtime.json')))
`
const stubExecutions = `import json, os
d = os.environ['TROUPE_TEST_STUBS']
def list_executions(limit=50):
    return json.load(open(os.path.join(d, 'executions.json')))
def get_execution(i):
    return {'status': json.load(open(os.path.join(d, 'statuses.json'))).get(i)}
`

func newUpdateHost(t *testing.T) *updateHost {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("these tests run the guest job on the developer machine's python3")
	}
	dir := t.TempDir()
	h := &updateHost{t: t, dir: dir, repo: filepath.Join(dir, "kits"), live: filepath.Join(dir, "live", ".hermes")}
	h.base, h.kit = filepath.Join(h.repo, "base"), filepath.Join(h.repo, "ada")
	stubs := filepath.Join(dir, "stubs")
	for name, text := range map[string]string{
		"bin/sbx": fakeGuestSbx, "inventory.json": `[{"name":"ada","status":"running"}]`,
		"stubs/gateway/__init__.py": "", "stubs/gateway/status.py": stubStatus,
		"stubs/gateway/drain_control.py": "", "stubs/cron/__init__.py": "", "stubs/cron/executions.py": stubExecutions,
		"stubs/pid": "", "stubs/runtime.json": "{}", "stubs/executions.json": "[]", "stubs/statuses.json": "{}",
		"hermes/pm/__init__.py": "", "hermes/pm/environments.py": "import shutil\nfrom pathlib import Path\n" +
			"def project_python(root):\n    return Path(shutil.which('python3'))\n",
	} {
		h.writeAt(filepath.Join(dir, name), text)
	}
	os.Chmod(filepath.Join(dir, "bin/sbx"), 0o755)
	t.Setenv("TROUPE_TEST_HERMES_HOME", h.live)
	t.Setenv("TROUPE_TEST_STUBS", stubs)
	t.Setenv("PYTHONPATH", stubs)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	os.MkdirAll(h.live, 0o700)

	h.seed("base", "skills/review/SKILL.md", "---\nname: review\ndescription: Review PRs.\n---\nRead the diff.\n")
	h.seed("base", "skills/review/references/style.md", "Short sentences.\n")
	h.seed("base", "scripts/watch.py", "print('v1')\n")
	h.seed("ada", "SOUL.md", "You are Ada.\n")

	noProbe := func(context.Context) error { return nil }
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h.agent = &Agent{
		Name: "ada", Command: "update", Probe: noProbe,
		Printer: &progress.Printer{Agent: "Ada", Location: time.UTC, Out: &h.out, Err: &h.err, Now: func() time.Time { return clock }},
		Runner:  sbx.Runner{Binary: filepath.Join(dir, "bin/sbx"), Log: filepath.Join(dir, "native.log"), Probe: noProbe},
		Timing:  DefaultTiming(),
	}
	h.seeds = Seeds{Kits: []string{h.base, h.kit}}
	return h
}

func (h *updateHost) writeAt(path, text string) {
	h.t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// seed writes a kit's seed file.
func (h *updateHost) seed(kit, rel, text string) {
	h.writeAt(filepath.Join(h.repo, kit, "files/home/.hermes", rel), text)
}

func (h *updateHost) unseed(kit, rel string) {
	if err := os.RemoveAll(filepath.Join(h.repo, kit, "files/home/.hermes", rel)); err != nil {
		h.t.Fatal(err)
	}
}

// liveFile writes a file into the agent's live home, as the agent or an earlier install would.
func (h *updateHost) liveFile(rel, text string) {
	h.writeAt(filepath.Join(h.live, rel), text)
	os.Chmod(filepath.Join(h.live, rel), 0o600)
}

// reseed files a finished recreate's receipt for the seeds as they are now: their manifest
// and, as a recreate keeps them, their bytes.
func (h *updateHost) reseed(stamp string) {
	h.t.Helper()
	run := filepath.Join(os.Getenv("HOME"), ".local/state/sandbox-replacements/ada/reseed/completed", stamp)
	hashes := map[string]string{}
	files, err := h.seeds.composed()
	if err != nil {
		h.t.Fatal(err)
	}
	for rel, data := range files {
		hashes[rel] = digest(data)
		h.writeAt(filepath.Join(run, "seeds", rel), string(data))
	}
	manifest, _ := json.Marshal(map[string]any{"sha256": hashes})
	h.writeAt(filepath.Join(run, "seed-manifest.json"), string(manifest))
	h.writeAt(filepath.Join(run, "cutover.json"), `{"new_id": "fresh"}`)
}

func (h *updateHost) liveText(rel string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(h.live, rel))
	return string(data), err == nil
}

// installAll brings the live home to the seeds and files the recreate's receipt that says so,
// as a fresh agent would have them.
func (h *updateHost) installAll() {
	h.reseed("2026-01-01T00:00:00Z")
	for _, kit := range []string{h.base, h.kit} {
		root := filepath.Join(kit, "files/home/.hermes")
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				rel, _ := filepath.Rel(root, p)
				data, _ := os.ReadFile(p)
				h.liveFile(rel, string(data))
			}
			return nil
		})
	}
	for _, d := range []string{"skills", "skills/review", "skills/review/references", "scripts"} {
		os.Chmod(filepath.Join(h.live, d), 0o700)
	}
}

func (h *updateHost) plan(kind Kind) *Plan {
	h.t.Helper()
	plan, err := h.agent.PlanUpdate(context.Background(), h.seeds, kind)
	if err != nil {
		h.t.Fatalf("plan: %v", err)
	}
	return plan
}

func changes(p *Plan) string {
	var parts []string
	for _, c := range p.Changes {
		s := c.Action + " " + c.Name
		if c.Overwrites {
			s += " overwrites"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func TestNothingToUpdateWhenLiveMatches(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	for _, kind := range []Kind{Skills, Scripts, Soul} {
		if p := h.plan(kind); len(p.Changes) != 0 {
			t.Errorf("%s: %s", kind, changes(p))
		}
	}
}

func TestAddRewriteAndDeleteWithoutAgentEdits(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	h.seed("base", "scripts/notes.py", "print('notes')\n")
	p := h.plan(Scripts)
	if got := changes(p); got != "add notes.py, rewrite watch.py" {
		t.Fatalf("changes: %s", got)
	}
	if err := h.agent.applyPlan(context.Background(), p); err != nil {
		t.Fatalf("apply: %v\n%s", err, h.err.String())
	}
	// What troupe installed and the seeds no longer hold is deleted; a file troupe never
	// installed is the agent's own and is never listed.
	h.unseed("base", "scripts/notes.py")
	h.liveFile("scripts/mine.py", "print('mine')\n")
	if got := changes(h.plan(Scripts)); got != "delete notes.py" {
		t.Fatalf("changes after removal: %s", got)
	}
}

// An edit is measured against what troupe last installed, from its own receipts: an older seed
// version troupe installed is not the agent's edit, whatever happened to the seeds since.
func TestEditsAreMeasuredAgainstWhatTroupeInstalled(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	h.seed("base", "scripts/watch.py", "print('v3')\n")
	if got := changes(h.plan(Scripts)); got != "rewrite watch.py" {
		t.Fatalf("v1 as recreated: %s", got)
	}
	if err := h.agent.applyPlan(context.Background(), h.plan(Scripts)); err != nil {
		t.Fatal(err)
	}
	h.seed("base", "scripts/watch.py", "print('v4')\n")
	if got := changes(h.plan(Scripts)); got != "rewrite watch.py" {
		t.Fatalf("v3 as updated: %s", got)
	}
	// The agent's edit of an installed file, and a file at a seed version troupe never wrote.
	h.liveFile("scripts/watch.py", "print('v2')\n")
	if got := changes(h.plan(Scripts)); got != "rewrite watch.py overwrites" {
		t.Fatalf("a version troupe never installed: %s", got)
	}
}

// Without a finished recreate's receipt, troupe cannot tell the agent's edits: it refuses.
func TestUpdateWithoutARecreateReceiptIsRefused(t *testing.T) {
	h := newUpdateHost(t)
	h.liveFile("scripts/watch.py", "print('v1')\n")
	_, err := h.agent.PlanUpdate(context.Background(), h.seeds, Scripts)
	if failureClass(err) != "no_completed_reseed" {
		t.Fatalf("err %v", err)
	}
}

func TestAgentEditsAreFlaggedOverwrittenAndKept(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.liveFile("scripts/watch.py", "print('v1')\nprint('my fix')\n")
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	p := h.plan(Scripts)
	if got := changes(p); got != "rewrite watch.py overwrites" {
		t.Fatalf("changes: %s", got)
	}
	if err := h.agent.applyPlan(context.Background(), p); err != nil {
		t.Fatalf("apply: %v\n%s", err, h.err.String())
	}
	if text, _ := h.liveText("scripts/watch.py"); text != "print('v2')\n" {
		t.Fatalf("live is %q", text)
	}
	kept, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/*/overwritten/scripts/watch.py"))
	if len(kept) != 1 {
		t.Fatalf("overwritten copy: %v", kept)
	}
	if data, _ := os.ReadFile(kept[0]); string(data) != "print('v1')\nprint('my fix')\n" {
		t.Fatalf("kept copy %q", data)
	}
	// A later update prunes file contents from older plans, never the agent's copies.
	h.seed("base", "scripts/watch.py", "print('v3')\n")
	if err := h.agent.applyPlan(context.Background(), h.plan(Scripts)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept[0]); err != nil {
		t.Fatalf("the agent's copy was pruned: %v", err)
	}
}

// A file the agent added inside a seeded skill is the agent's edit: it goes, and is kept.
func TestSkillIsRewrittenAsAWholeFolder(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.liveFile("skills/review/notes.md", "my own notes\n")
	h.seed("base", "skills/review/references/style.md", "Short, plain sentences.\n")
	p := h.plan(Skills)
	if got := changes(p); got != "rewrite review overwrites" || p.Restart {
		t.Fatalf("changes %s, restart %v", got, p.Restart)
	}
	if err := h.agent.applyPlan(context.Background(), p); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, present := h.liveText("skills/review/notes.md"); present {
		t.Fatal("the agent's extra file is still in the seeded skill")
	}
	if text, _ := h.liveText("skills/review/references/style.md"); text != "Short, plain sentences.\n" {
		t.Fatalf("live %q", text)
	}
}

func TestAgentMadeSkillsAreNeverTouched(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.liveFile("skills/my-own/SKILL.md", "---\nname: my-own\n---\n")
	h.seed("base", "skills/review/SKILL.md", "---\nname: review\ndescription: Review PRs.\n---\nRead the whole diff.\n")
	if got := changes(h.plan(Skills)); got != "rewrite review" {
		t.Fatalf("changes: %s", got)
	}
}

func TestRestartOnlyWhenTheSkillIndexChanges(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "skills/review/SKILL.md", "---\nname: review\ndescription: Review pull requests.\n---\nRead the diff.\n")
	h.seed("base", "skills/triage/SKILL.md", "---\nname: triage\ndescription: Triage.\n---\n")
	p := h.plan(Skills)
	if !p.Restart || strings.Join(p.AddedSkills, ",") != "triage" {
		t.Fatalf("restart %v added %v", p.Restart, p.AddedSkills)
	}
}

func TestLiveChangedAfterTheListIsRefused(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	p := h.plan(Scripts)
	h.liveFile("scripts/watch.py", "print('edited meanwhile')\n")
	err := h.agent.applyPlan(context.Background(), p)
	if !strings.Contains(failureClass(err), "live_file_changed") {
		t.Fatalf("err %v", err)
	}
	if text, _ := h.liveText("scripts/watch.py"); text != "print('edited meanwhile')\n" {
		t.Fatal("the meanwhile edit was overwritten")
	}
}

// Update ties the confirmation and --yes to exactly what was listed.
func TestConfirmationDryRunAndYes(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	os.MkdirAll(h.agent.Paths.Pairs, 0o700)
	h.agent.Paths.Pairs = filepath.Join(h.dir, "pairs")
	fakeSystemctlActive(t, h.dir)
	ctx := context.Background()

	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{}); !strings.HasPrefix(failureClass(err), "no_terminal_to_confirm") {
		t.Fatalf("without a terminal: %v", err)
	}
	no := func(string) bool { return false }
	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{Confirm: no}); err != nil {
		t.Fatal(err)
	}
	if text, _ := h.liveText("scripts/watch.py"); text != "print('v1')\n" {
		t.Fatal("changed after the operator said no")
	}
	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{Yes: true}); !strings.HasPrefix(failureClass(err), "no_dry_run") {
		t.Fatalf("--yes without a dry run: %v", err)
	}
	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	h.seed("base", "scripts/watch.py", "print('v3')\n")
	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{Yes: true}); !strings.HasPrefix(failureClass(err), "changed_since_dry_run") {
		t.Fatalf("--yes after a change: %v", err)
	}
	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.agent.Update(ctx, h.seeds, Scripts, UpdateOptions{Yes: true}); err != nil {
		t.Fatalf("--yes after a matching dry run: %v\n%s", err, h.err.String())
	}
	if text, _ := h.liveText("scripts/watch.py"); text != "print('v3')\n" {
		t.Fatalf("live %q", text)
	}
}

// fakeSystemctlActive puts a systemctl on PATH that reports every unit active.
func fakeSystemctlActive(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "sysbin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n[ \"$1\" = is-active ] && echo active\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// Paths in the guest's answer come from the agent's VM and are never trusted.
func TestUntrustedGuestPathsAreRefused(t *testing.T) {
	items := []item{{root: "skills/review"}, {root: "scripts/watch.py"}}
	for rel, want := range map[string]bool{
		"skills/review/SKILL.md": true, "scripts/watch.py": true,
		"skills/review/../../../.ssh/authorized_keys": false, "../escape": false, "/etc/passwd": false,
		"skills/reviewer/SKILL.md": false, "skills/review/./x": false, "skills/other/SKILL.md": false, "": false,
		"skills/review/\x1b]0;owned\x07": false, "skills/review/\u009b2J": false, "skills/review/\xff": false,
		"skills/review/notes 2.md": true,
	} {
		if got := insideItems(rel, items); got != want {
			t.Errorf("insideItems(%q) = %v, want %v", rel, got, want)
		}
	}
	// And the host never writes a kept copy outside the receipt.
	h := newUpdateHost(t)
	h.installAll()
	escape := filepath.Join(h.dir, "escaped")
	plan := &Plan{Agent: "ada", Files: map[string]*fileChange{}, overwritten: map[string][]byte{"../../../../escaped": []byte("x")}}
	if err := h.agent.applyPlan(context.Background(), plan); !strings.HasPrefix(failureClass(err), "live_path_not_allowed") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(escape); err == nil {
		t.Fatal("a kept copy was written outside the receipt")
	}
}

// An update installs exactly what a recreate would: the composed seed folders, bytecode
// skipped, and a path two kits provide refused.
func TestUpdateInstallsWhatARecreateComposes(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "skills/review/__pycache__/x.cpython-313.pyc", "bytecode")
	if got := changes(h.plan(Skills)); got != "" {
		t.Fatalf("bytecode was planned: %s", got)
	}
	h.seed("base", "skills/review/references/extra.md", "More.\n")
	if got := changes(h.plan(Skills)); got != "rewrite review" {
		t.Fatalf("a new seed file: %s", got)
	}
	h.seed("ada", "scripts/watch.py", "print('ada')\n")
	if _, err := h.agent.PlanUpdate(context.Background(), h.seeds, Scripts); err == nil || !strings.Contains(err.Error(), "two kits") {
		t.Fatalf("a path from two kits: %v", err)
	}
}

// troupe update reads the kits and its own receipts: git is not needed on the host.
func TestUpdateRunsWithoutGit(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	bin := filepath.Join(h.dir, "nogit")
	os.MkdirAll(bin, 0o755)
	for _, tool := range []string{"sh", "dirname", "cat", "mktemp", "rm", "tar", "python3"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		os.Symlink(path, filepath.Join(bin, tool))
	}
	t.Setenv("PATH", bin)
	if _, err := exec.LookPath("git"); err == nil {
		t.Fatal("git is still on PATH")
	}
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	if err := h.agent.applyPlan(context.Background(), h.plan(Scripts)); err != nil {
		t.Fatalf("apply: %v\n%s", err, h.err.String())
	}
	if text, _ := h.liveText("scripts/watch.py"); text != "print('v2')\n" {
		t.Fatalf("live %q", text)
	}
}

// With a restart needed, the restart is sent right after the install: a failing file check
// cannot leave the new skill list unloaded.
func TestRestartIsSentEvenWhenTheFileCheckFails(t *testing.T) {
	h := newRestartHost(t)
	h.installAll()
	h.seed("base", "skills/triage/SKILL.md", "---\nname: triage\ndescription: Triage.\n---\n")
	os.Chmod(filepath.Join(h.live, "skills"), 0o755) // the parent mode check will fail
	h.session("- review: Review PRs.\n- triage: Triage.")
	h.gateway(true)
	err := h.agent.applyPlan(context.Background(), h.plan(Skills))
	if !strings.HasPrefix(failureClass(err), "permissions") && err != nil {
		t.Logf("err %v", err)
	}
	if found, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/*/restart.env")); len(found) != 1 {
		t.Fatalf("the restart was not sent: %v (err %v)", found, err)
	}
}

// A word from inside the agent's VM reaches the terminal only as a plain error name.
func TestGuestWordsCannotCarryControlCodes(t *testing.T) {
	for in, want := range map[any]string{
		"live_file_changed": "live_file_changed", "\x1b[2J\x1b]0;owned\x07": "unknown",
		"Install": "unknown", 42: "unknown", nil: "unknown", "a b": "unknown",
	} {
		if got := Plain(in); got != want {
			t.Errorf("Plain(%q) = %q, want %q", in, got, want)
		}
	}
	// End to end: a guest answer with an escape sequence in its class.
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	h.writeAt(filepath.Join(h.dir, "bin/sbx"), strings.Replace(fakeGuestSbx, "    exec sh -c \"$script\" sh \"$(command -v python3)\" \"$hermes\" \"$@\" ;;",
		"    if [ \"$1 $2\" = \"update install\" ]; then cat >/dev/null; printf '{\"status\": \"failed\", \"class\": \"\\u001b[2Jowned\", \"restored\": true}\\n'; exit 1; fi\n    exec sh -c \"$script\" sh \"$(command -v python3)\" \"$hermes\" \"$@\" ;;", 1))
	err := h.agent.applyPlan(context.Background(), h.plan(Scripts))
	h.agent.Report(err)
	if strings.Contains(h.out.String()+h.err.String(), "\x1b") || !strings.Contains(failureClass(err), "install_failed unknown") {
		t.Fatalf("err %v output %q", err, h.out.String()+h.err.String())
	}
}

// update read's answer is decoded as a counted stream: a listing past the limits is refused
// before it can inflate in host memory, and a normal one decodes as before.
func TestReadAnswerIsCountedWhileDecoded(t *testing.T) {
	var files strings.Builder
	files.WriteString(`{"status":"passed","files":{`)
	for i := 0; i <= maxLiveFiles; i++ {
		if i > 0 {
			files.WriteString(",")
		}
		fmt.Fprintf(&files, `"skills/x/%d":{}`, i)
	}
	files.WriteString(`}}`)
	var answer readAnswer
	if err := json.Unmarshal([]byte(files.String()), &answer); err == nil {
		t.Fatal("a listing past maxLiveFiles decoded")
	}
	running := `{"running":[` + strings.Repeat(`{"id":"x"},`, maxRunning) + `{"id":"y"}]}`
	if err := json.Unmarshal([]byte(running), &answer); err == nil {
		t.Fatal("a running list past maxRunning decoded")
	}
	for _, bad := range []string{`{"running":[{"id":{"a":1}}]}`, `{"running":[{"id":"` + strings.Repeat("x", 200) + `"}]}`} {
		if err := json.Unmarshal([]byte(bad), &answer); err == nil {
			t.Fatalf("accepted %.40s", bad)
		}
	}
	ok := `{"status":"passed","files":{"skills/x/SKILL.md":{"b64":"aGk="},"skills/x/l":{"error":"symlink"}},"running":[{"id":7}]}`
	answer = readAnswer{}
	if err := json.Unmarshal([]byte(ok), &answer); err != nil {
		t.Fatal(err)
	}
	if *answer.Files["skills/x/SKILL.md"].B64 != "aGk=" || *answer.Files["skills/x/l"].Error != "symlink" ||
		len(answer.Running) != 1 || answer.Running[0] != "7" {
		t.Fatalf("decoded %+v", answer)
	}
}

// An upgrade's seed step makes the seeded skills and scripts match the new kit version: the
// seeds win (the agent's lessons are harvested into the kit first), every file of the agent's
// own it replaces or removes is kept in the receipt, and SOUL.md (the agent's own folder's) is left.
func TestApplySeedsMakesTheSeedsMatchTheNewKit(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.agent.Paths.Pairs = filepath.Join(h.dir, "pairs")
	os.MkdirAll(h.agent.Paths.Pairs, 0o700)
	fakeSystemctlActive(t, h.dir)
	h.liveFile("skills/review/references/style.md", "Short sentences. Agent's own note.\n")
	h.seed("base", "skills/review/references/style.md", "Short, plain sentences.\n")
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	h.seed("ada", "SOUL.md", "You are Ada, renamed.\n")
	if err := h.agent.ApplySeeds(context.Background(), h.seeds); err != nil {
		t.Fatalf("%v\n%s%s", err, h.out.String(), h.err.String())
	}
	for rel, want := range map[string]string{
		"scripts/watch.py":                  "print('v2')\n",
		"skills/review/references/style.md": "Short, plain sentences.\n",
		"SOUL.md":                           "You are Ada.\n",
	} {
		if text, _ := h.liveText(rel); text != want {
			t.Errorf("%s: %q", rel, text)
		}
	}
	kept, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/*/overwritten/skills/review/references/style.md"))
	if len(kept) != 1 {
		t.Fatalf("the agent's replaced file was not kept: %v", kept)
	}
	// Skills and scripts each got their own receipt, even within one second, so troupe knows
	// what it installed: the next run finds nothing to change.
	plans, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".local/state/live-apply/ada/*/plan.json"))
	if len(plans) != 2 {
		t.Fatalf("receipts %v", plans)
	}
	h.out.Reset()
	if err := h.agent.ApplySeeds(context.Background(), h.seeds); err != nil || !strings.Contains(h.out.String(), "already match the new kit") {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
}
