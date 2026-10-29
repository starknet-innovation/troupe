//go:build dev

package harvest

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// harvestSbx answers the inventory from inventory.json and runs the real guest job on this
// host's python3, against the live home TROUPE_TEST_HERMES_HOME names, with a stand-in Hermes
// folder whose package manager names that python3. It keeps the last job archive (with its
// request.json) and, when answer exists, prints it instead of running the job.
const harvestSbx = `#!/bin/sh
d=$(dirname "$0")
printf 'sbx %s\n' "$1" >> "$d/calls"
case "$1" in
  ls) cat "$d/inventory.json" ;;
  exec)
    shift; [ "$1" = -i ] && shift; [ "$1" = --workdir ] && shift 2; shift
    script=$3; shift 6
    cat > "$d/request.tar"
    if [ -f "$d/answer" ]; then cat "$d/answer"; exit 0; fi
    exec sh -c "$script" sh "$(command -v python3)" "$d/hermes" "$@" < "$d/request.tar" ;;
esac
`

const (
	skill  = "skills/coding-workflow/references/project-execution.md"
	notes  = "skills/coding-workflow/references/native-notes.md"
	base   = "# Execution\n\nRun checks.\n"
	lesson = "# Execution\n\nRun checks.\n\nLesson from a run.\n"
	newer  = base + "Newer seed rule.\n"
)

// harvestHost is a kits root (no version control) with ada on the base kit, ada's last
// recreate's receipts, a live home for the guest job and a fake sbx.
type harvestHost struct {
	t                             *testing.T
	dir, root, seeds, live, tools string
	e                             env
	out, err                      *bytes.Buffer
	expected                      []string
}

type harvestCase struct {
	// live is the agent's skill files (nil: the seeded ones, the skill with a lesson).
	live map[string]string
	// agentShas are the ledger's agent edits by file (nil: the lesson); created the files he made.
	agentShas map[string][]string
	created   map[string]string
	vmID      string
}

func newHarvestHost(t *testing.T, c harvestCase) *harvestHost {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("these tests run the guest job on the developer machine's python3")
	}
	dir := t.TempDir()
	// Never the operator's own troupe data or settings: harvest reads every agent troupe knows.
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg-data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	h := &harvestHost{t: t, dir: dir, root: filepath.Join(dir, "kits"), live: filepath.Join(dir, "live/.hermes"),
		tools: filepath.Join(dir, "tools"), out: &bytes.Buffer{}, err: &bytes.Buffer{}, expected: []string{notes, skill}}
	h.seeds = filepath.Join(h.root, "kits/coder/files/home/.hermes")
	write(t, filepath.Join(h.root, "kits/coder/coder.yaml"), "# syntax=docker/sandbox-kit:3\nkind: workload\n", 0o644)
	write(t, filepath.Join(h.root, "agents/ada/sbxenv.yaml"), "name: ada\nkits:\n  - \"../../kits/coder\"\n  - \".\"\n", 0o644)
	write(t, filepath.Join(h.seeds, skill), base, 0o644)
	write(t, filepath.Join(h.seeds, notes), "notes\n", 0o644)
	reseed := filepath.Join(dir, "state/ada/reseed/completed/2026-09-23T17:24:06Z")
	// The recreate kept the seeds' bytes beside their manifest.
	write(t, filepath.Join(reseed, "seeds", skill), base, 0o600)
	write(t, filepath.Join(reseed, "seeds", notes), "notes\n", 0o600)
	manifest, _ := json.Marshal(map[string]any{"sha256": map[string]string{skill: digest([]byte(base)),
		notes: digest([]byte("notes\n")), "config.yaml": "x"}})
	write(t, filepath.Join(reseed, "seed-manifest.json"), string(manifest), 0o644)
	write(t, filepath.Join(reseed, "cutover.json"), `{"new_id": "fresh"}`, 0o644)

	live := c.live
	if live == nil {
		live = map[string]string{skill: lesson, notes: "notes\n"}
	}
	for rel, text := range live {
		write(t, filepath.Join(h.live, rel), text, 0o600)
	}
	agentShas := c.agentShas
	if agentShas == nil {
		agentShas = map[string][]string{skill: {digest([]byte(lesson))}}
	}
	var ledger []string
	entry := func(action, rel string, shas []string) {
		var after []map[string]string
		for _, sha := range shas {
			after = append(after, map[string]string{"path": filepath.Join(h.live, rel), "sha256": sha})
		}
		line, _ := json.Marshal(map[string]any{"actor": "agent", "ts": "2026-09-23T18:03:06Z", "action": action,
			"skill": "coding-workflow", "after": after})
		ledger = append(ledger, string(line))
	}
	for rel, shas := range agentShas {
		entry("patch", rel, shas)
	}
	for rel, text := range c.created {
		write(t, filepath.Join(h.live, rel), text, 0o600)
		entry("create", rel, []string{digest([]byte(text))})
	}
	write(t, filepath.Join(h.live, "skills/.curator_ledger.jsonl"), strings.Join(ledger, "\n"), 0o600)

	vmID := c.vmID
	if vmID == "" {
		vmID = "fresh"
	}
	write(t, filepath.Join(h.tools, "sbx"), harvestSbx, 0o755)
	write(t, filepath.Join(h.tools, "inventory.json"), `[{"name":"ada","id":"`+vmID+`","status":"running"}]`, 0o644)
	write(t, filepath.Join(h.tools, "hermes/pm/__init__.py"), "", 0o644)
	write(t, filepath.Join(h.tools, "hermes/pm/environments.py"), "import shutil\nfrom pathlib import Path\n"+
		"def project_python(root):\n    return Path(shutil.which('python3'))\n", 0o644)
	t.Setenv("TROUPE_TEST_HERMES_HOME", h.live)
	h.e = env{ctx: context.Background(), out: h.out, err: h.err, kitsFlag: h.root, tools: tools{
		runner: sbx.Runner{Binary: filepath.Join(h.tools, "sbx"), Log: filepath.Join(dir, "sbx.log"),
			Paths: guard.Paths{Pairs: filepath.Join(dir, "pairs")}, Probe: func(context.Context) error { return nil }},
		scratch: filepath.Join(dir, "scratch"),
		state:   filepath.Join(dir, "state"),
		updates: filepath.Join(dir, "live-apply"),
		now:     func() time.Time { return time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC) },
	}}
	return h
}

// changeSeed changes a seed in the kits.
func (h *harvestHost) changeSeed(rel, text string) {
	write(h.t, filepath.Join(h.seeds, rel), text, 0o644)
}

// migrate files troupe migrate's finished receipt: a new VM that kept the agent's state.
func (h *harvestHost) migrate(stamp, id string) {
	write(h.t, filepath.Join(h.dir, "state/ada/completed", stamp, "cutover.json"), `{"new_id": "`+id+`"}`, 0o644)
}

// update files troupe reseed's receipt: the plan's after hashes and the bytes it wrote, applied
// when the install passed, and the agent's edited copies it replaced. A nil status is an install that never
// answered; a nil file is a deletion.
func (h *harvestHost) update(stamp string, files map[string]*string, status any, overwritten map[string]string) {
	run := filepath.Join(h.dir, "live-apply/ada", stamp)
	plan := map[string]any{}
	for rel, text := range files {
		entry := map[string]any{"before": nil, "after": nil}
		if text != nil {
			entry["after"], entry["b64"] = digest([]byte(*text)), base64.StdEncoding.EncodeToString([]byte(*text))
		}
		plan[rel] = entry
	}
	data, _ := json.Marshal(map[string]any{"kind": "skills", "files": plan})
	write(h.t, filepath.Join(run, "plan.json"), string(data), 0o644)
	install := "null"
	if status != nil {
		data, _ = json.Marshal(map[string]any{"status": status})
		install = string(data)
	}
	write(h.t, filepath.Join(run, "install.json"), install, 0o644)
	for rel, text := range overwritten {
		write(h.t, filepath.Join(run, "overwritten", rel), text, 0o600)
	}
}

// answer makes the guest print payload instead of reading the live home.
func (h *harvestHost) answer(payload any) {
	data, _ := json.Marshal(payload)
	write(h.t, filepath.Join(h.tools, "answer"), string(data), 0o644)
}

func entryOf(text string) map[string]string {
	return map[string]string{"sha256": digest([]byte(text)), "b64": base64.StdEncoding.EncodeToString([]byte(text))}
}

// collect runs troupe harvest ada: the status lines, or the blocked class.
func (h *harvestHost) collect() ([]string, string) {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	code := harvestCommand(h.e, []string{"ada"})
	if code != 0 {
		blocked, ok := strings.CutPrefix(strings.TrimSpace(h.err.String()), "blocked ")
		if code != 1 || !ok || h.out.Len() > 0 {
			h.t.Fatalf("exit %d, stdout %q, stderr %q", code, h.out.String(), h.err.String())
		}
		return nil, blocked
	}
	if h.err.Len() > 0 {
		h.t.Fatalf("stderr %q", h.err.String())
	}
	if paths := h.request(); !reflect.DeepEqual(paths, h.expected) {
		h.t.Fatalf("request paths %v, want %v", paths, h.expected)
	}
	return strings.Split(strings.TrimSuffix(h.out.String(), "\n"), "\n"), ""
}

// request is the paths the last guest call asked for.
func (h *harvestHost) request() []string {
	h.t.Helper()
	file, err := os.Open(filepath.Join(h.tools, "request.tar"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			h.t.Fatalf("no request.json: %v", err)
		}
		if header.Name == "request.json" {
			var request struct{ Paths []string }
			data, _ := io.ReadAll(reader)
			if err := json.Unmarshal(data, &request); err != nil {
				h.t.Fatal(err)
			}
			return request.Paths
		}
	}
}

func (h *harvestHost) blocked(want string) {
	h.t.Helper()
	if lines, class := h.collect(); class != want {
		h.t.Fatalf("lines %q, blocked %q, want blocked %s", lines, class, want)
	}
}

func (h *harvestHost) collection() string {
	return filepath.Join(h.dir, "scratch/ada/20260923T200000Z")
}

func (h *harvestHost) read(rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.collection(), rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func (h *harvestHost) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(h.collection(), rel))
	return err == nil
}

func (h *harvestHost) sbxCalls() string {
	data, _ := os.ReadFile(filepath.Join(h.tools, "calls"))
	return string(data)
}

func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func count(lines []string, part string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, part) {
			n++
		}
	}
	return n
}

func TestHarvestRefusesMalformedGuestAnswers(t *testing.T) {
	for _, bad := range []any{
		map[string]any{"files": []any{}, "ledger": []any{}, "created": map[string]any{}},
		map[string]any{"files": map[string]any{}, "ledger": []any{map[string]any{"ts": 1, "paths": []any{}, "after": []any{}}}, "created": map[string]any{}},
		map[string]any{"files": map[string]any{}, "ledger": []any{"x"}, "created": map[string]any{}},
		[]any{"not", "a", "dict"},
		// Seeded files that do not decode.
		map[string]any{"files": map[string]any{skill: map[string]string{"sha256": "x", "b64": "%%% not base64 %%%"}},
			"ledger": []any{}, "created": map[string]any{}},
	} {
		h := newHarvestHost(t, harvestCase{})
		h.answer(bad)
		h.blocked("guest_read_invalid")
		if _, err := os.Stat(filepath.Join(h.dir, "scratch")); err == nil {
			t.Fatal("a refused answer left a collection")
		}
	}
}

func TestHarvestStopsOnUnreadableOrOversizedGuestAnswers(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	write(t, filepath.Join(h.tools, "answer"), "Traceback: not json\n", 0o644)
	h.blocked("guest_read_failed")
	// The VM decides the size: past the cap the read is stopped, not buffered.
	write(t, filepath.Join(h.tools, "answer"), strings.Repeat("x", 16<<20+1), 0o644)
	h.blocked("guest_read_failed")
}

func TestHarvestSkipsCollidingCreatedNames(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	h.answer(map[string]any{"files": map[string]any{skill: entryOf(lesson), notes: entryOf("notes\n")}, "ledger": []any{},
		"created": map[string]any{"skills/a": entryOf("one"), "skills/a/live": entryOf("two")}})
	lines, _ := h.collect()
	if count(lines, "could not be written") != 1 || !contains(lines, "new_file skills/a") {
		t.Fatalf("lines %q", lines)
	}
}

func TestHarvestKeepsPlantedNamesInsideScratchAndOffTheTerminal(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	victim := filepath.Join(h.dir, "victim")
	h.answer(map[string]any{"files": map[string]any{skill: entryOf(lesson), notes: entryOf("notes\n")}, "ledger": []any{},
		"created": map[string]any{"../../../../victim/.bashrc.d": entryOf("echo pwned"), filepath.Join(victim, "abs"): entryOf("echo pwned"),
			"skills/\x1b]0;HIJACKED\x07x": entryOf("x"), "skills/ok/notes.md": entryOf("fine")}})
	lines, _ := h.collect()
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("wrote outside scratch")
	}
	if strings.ContainsAny(strings.Join(lines, "\n"), "\x1b\x07") {
		t.Fatalf("control characters reached the terminal: %q", lines)
	}
	if !contains(lines, "new_file skills/ok/notes.md") || count(lines, "unsafe name") != 3 {
		t.Fatalf("lines %q", lines)
	}
}

func TestHarvestCollectsAnAgentChangeWithItsExactBasePrivately(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	lines, _ := h.collect()
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "changed_by_agent "+skill+" base=") ||
		strings.Contains(lines[0], "seed_changed") || strings.Contains(lines[0], "base=unknown") ||
		!strings.HasSuffix(lines[0], " agent_edits=1") {
		t.Fatalf("lines %q", lines) // The unchanged notes file is not reported.
	}
	if lines[1] != "scratch "+h.collection()+" files=1" {
		t.Fatalf("scratch line %q", lines[1])
	}
	// Blending is the user's; troupe names the command that then replaces the agent's skills.
	if !strings.HasPrefix(lines[2], "next: blend the lessons") || !strings.HasSuffix(lines[2], "then run troupe reseed skills ada") {
		t.Fatalf("next step %q", lines[2])
	}
	folder := filepath.Join("files", skill)
	if h.read(folder+"/base") != base || h.read(folder+"/live") != lesson || h.read(folder+"/seed") != base {
		t.Fatal("copies differ")
	}
	if diff := h.read(folder + "/agent.diff"); !strings.Contains(diff, "+Lesson from a run.\n") ||
		!strings.HasPrefix(diff, "--- base\n+++ live\n@@ -1,3 +1,5 @@\n") {
		t.Fatalf("agent.diff %q", diff)
	}
	if h.read(folder+"/seed.diff") != "" {
		t.Fatal("seed.diff of an unchanged seed is not empty")
	}
	for _, path := range []string{filepath.Join(h.dir, "scratch"), h.collection(), filepath.Join(h.collection(), folder),
		filepath.Join(h.collection(), folder, "live"), filepath.Join(h.collection(), "summary.json")} {
		info, err := os.Stat(path)
		want := os.FileMode(0o600)
		if err == nil && info.IsDir() {
			want = 0o700
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", path, info, err)
		}
	}
	if strings.Contains(strings.Join(lines, "\n"), "Lesson from a run") {
		t.Fatal("file contents reached the terminal")
	}
	var summary map[string]any
	json.Unmarshal([]byte(h.read("summary.json")), &summary)
	item := summary["files"].(map[string]any)[skill].(map[string]any)
	if summary["agent"] != "ada" || summary["reseed"] != "2026-09-23T17:24:06Z" || summary["migrate"] != nil ||
		item["status"] != "changed_by_agent" || item["seed_changed_since_base"] != false ||
		!reflect.DeepEqual(item["agent_edits"], []any{"2026-09-23T18:03:06Z"}) ||
		!reflect.DeepEqual(summary["updates"], []any{}) || !reflect.DeepEqual(summary["overwritten"], []any{}) {
		t.Fatalf("summary %v", summary)
	}
}

// An agent troupe created from a published kit is harvested the same way: its seeds are the
// pinned kit's files in troupe's data folder, and no kits root is needed.
func TestHarvestAPublishedAgent(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	t.Setenv("XDG_DATA_HOME", h.dir) // troupe's data folder is then h.dir/troupe
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h.dir, "config"))
	data := filepath.Join(h.dir, "troupe")
	kit := filepath.Join(data, "kits/coder-1.2.0")
	write(t, filepath.Join(kit, "kit.json"), `{"reference":"ghcr.io/example/coder:1.2.0","type":"coder","version":"1.2.0","hermes":"0.22.2"}`, 0o644)
	write(t, filepath.Join(kit, "files/home/.hermes", skill), base, 0o644)
	write(t, filepath.Join(kit, "files/home/.hermes", notes), "notes\n", 0o644)
	write(t, filepath.Join(data, "agents/ada/sbxenv.yaml"), "name: ada\nkits:\n  - \"ghcr.io/example/coder:1.2.0\"\n  - \".\"\n", 0o600)
	os.RemoveAll(h.root) // no kits root at all
	h.e.kitsFlag = ""
	lines, _ := h.collect()
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "changed_by_agent "+skill+" base=") || strings.Contains(lines[0], "seed_changed") {
		t.Fatalf("lines %q\n%s", lines, h.err.String())
	}
	if !strings.HasSuffix(lines[2], "publish a new version of the kit, then run troupe upgrade ada") {
		t.Fatalf("next step %q", lines[2])
	}
	if h.read(filepath.Join("files", skill, "seed")) != base {
		t.Fatal("the seed is not the pinned kit's")
	}
}

// The base comes from troupe's own receipts: git is not needed on the host.
func TestHarvestFindsTheBaseWithoutGit(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
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
	lines, _ := h.collect()
	if lines[0] != "changed_by_agent "+skill+" base=2026-09-23T17:24:06Z agent_edits=1" ||
		h.read(filepath.Join("files", skill, "base")) != base {
		t.Fatalf("lines %q", lines)
	}
}

// A recreate receipt from before troupe kept the seeds' bytes names no base.
func TestHarvestReportsAnUnknownBaseForAnOlderReceipt(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	os.RemoveAll(filepath.Join(h.dir, "state/ada/reseed/completed/2026-09-23T17:24:06Z/seeds"))
	lines, _ := h.collect()
	if !strings.HasPrefix(lines[0], "changed_by_agent "+skill+" base=unknown") || h.exists(filepath.Join("files", skill, "base")) {
		t.Fatalf("lines %q", lines)
	}
}

func TestHarvestFlagsUnattributedChangesNewFilesAndSeedDrift(t *testing.T) {
	h := newHarvestHost(t, harvestCase{agentShas: map[string][]string{}, created: map[string]string{"skills/new-skill/SKILL.md": "new skill\n"}})
	write(t, filepath.Join(h.seeds, skill), newer, 0o644)
	lines, _ := h.collect()
	if !strings.HasPrefix(lines[0], "changed_without_agent_record "+skill) || !strings.Contains(lines[0], " seed_changed") ||
		!contains(lines, "new_file skills/new-skill/SKILL.md") {
		t.Fatalf("lines %q", lines)
	}
	if h.read("files/skills/new-skill/SKILL.md/live") != "new skill\n" {
		t.Fatal("created copy differs")
	}
}

func TestHarvestReportsAnUnknownBaseAndALessonAlreadyInTheSeed(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	manifest, _ := json.Marshal(map[string]any{"sha256": map[string]string{skill: digest([]byte("uncommitted seed")),
		notes: digest([]byte("notes\n"))}})
	write(t, filepath.Join(h.dir, "state/ada/reseed/completed/2026-09-23T17:24:06Z/seed-manifest.json"), string(manifest), 0o644)
	write(t, filepath.Join(h.seeds, skill), lesson, 0o644)
	lines, _ := h.collect()
	if !strings.Contains(lines[0], "base=unknown") || !strings.Contains(lines[0], " already_in_seed") {
		t.Fatalf("lines %q", lines)
	}
}

func TestHarvestReportsADeletedLiveFile(t *testing.T) {
	h := newHarvestHost(t, harvestCase{live: map[string]string{skill: base}, agentShas: map[string][]string{}})
	lines, _ := h.collect()
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "deleted_live "+notes+" base=") {
		t.Fatalf("lines %q", lines)
	}
}

func TestHarvestPreconditionsStopBeforeAnyGuestRead(t *testing.T) {
	cases := map[string]func(h *harvestHost){
		"unregistered_or_unresolvable_agent": func(h *harvestHost) { os.Remove(filepath.Join(h.root, "agents/ada/sbxenv.yaml")) },
		"sandbox_preflight_failed": func(h *harvestHost) {
			h.e.tools.runner.Probe = func(context.Context) error { return context.DeadlineExceeded }
		},
		"running_vm_is_not_the_last_reseed_or_migrate": func(h *harvestHost) {
			write(t, filepath.Join(h.tools, "inventory.json"), `[{"name":"ada","id":"replaced"}]`, 0o644)
		},
		"no_completed_reseed": func(h *harvestHost) { os.RemoveAll(filepath.Join(h.dir, "state")) },
	}
	for class, change := range cases {
		h := newHarvestHost(t, harvestCase{})
		change(h)
		h.blocked(class)
		if strings.Contains(h.sbxCalls(), "sbx exec") {
			t.Fatalf("%s: guest read after a refusal: %s", class, h.sbxCalls())
		}
		if _, err := os.Stat(filepath.Join(h.dir, "scratch")); err == nil {
			t.Fatalf("%s: a refusal left a collection", class)
		}
	}
	// A suspended agent (a disposable copy exists) is refused before any sbx call.
	h := newHarvestHost(t, harvestCase{})
	write(t, filepath.Join(h.dir, "pairs/ada.json"), `{"phase": "copying"}`, 0o644)
	h.blocked("sandbox_preflight_failed")
	if h.sbxCalls() != "" {
		t.Fatalf("sbx calls for a suspended agent: %s", h.sbxCalls())
	}
	h.e.tools.runner.Binary = "/nonexistent"
	h.out.Reset()
	h.err.Reset()
	if code := harvestCommand(h.e, []string{"Bad Name"}); code != 1 || h.err.String() != "blocked malformed_agent_name\n" {
		t.Fatalf("exit %d, stderr %q", code, h.err.String())
	}
	if code := harvestCommand(h.e, nil); code != 2 {
		t.Fatalf("exit %d without an agent", code)
	}
}

func TestHarvestReadsTheVMOfTheLastMigrateAfterTheReseed(t *testing.T) {
	// The migrate kept the agent's skills: its VM is the one to read, against the reseed's base.
	h := newHarvestHost(t, harvestCase{vmID: "migrated"})
	h.migrate("2026-09-23T18:00:00Z", "migrated")
	if lines, class := h.collect(); class != "" || !strings.HasPrefix(lines[0], "changed_by_agent "+skill) {
		t.Fatalf("lines %q, blocked %q", lines, class)
	}
	var summary map[string]any
	json.Unmarshal([]byte(h.read("summary.json")), &summary)
	if summary["migrate"] != "2026-09-23T18:00:00Z" {
		t.Fatalf("summary %v", summary)
	}
	// The reseed's VM is gone once a later migrate replaced it.
	h = newHarvestHost(t, harvestCase{})
	h.migrate("2026-09-23T18:00:00Z", "migrated")
	h.blocked("running_vm_is_not_the_last_reseed_or_migrate")
	// A migrate before the reseed does not count, nor does a folder name that is no time.
	h = newHarvestHost(t, harvestCase{})
	h.migrate("2026-09-22T18:00:00Z", "older")
	h.migrate("latest", "planted")
	if lines, class := h.collect(); class != "" || !strings.HasPrefix(lines[0], "changed_by_agent "+skill) {
		t.Fatalf("lines %q, blocked %q", lines, class)
	}
}

func text(s string) *string { return &s }

func TestHarvestAppliedUpdatesMoveTheBase(t *testing.T) {
	// The update wrote the newer seed: a live copy equal to it is no agent edit.
	h := newHarvestHost(t, harvestCase{agentShas: map[string][]string{}, live: map[string]string{skill: newer, notes: "notes\n"}})
	h.changeSeed(skill, newer)
	h.update("20260923T190000Z", map[string]*string{skill: text(newer)}, "passed", nil)
	if lines, _ := h.collect(); !reflect.DeepEqual(lines, []string{"no agent skill edits to review"}) {
		t.Fatalf("lines %q", lines)
	}
	var summary map[string]any
	json.Unmarshal([]byte(h.read("summary.json")), &summary)
	if !reflect.DeepEqual(summary["updates"], []any{"20260923T190000Z"}) {
		t.Fatalf("summary %v", summary)
	}
	// A lesson after the update is diffed against the updated version.
	withLesson := newer + "Lesson from a run.\n"
	h = newHarvestHost(t, harvestCase{agentShas: map[string][]string{skill: {digest([]byte(withLesson))}},
		live: map[string]string{skill: withLesson, notes: "notes\n"}})
	h.changeSeed(skill, newer)
	h.update("20260923T190000Z", map[string]*string{skill: text(newer)}, "passed", nil)
	lines, _ := h.collect()
	if !strings.HasPrefix(lines[0], "changed_by_agent "+skill) || strings.Contains(lines[0], "seed_changed") ||
		h.read(filepath.Join("files", skill, "base")) != newer {
		t.Fatalf("lines %q", lines)
	}
}

func TestHarvestFailedOlderAndUnansweredUpdatesDoNotMoveTheBase(t *testing.T) {
	for _, c := range []struct {
		stamp  string
		status any
	}{{"20260923T190000Z", "failed"}, {"20260922T190000Z", "passed"}, {"not-a-time", "passed"}, {"20260923T190000Z", nil}} {
		h := newHarvestHost(t, harvestCase{agentShas: map[string][]string{}, live: map[string]string{skill: newer, notes: "notes\n"}})
		h.changeSeed(skill, newer)
		h.update(c.stamp, map[string]*string{skill: text(newer)}, c.status, nil)
		if lines, class := h.collect(); class != "" || !strings.HasPrefix(lines[0], "changed_without_agent_record "+skill) {
			t.Fatalf("%v: lines %q, blocked %q", c, lines, class)
		}
	}
}

func TestHarvestRefusesAnUnreadableAppliedUpdate(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	h.update("20260923T190000Z", map[string]*string{}, "passed", nil)
	write(t, filepath.Join(h.dir, "live-apply/ada/20260923T190000Z/plan.json"), `{"files": {"x": "no after"}}`, 0o644)
	h.blocked("update_receipt_unreadable")
	if h.sbxCalls() != "" {
		t.Fatalf("sbx calls: %s", h.sbxCalls())
	}
}

func TestHarvestReportsAnEditAnUpdateOverwrote(t *testing.T) {
	// The agent's lesson was replaced by the update; troupe kept his copy in the receipt.
	h := newHarvestHost(t, harvestCase{live: map[string]string{skill: newer, notes: "notes\n"}})
	h.changeSeed(skill, newer)
	h.update("20260923T190000Z", map[string]*string{skill: text(newer)}, "passed", map[string]string{skill: lesson})
	// A link in the receipt is no kept copy.
	if err := os.Symlink(filepath.Join(h.dir, "state"), filepath.Join(h.dir, "live-apply/ada/20260923T190000Z/overwritten/skills/linked.md")); err != nil {
		t.Fatal(err)
	}
	lines, _ := h.collect()
	if len(lines) != 3 || !reflect.DeepEqual(lines[:2], []string{"overwritten_by_update " + skill + " receipt=20260923T190000Z",
		"scratch " + h.collection() + " files=1"}) || !strings.HasPrefix(lines[2], "next: ") {
		t.Fatalf("lines %q", lines)
	}
	folder := filepath.Join("files", skill)
	if h.read(folder+"/overwritten-20260923T190000Z") != lesson ||
		!strings.Contains(h.read(folder+"/overwritten-20260923T190000Z.diff"), "+Lesson from a run.\n") {
		t.Fatal("overwritten copy or diff differs")
	}
	var summary struct{ Overwritten []map[string]any }
	json.Unmarshal([]byte(h.read("summary.json")), &summary)
	if len(summary.Overwritten) != 1 || summary.Overwritten[0]["receipt"] != "20260923T190000Z" || summary.Overwritten[0]["base_receipt"] != "2026-09-23T17:24:06Z" {
		t.Fatalf("summary %v", summary)
	}
}

func TestHarvestDropsAFileAnUpdateDeleted(t *testing.T) {
	h := newHarvestHost(t, harvestCase{live: map[string]string{skill: lesson}})
	h.update("20260923T190000Z", map[string]*string{notes: nil}, "passed", nil)
	h.expected = []string{skill}
	lines, _ := h.collect()
	if !strings.HasPrefix(lines[0], "changed_by_agent "+skill) || count(lines, notes) != 0 {
		t.Fatalf("lines %q", lines)
	}
}

func TestUnifiedDiffMatchesDifflib(t *testing.T) {
	if got := string(unified([]byte("a\nb\n"), []byte("a\nb\n"), "x", "y")); got != "" {
		t.Fatalf("equal inputs: %q", got)
	}
	var a []string
	for i := 1; i <= 20; i++ {
		a = append(a, string(rune('a'+i))+"\n")
	}
	b := append([]string{}, a...)
	b[1], b[17] = "changed\n", "also\n"
	b = append(b[:10], b[11:]...)
	got := string(unified([]byte(strings.Join(a, "")), []byte(strings.Join(b, "")), "base", "live"))
	// difflib.unified_diff(a, b, 'base', 'live') for the same lines.
	want := "--- base\n+++ live\n@@ -1,5 +1,5 @@\n b\n-c\n+changed\n d\n e\n f\n" +
		"@@ -8,13 +8,12 @@\n i\n j\n k\n-l\n m\n n\n o\n p\n q\n r\n-s\n+also\n t\n u\n"
	if got != want {
		t.Fatalf("diff\n%s\nwant\n%s", got, want)
	}
	if got := string(unified([]byte(""), []byte("x"), "base", "live")); got != "--- base\n+++ live\n@@ -0,0 +1 @@\n+x\n\\ No newline at end of file\n" {
		t.Fatalf("diff %q", got)
	}
}

func TestReceiptTimesAreSortedAndOthersSkipped(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20260923T190000Z", "2026-09-22T10:00:00Z", "latest", "2026-09-23T18:00:00Z"} {
		write(t, filepath.Join(dir, name, "cutover.json"), "{}", 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "2026-09-24T00:00:00Z"), 0o755) // no cutover.json
	var names []string
	for _, r := range agent.Receipts(dir, "cutover.json") {
		names = append(names, r.Name)
	}
	if !reflect.DeepEqual(names, []string{"2026-09-22T10:00:00Z", "2026-09-23T18:00:00Z", "20260923T190000Z"}) {
		t.Fatalf("receipts %v", names)
	}
}

// A "created" file under a seeded path would replace the live copy reviewed beside its diff:
// the seeded read wins and the planted copy is never written.
func TestHarvestNeverLetsACreatedFileReplaceASeededOne(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	h.answer(map[string]any{"files": map[string]any{skill: entryOf(lesson), notes: nil}, "ledger": []any{},
		"created": map[string]any{skill: entryOf("payload\n"), notes: entryOf("payload\n")}})
	lines, _ := h.collect()
	if h.read(filepath.Join("files", skill, "live")) != lesson || h.read(filepath.Join("files", notes, "live")) != "" {
		t.Fatalf("planted live copies: %q", lines)
	}
	if contains(lines, "new_file "+skill) || contains(lines, "new_file "+notes) || count(lines, "seeded path") != 2 {
		t.Fatalf("lines %q", lines)
	}
}

// The answer is decoded as typed values with counted lists: an answer within the size cap but
// built to inflate in memory is refused before it is held.
func TestHarvestRefusesInflatedGuestAnswers(t *testing.T) {
	many := func(n int) []string { return make([]string, n) }
	created := map[string]any{}
	for i := 0; i <= maxCreated; i++ {
		created["skills/x/"+strconv.Itoa(i)] = nil
	}
	for name, bad := range map[string]any{
		"long ledger entry": map[string]any{"files": map[string]any{}, "created": map[string]any{},
			"ledger": []any{map[string]any{"ts": "t", "paths": many(maxLedgerPaths + 1), "after": many(maxLedgerPaths + 1)}}},
		"unpaired ledger entry": map[string]any{"files": map[string]any{}, "created": map[string]any{},
			"ledger": []any{map[string]any{"ts": "t", "paths": many(2), "after": many(1)}}},
		"unrequested seeded file": map[string]any{"files": map[string]any{"skills/other.md": nil}, "created": map[string]any{},
			"ledger": []any{}},
		"too many created files": map[string]any{"files": map[string]any{}, "created": created, "ledger": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarvestHost(t, harvestCase{})
			h.answer(bad)
			h.blocked("guest_read_invalid")
		})
	}
}

// A stopped agent is never started to be read: sbx exec would boot his VM.
func TestHarvestRefusesAStoppedAgent(t *testing.T) {
	h := newHarvestHost(t, harvestCase{})
	write(t, filepath.Join(h.tools, "inventory.json"), `[{"name":"ada","id":"fresh","status":"stopped"}]`, 0o644)
	h.blocked("agent_not_running")
	if strings.Contains(h.sbxCalls(), "exec") {
		t.Fatalf("calls: %s", h.sbxCalls())
	}
}
