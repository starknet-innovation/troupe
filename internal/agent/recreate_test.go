package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// The fake machine for a recreate: sbx keeps an inventory and secret records in files, guest
// jobs pass unless fail-JOB exists, systemctl tracks one gateway unit, docker builds kits. sbx refuses kit
// validate, as sbx 0.48 does for v3 source kits. Every call is appended to calls.
const recreateSbx = `#!/bin/sh
d=$(dirname "$0")/..
case "$1" in
  ls) cat "$d/inventory.json" ;;
  stop) echo "sbx stop" >> "$d/calls"; [ -f "$d/fail-stop" ] && exit 1
        sed 's/"running"/"stopped"/' "$d/inventory.json" > "$d/i" && mv "$d/i" "$d/inventory.json" ;;
  rm) echo "sbx rm" >> "$d/calls"; [ -f "$d/fail-rm" ] && exit 1
      python3 -c 'import json,sys; p=sys.argv[1]; json.dump([s for s in json.load(open(p)) if s["name"]!=sys.argv[2]], open(p,"w"))' "$d/inventory.json" "$2" ;;
  env) case " $* " in *" --auto-approve "*|*" -y "*) echo "unknown flag" >&2; exit 1 ;; esac
       if [ "$2" = plan ]; then echo "sbx env plan $3" >> "$d/calls"; [ -f "$d/fail-plan" ] && exit 1; echo "kits: coder ada"; exit 0; fi
       case " $* " in *" create "*" --force "*) ;; *) exit 1 ;; esac
       echo "sbx env create $SBX_ROOT_SIZE_SEEN" >> "$d/calls"; [ -f "$d/fail-create" ] && exit 1
       echo "created with $DOCKER_SANDBOXES_ROOT_SIZE docker=$DOCKER_SANDBOXES_DOCKER_SIZE" >> "$d/calls"
       [ -f "$d/image-ada" ] && echo "sandbox-kit-assembled:new" > "$d/image-ada"
       python3 -c 'import json,sys; p=sys.argv[1]; json.dump([s for s in json.load(open(p)) if s["name"]!="ada"]+[{"name":"ada","id":"new","status":"running"}], open(p,"w"))' "$d/inventory.json" ;;
  kit) echo "sbx kit $2" >> "$d/calls"; exit 1 ;;
  settings) if [ "$2" = get ]; then cat "$d/sources" 2>/dev/null || echo '["docker.io/"]'; else echo "sbx settings set $4" >> "$d/calls"; echo "$4" > "$d/sources"; fi ;;
  inspect) [ -f "$d/image-$2" ] || exit 1; printf '[{"name":"%s","image":"%s"}]' "$2" "$(cat "$d/image-$2")" ;;
  template) if [ "$2" = ls ]; then cat "$d/templates.json" 2>/dev/null || echo '[]'; else echo "sbx template $*" >> "$d/calls"; if [ -f "$d/fail-template-rm" ]; then exit 1; fi; fi ;;
  secret)
    case "$2" in
      ls) if [ "$3" = "--global" ]; then echo '{"secrets":[],"custom_secrets":[]}'; else cat "$d/records.json"; fi ;;
      set-custom) case " $* " in *" --force "*) exit 1 ;; esac
        echo "sbx secret set-custom $*" >> "$d/calls"
        env=$(echo "$*" | sed 's/.*--env \([A-Z_]*\).*/\1/'); ph=$(echo "$*" | sed 's/.*--placeholder \([^ ]*\).*/\1/')
        python3 - "$d/records.json" "$env" "$ph" <<'PY'
import json, sys
path, env, ph = sys.argv[1:]
data = json.load(open(path))
data["custom_secrets"] = [c for c in data["custom_secrets"] if c["env"] != env] + [{"env": env, "scope": "ada", "placeholder": ph}]
json.dump(data, open(path, "w"))
PY
        ;;
    esac ;;
  exec)
    if [ "$2" = -it ]; then echo "sbx $*" >> "$d/calls"; exit 0; fi
    cat > /dev/null
    while [ "$1" != "sh" ] || [ "$2" = "-c" ]; do shift; done
    shift 3
    echo "job $*" >> "$d/calls"
    job=$1; [ "$job" = kit ] && job="kit-$2-$3"; [ "$job" = slack ] && job="slack-$2"
    if [ -f "$d/answer-$job" ]; then cat "$d/answer-$job"; exit 0; fi
    if [ -f "$d/fail-$job" ]; then echo '{"status": "failed", "class": "planted_failure", "output": "x"}'; exit 1; fi
    case "$1" in
      drain) if [ "$2" = cancel ]; then echo '{"running": true}'; else echo '{"running": true, "marker": true, "fresh": true, "state": "draining", "active": 0, "work": []}'; fi ;;
      *) echo '{"status": "passed", "output": "fine"}' ;;
    esac ;;
esac
`

const recreateSystemctl = `#!/bin/sh
d=$(dirname "$0")/..
case "$1" in
  show) case "$2" in hermes-gateway@*) cat "$d/mainpid" ;; *.service) echo 0 ;; *) case "$*" in *hermes-gateway@*) cat "$d/mainpid" ;; *) echo 0 ;; esac ;; esac ;;
  is-active) case "$2" in *.timer) cat "$d/timer" ;; hermes-gateway@ada.service) cat "$d/active" ;; *) echo inactive ;; esac ;;
  stop) echo "systemctl stop $2" >> "$d/calls"; case "$2" in *.timer) echo inactive > "$d/timer" ;; *) echo 0 > "$d/mainpid"; echo inactive > "$d/active" ;; esac ;;
  start) echo "systemctl start $2" >> "$d/calls"; case "$2" in *.timer) echo active > "$d/timer" ;; *) echo 5555 > "$d/mainpid"; echo active > "$d/active" ;; esac ;;
  *) echo "systemctl $*" >> "$d/calls" ;;
esac
`

// recreateDocker is the host's docker CLI: it records each kit build (folder and descriptor)
// and fails one when fail-build-FOLDER exists.
const recreateDocker = `#!/bin/sh
d=$(dirname "$0")/..
[ "$1 $2" = "buildx build" ] || exit 2
echo "docker build $(basename "$3") $4 $(basename "$5") $6 $7" >> "$d/calls"
[ -f "$d/fail-build-$(basename "$3")" ] && { echo "planted build failure" >&2; exit 1; }
exit 0
`

const recreateCreds = `#!/bin/sh
exit 0
`

type rig struct {
	t        *testing.T
	dir      string
	agent    *Agent
	opts     RecreateOptions
	out, err bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, dir: dir}
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	for name, text := range map[string]string{"sbx": recreateSbx, "systemctl": recreateSystemctl, "sudo": fakeSudo,
		"docker": recreateDocker, "systemd-creds": recreateCreds} {
		os.WriteFile(filepath.Join(bin, name), []byte(text), 0o755)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	os.MkdirAll(filepath.Join(dir, "sandboxd/containerd/state/io.containerd.runtime.v2.task/docker"), 0o755)
	r.write("mainpid", "4242\n")
	r.write("active", "active\n")
	r.write("timer", "active\n")
	r.write("inventory.json", `[{"name":"ada","id":"old","status":"running"},{"name":"grace","id":"g","status":"stopped"}]`)
	r.write("records.json", `{"secrets":[{"scope":"ada","type":"service","name":"venice"}],"custom_secrets":[]}`)

	paths := guard.Paths{Pairs: filepath.Join(dir, "pairs"), Sandboxd: filepath.Join(dir, "sandboxd")}
	noProbe := func(context.Context) error { return nil }
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	runner := sbx.Runner{Paths: paths, Binary: filepath.Join(bin, "sbx"), Log: filepath.Join(dir, "native.log"), Probe: noProbe}
	r.agent = &Agent{
		Name: "ada", Command: "reset", Paths: paths, Slack: true,
		Terminal: strings.NewReader(""), Probe: noProbe, State: filepath.Join(dir, "state"),
		Printer: &progress.Printer{Agent: "Ada", Out: &r.out, Err: &r.err, Now: func() time.Time { return clock }},
		Runner:  runner,
		Timing: Timing{ReadyTries: 3, ReadyEvery: time.Millisecond, StopWait: 50 * time.Millisecond, StopEvery: time.Millisecond,
			Keepalive: time.Hour, Drain: DrainTiming{Poll: time.Millisecond, Refresh: time.Hour, Note: time.Hour}},
	}
	kits := r.kits()
	kit, err := ReadKit(registry.Registry{Root: kits}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	services, err := secret.Services("ada", []string{"venice"}, filepath.Join(kits, "kits", "coder"))
	if err != nil {
		t.Fatal(err)
	}
	store := &secret.Store{Agent: "ada", Services: services, Root: filepath.Join(dir, "credentials"), Runner: runner}
	r.ciphertexts(store)
	r.opts = RecreateOptions{Kit: kit, Secrets: store, Units: func() error { return nil },
		CollectorTimer: "collector.timer",
		Hold: func(context.Context, string) (func(), error) {
			r.call("holder start")
			return func() { r.call("holder stop") }, nil
		}}
	return r
}

func (r *rig) write(name, text string) {
	r.t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(r.dir, name)), 0o755)
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) call(line string) {
	f, _ := os.OpenFile(filepath.Join(r.dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, line)
	f.Close()
}

func (r *rig) calls() string {
	data, _ := os.ReadFile(filepath.Join(r.dir, "calls"))
	return string(data)
}

// testHermes is the Hermes version the rig's workload dockerfile names.
const testHermes = "0.9.7"

// kits writes a v3 workload kit (kits/coder) with Slack and both kit tools, and the agent ada
// (agents/ada), a mixin on it.
func (r *rig) kits() string {
	root := filepath.Join(r.dir, "kits")
	r.write("kits/kits/coder/coder.yaml", "# syntax=docker/sandbox-kit:3\nschemaVersion: \"3\"\nkind: workload\nversion: 1.0.0\nprovides: [\"coder@1.0.0\"]\n")
	r.write("kits/kits/coder/coder.dockerfile", realHermes)
	r.write("kits/kits/coder/.dockerignore", "**/__pycache__\n")
	r.write("kits/kits/coder/slack.env", "SLACK_WORKSPACE=T0AAAAAAAA\nSLACK_BOUNDARY_PUBLIC=C0BBBBBBBB\nSLACK_BOUNDARY_PRIVATE=C0CCCCCCCC\n")
	r.write("kits/kits/coder/slack-app-manifest.json", `{"oauth_config":{"scopes":{"bot":["chat:write","im:history"]}}}`)
	r.write("kits/kits/coder/secrets.json", `{"placeholders":[
 {"service":"slack-bot","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-bot"},
 {"service":"slack-app","env":"SLACK_APP_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-app"}]}`)
	r.write("kits/kits/coder/tools/seed-check.py", "# kit seed rules\n")
	r.write("kits/kits/coder/tools/setup.py", "# kit setup\n")
	r.write("kits/kits/coder/files/home/.hermes/config.yaml", "model: x\n")
	r.write("kits/agents/ada/sbxenv.yaml", "schemaVersion: \"1\"\nname: ada\nagent: coder\nkits:\n  - \"../../kits/coder\"\n  - \".\"\nsandboxOptions:\n  cpus: 2\n  memory: 4g\n")
	r.write("kits/agents/ada/ada.yaml", "# syntax=docker/sandbox-kit:3\nschemaVersion: \"3\"\nkind: mixin\nversion: 1.0.0\nprovides: [\"ada@1.0.0\"]\nrequires: [\"coder\"]\n")
	r.write("kits/agents/ada/ada.dockerfile", "COPY files/home/.hermes /home/agent/.hermes\n")
	r.write("kits/agents/ada/storage.env", "DOCKER_SANDBOXES_ROOT_SIZE=20g\nDOCKER_SANDBOXES_DOCKER_SIZE=5g\n")
	r.write("kits/agents/ada/.dockerignore", "sbxenv.yaml\nstorage.env\n")
	r.write("kits/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=C0DDDDDDDD\nSLACK_ALLOWED_USERS=U0EEEEEEEE\n")
	r.write("kits/agents/ada/files/home/.hermes/SOUL.md", "I am Ada.\n")
	return root
}

func (r *rig) ciphertexts(store *secret.Store) {
	header, _ := hex.DecodeString("ef4ac13679a9480ea7db68897f9f165d")
	data := base64.StdEncoding.EncodeToString(append(header, bytes.Repeat([]byte{7}, 120)...))
	os.MkdirAll(filepath.Join(store.Root, "ada"), 0o700)
	os.Chmod(store.Root, 0o700)
	for _, svc := range store.Services {
		os.WriteFile(store.File(svc), []byte(data), 0o600)
	}
}

func (r *rig) run() error {
	return r.agent.Recreate(context.Background(), r.opts)
}

func (r *rig) current(name string) string {
	return filepath.Join(r.agent.State, "ada", "reseed", "current", name)
}

func (r *rig) completed() []string {
	entries, _ := os.ReadDir(filepath.Join(r.agent.State, "ada", "reseed", "completed"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// order fails unless each line appears in calls, in this order.
func (r *rig) order(lines ...string) {
	r.t.Helper()
	calls := r.calls()
	at := 0
	for _, line := range lines {
		i := strings.Index(calls[at:], line)
		if i < 0 {
			r.t.Fatalf("missing or out of order: %q\ncalls:\n%s", line, calls)
		}
		at += i + len(line)
	}
}

func TestRecreateRunsEveryStepInOrder(t *testing.T) {
	r := newRig(t)
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s%s\ncalls:\n%s", err, r.out.String(), r.err.String(), r.calls())
	}
	r.order("systemctl stop collector.timer", "sbx env plan "+r.opts.Kit.sbxenv,
		"docker build coder -f coder.yaml --output type=cacheonly", "docker build ada -f ada.yaml --output type=cacheonly",
		"job kit seed-check.py validate", "job drain",
		"systemctl stop hermes-gateway@ada.service", "job gateway-stopped", "sbx stop", "sbx rm",
		"created with 20g", "holder start", "job fresh", "job kit seed-check.py check",
		"sbx secret set-custom", "job slack setup", "job kit setup.py run",
		"systemctl enable hermes-gateway@ada.service", "systemctl start hermes-gateway@ada.service",
		"job ready --platform slack", "holder stop", "systemctl start collector.timer")
	if !strings.Contains(r.calls(), "job kit seed-check.py validate") {
		t.Fatal("the kit's seed rules did not run in the old VM before removal")
	}
	// A local kit is built from its source, and the line says so.
	if !strings.Contains(r.out.String(), "Ada creation: Building the image and creating the new VM from the kits.") {
		t.Fatalf("output:\n%s", r.out.String())
	}
	if strings.Index(r.calls(), "job kit seed-check.py validate") > strings.Index(r.calls(), "sbx stop") {
		t.Fatal("the kit's seed rules ran after the VM was stopped")
	}
	done := r.completed()
	if len(done) != 1 {
		t.Fatalf("completed %v", done)
	}
	if _, err := os.Stat(r.current("")); err == nil {
		t.Fatal("the transaction folder was left open")
	}
	// The receipt keeps the composed seeds' bytes, what later updates measure edits against.
	checkInstalled(t, filepath.Join(r.agent.State, "ada", "reseed", "completed"), done[0])
	if info, err := os.Stat(filepath.Join(r.agent.State, "ada", "reseed", "completed", done[0], "seeds", "SOUL.md")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kept seed file: %v %v", info, err)
	}
	if !strings.Contains(r.out.String(), "Ada reset: ok") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

func TestFailureBeforeRemovalStartsTheOldGatewayAgain(t *testing.T) {
	r := newRig(t)
	r.write("fail-stop", "")
	r.agent.Timing.StopWait = time.Millisecond
	err := r.run()
	if err == nil {
		t.Fatal("passed")
	}
	if strings.Contains(r.calls(), "sbx rm") || strings.Contains(r.calls(), "sbx env create") {
		t.Fatalf("removed or created after a failed stop:\n%s", r.calls())
	}
	r.order("systemctl stop hermes-gateway@ada.service", "job drain cancel", "systemctl start hermes-gateway@ada.service",
		"systemctl start collector.timer")
}

func TestFailureAfterRemovalNeverRestoresAndResumes(t *testing.T) {
	r := newRig(t)
	r.write("fail-create", "")
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	if strings.Contains(r.calls(), "systemctl start hermes-gateway") {
		t.Fatalf("restarted a gateway after removal:\n%s", r.calls())
	}
	if _, err := os.Stat(r.current("removed.json")); err != nil {
		t.Fatal("the removal receipt is missing")
	}
	// Fixed and run again: no second drain or removal, the creation resumes and finishes.
	os.Remove(filepath.Join(r.dir, "fail-create"))
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err != nil {
		t.Fatalf("resume: %v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "job drain") || strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("the resumed run drained or removed again:\n%s", r.calls())
	}
	r.order("created with 20g", "job fresh", "systemctl start hermes-gateway@ada.service")
}

func TestResumeAfterFreshChecksSkipsThem(t *testing.T) {
	r := newRig(t)
	r.write("fail-slack-setup", "")
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	os.Remove(filepath.Join(r.dir, "fail-slack-setup"))
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err != nil {
		t.Fatalf("resume: %v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "job fresh") || strings.Contains(r.calls(), "sbx env create") {
		t.Fatalf("the resumed run repeated finished steps:\n%s", r.calls())
	}
	r.order("job slack setup", "systemctl start hermes-gateway@ada.service")
}

func TestChangedIdentityBeforeRemovalIsRefused(t *testing.T) {
	r := newRig(t)
	r.agent.Timing.StopWait = time.Millisecond
	r.write("fail-stop", "") // stop the first run after its preflight
	r.run()
	os.Remove(filepath.Join(r.dir, "fail-stop"))
	r.write("inventory.json", `[{"name":"ada","id":"someone-else","status":"running"}]`)
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "unexpected_vm_identity") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") || strings.Contains(r.calls(), "sbx stop") {
		t.Fatalf("acted on another VM:\n%s", r.calls())
	}
}

func TestChangedSeedsAreRefused(t *testing.T) {
	r := newRig(t)
	r.write("fail-create", "")
	r.run()
	r.write("kits/agents/ada/files/home/.hermes/SOUL.md", "I am someone else.\n")
	kit, err := ReadKit(registry.Registry{Root: filepath.Join(r.dir, "kits")}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	r.opts.Kit = kit
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "seeds_changed") {
		t.Fatalf("err %v", err)
	}
}

func TestCreate(t *testing.T) {
	r := newRig(t)
	r.write("inventory.json", `[]`)
	r.write("active", "inactive\n")
	r.write("mainpid", "0\n")
	r.opts.Create = true
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "job drain") || strings.Contains(r.calls(), "sbx rm") || strings.Contains(r.calls(), "seed-check.py validate") {
		t.Fatalf("a creation acted on a VM that does not exist:\n%s", r.calls())
	}
	r.order("created with 20g", "job fresh", "systemctl start hermes-gateway@ada.service")
	// The agent now existed: a second creation is refused, and so is recreating an existing VM.
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "create_refused_agent_existed_before") {
		t.Fatalf("second create: %v", err)
	}
	r2 := newRig(t)
	r2.opts.Create = true
	if err := r2.run(); err == nil || !strings.Contains(err.Error(), "create_refused_sandbox_exists") {
		t.Fatalf("create over an existing VM: %v", err)
	}
}

func TestUnwiredPlaceholderStopsBeforeTheFirstStart(t *testing.T) {
	r := newRig(t)
	// A stray global-looking record for the bot token: wiring must refuse, not paper over it.
	r.write("records.json", `{"secrets":[{"scope":"ada","type":"service","name":"venice"}],"custom_secrets":[{"env":"SLACK_BOT_TOKEN","scope":"(global)","placeholder":"x"}]}`)
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "unexpected_sandbox_record") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "systemctl start hermes-gateway") {
		t.Fatalf("started with an unexpected record:\n%s", r.calls())
	}
}

func TestGenericSeedChecks(t *testing.T) {
	r := newRig(t)
	root := filepath.Join(r.dir, "kits")
	for name, plant := range map[string][2]string{
		"slack id in soul":       {"kits/agents/ada/files/home/.hermes/SOUL.md", "Post in C0DDDDDDDD.\n"},
		"home is a boundary":     {"kits/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=C0BBBBBBBB\nSLACK_ALLOWED_USERS=U0EEEEEEEE\n"},
		"home is another member": {"kits/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=U0FFFFFFFF\nSLACK_ALLOWED_USERS=U0EEEEEEEE,U0FFFFFFFF\n"},
		"wide allowlist":         {"kits/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=C0DDDDDDDD\nSLACK_ALLOWED_USERS=*\n"},
		"no manager":             {"kits/agents/ada/files/home/.hermes/.env", "SLACK_HOME_CHANNEL=C0DDDDDDDD\nSLACK_ALLOWED_USERS=U0EEEEEEEE\n"},
		"manager not allowed":    {"kits/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=C0DDDDDDDD\nSLACK_ALLOWED_USERS=U0FFFFFFFF\n"},
		"repeated key":           {"kits/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=C0DDDDDDDD\nSLACK_HOME_CHANNEL=C0DDDDDDDD\nSLACK_ALLOWED_USERS=U0EEEEEEEE\n"},
		"overlap":                {"kits/agents/ada/files/home/.hermes/config.yaml", "model: y\n"},
	} {
		t.Run(name, func(t *testing.T) {
			before, _ := os.ReadFile(filepath.Join(r.dir, plant[0]))
			r.write(plant[0], plant[1])
			defer func() {
				if before == nil {
					os.Remove(filepath.Join(r.dir, plant[0]))
				} else {
					os.WriteFile(filepath.Join(r.dir, plant[0]), before, 0o644)
				}
			}()
			if _, err := ReadKit(registry.Registry{Root: root}, "ada"); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// The negative control: the untouched kit passes.
	if _, err := ReadKit(registry.Registry{Root: root}, "ada"); err != nil {
		t.Fatal(err)
	}
}

func TestAnOpenReplacementBlocksARecreate(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.agent.State, "ada", "current"), 0o700)
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "other_maintenance_pending") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "job") || strings.Contains(r.calls(), "systemctl stop") {
		t.Fatalf("acted:\n%s", r.calls())
	}
}

// A VM whose process is still alive after sbx stop is never removed.
func TestARunningVMIsNeverRemoved(t *testing.T) {
	r := newRig(t)
	r.agent.Timing.StopWait = time.Millisecond
	task := filepath.Join(r.dir, "sandboxd/containerd/state/io.containerd.runtime.v2.task/docker/t1")
	os.MkdirAll(task, 0o755)
	os.WriteFile(filepath.Join(task, "config.json"), []byte(`{"hostname":"ada"}`), 0o644)
	os.WriteFile(filepath.Join(task, "shim.pid"), []byte(fmt.Sprint(os.Getpid())), 0o644)
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "vm_still_running") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("removed a running VM:\n%s", r.calls())
	}
}

// A VM that vanished without this run's removal receipt is never replaced by a new one.
func TestAMissingVMNeedsTheRemovalReceipt(t *testing.T) {
	r := newRig(t)
	r.agent.Timing.StopWait = time.Millisecond
	r.write("fail-stop", "")
	r.run() // authorized the removal, then failed to stop the VM
	r.write("inventory.json", `[]`)
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "removal_receipt_missing") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx env create") {
		t.Fatalf("created a VM:\n%s", r.calls())
	}
}

// stopped makes ada's VM stopped with its gateway down, as troupe stop leaves it.
func (r *rig) stopped(other string) {
	r.write("inventory.json", fmt.Sprintf(`[{"name":"ada","id":"old","status":"stopped"},{"name":"grace","id":"g","status":%q}]`, other))
	r.write("active", "inactive\n")
	r.write("mainpid", "0\n")
}

// Recreating a stopped agent never execs into its old VM (an exec would boot it).
func TestAStoppedAgentIsRecreatedWithoutBootingIt(t *testing.T) {
	r := newRig(t)
	r.stopped("stopped")
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	before := r.calls()[:strings.Index(r.calls(), "sbx rm")]
	if strings.Contains(before, "job") {
		t.Fatalf("exec into the stopped VM before its removal:\n%s", before)
	}
	r.order("sbx rm", "created with 20g", "job fresh")
}

// A failure before removal never starts a gateway that was stopped before the recreate: the
// recovery restores only what the run itself stopped (it would boot the VM, past capacity).
func TestFailureBeforeRemovalLeavesAStoppedAgentStopped(t *testing.T) {
	r := newRig(t)
	r.stopped("stopped")
	r.write("fail-rm", "")
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	if !strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("never reached the removal:\n%s", r.calls())
	}
	if strings.Contains(r.calls(), "systemctl start hermes-gateway") {
		t.Fatalf("started the stopped agent's gateway:\n%s", r.calls())
	}
}

// A resumed run decides from the gateway as it is now, not from the first run's receipt: started
// by the operator in between, it is started again when the resumed run fails.
func TestResumedRunRestartsAGatewayStartedInBetween(t *testing.T) {
	r := newRig(t)
	r.stopped("stopped")
	r.write("fail-rm", "")
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	// The operator starts the agent; the next run resumes the same transaction.
	r.write("inventory.json", `[{"name":"ada","id":"old","status":"running"},{"name":"grace","id":"g","status":"stopped"}]`)
	r.write("active", "active\n")
	r.write("mainpid", "4242\n")
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	r.order("systemctl stop hermes-gateway@ada.service", "sbx rm", "systemctl start hermes-gateway@ada.service")
}

// A gateway systemd is restarting (activating, no main PID yet) is up: recovery starts it again.
func TestARestartingGatewayCountsAsUp(t *testing.T) {
	r := newRig(t)
	r.write("active", "activating\n")
	r.write("mainpid", "0\n")
	r.write("fail-rm", "")
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	r.order("systemctl stop hermes-gateway@ada.service", "sbx rm", "systemctl start hermes-gateway@ada.service")
}

// A resumed activation enables the gateway again before starting it: a troupe stop between the
// attempts disabled it, and the finished agent must start at boot.
func TestResumedActivationEnablesTheGateway(t *testing.T) {
	r := newRig(t)
	r.write("fail-ready", "")
	if err := r.run(); err == nil {
		t.Fatal("passed")
	}
	if _, err := os.Stat(r.current("activation-attempted.json")); err != nil {
		t.Fatalf("failed before the activation:\n%s", r.calls())
	}
	os.Remove(filepath.Join(r.dir, "fail-ready"))
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err != nil {
		t.Fatalf("resume: %v\n%s", err, r.calls())
	}
	r.order("systemctl enable hermes-gateway@ada.service", "systemctl start hermes-gateway@ada.service")
}

// A Docker disk size exported in the shell never reaches sbx env create.
func TestOnlyTheDeclaredDisksReachTheCreation(t *testing.T) {
	r := newRig(t)
	t.Setenv("DOCKER_SANDBOXES_DOCKER_SIZE", "99g")
	r.write("kits/agents/ada/storage.env", "DOCKER_SANDBOXES_ROOT_SIZE=20g\n")
	kit, err := ReadKit(registry.Registry{Root: filepath.Join(r.dir, "kits")}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	r.opts.Kit = kit
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if !strings.Contains(r.calls(), "created with 20g docker=\n") {
		t.Fatalf("calls:\n%s", r.calls())
	}
}

// The collector pause shares the host's one record with the bash guards: an earlier pause's
// state wins, and a phase record without a copy keeps the timer stopped (fail closed).
func TestCollectorPauseSharesTheHostRecord(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.agent.Paths.Pairs, 0o700)
	record := filepath.Join(r.agent.Paths.Pairs, "collector.json")
	os.WriteFile(record, []byte(`{"previous_state":"active"}`), 0o600)
	r.write("timer", "inactive\n") // stopped by the earlier pause
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if _, err := os.Stat(record); err == nil || !strings.Contains(r.calls(), "systemctl start collector.timer") {
		t.Fatalf("the earlier pause's active timer was not restored:\n%s", r.calls())
	}

	r2 := newRig(t)
	os.MkdirAll(r2.agent.Paths.Pairs, 0o700)
	os.WriteFile(filepath.Join(r2.agent.Paths.Pairs, "other.json"), []byte(`{"phase":"complete"}`), 0o600)
	if err := r2.run(); err == nil || !strings.Contains(err.Error(), "collector_restore_failed") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r2.calls(), "systemctl start collector.timer") {
		t.Fatal("restarted the collector beside a record naming no copy")
	}
	if _, err := os.Stat(filepath.Join(r2.agent.Paths.Pairs, "collector.json")); err != nil {
		t.Fatal("the pause record was dropped")
	}
}

func (r *rig) check() error {
	r.t.Helper()
	return r.agent.Check(context.Background(), r.opts)
}

// check proves a recreate would pass and changes nothing: no sudo, receipt, unit or VM change.
func TestCheckChangesNothing(t *testing.T) {
	r := newRig(t)
	if err := r.check(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	for _, forbidden := range []string{"sudo", "systemctl stop", "systemctl start", "systemctl enable", "systemctl disable",
		"sbx stop", "sbx rm", "sbx env create", "sbx kit", "job"} {
		if strings.Contains(r.calls(), forbidden) {
			t.Fatalf("check ran %q:\n%s", forbidden, r.calls())
		}
	}
	if _, err := os.Stat(filepath.Join(r.agent.State, "ada")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(r.agent.State, "ada"))
		t.Fatalf("check left state behind: %v", entries)
	}
	if !strings.Contains(r.out.String(), "A reset would pass its preflight now") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

// check refuses what the recreate's preflight would refuse, before anything changes.
func TestCheckRefusesWhatPreflightWould(t *testing.T) {
	cases := []struct {
		name, want string
		prepare    func(r *rig)
	}{
		{"kits sbx would refuse", "kit_invalid", func(r *rig) { r.write("fail-plan", "") }},
		{"a kit that does not build", "kit_build_failed: ada", func(r *rig) { r.write("fail-build-ada", "") }},
		{"no docker CLI", "docker_cli_missing", func(r *rig) { r.withoutDocker() }},
		{"open run with changed seeds", "seeds_changed", func(r *rig) {
			r.write("fail-rm", "")
			r.run()
			r.opts.Kit.Manifest = append([]byte("changed "), r.opts.Kit.Manifest...)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			c.prepare(r)
			if err := r.check(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v\n%s", err, r.calls())
			}
		})
	}
}

// realHermes is the shape the real kits use: the version named once as a global ARG, then
// brought into the build stage by a bare ARG (Dockerfile scoping).
var realHermes = "ARG HERMES_VERSION=" + testHermes + "\nFROM debian\nARG HERMES_VERSION\n"

// The Hermes version is read from the workload dockerfile's one ARG HERMES_VERSION line, and
// recorded in the preflight receipt; anything else is refused before any change.
func TestKitReadsTheHermesVersion(t *testing.T) {
	r := newRig(t)
	if r.opts.Kit.Hermes != testHermes {
		t.Fatalf("hermes %q", r.opts.Kit.Hermes)
	}
	root := filepath.Join(r.dir, "kits")
	for name, dockerfile := range map[string]string{
		"missing":       "FROM debian\n",
		"no value":      "FROM debian\nARG HERMES_VERSION\n",
		"twice, scoped": "ARG HERMES_VERSION=" + testHermes + "\nFROM debian\nARG HERMES_VERSION=" + testHermes + "\n",
		"short":         "ARG HERMES_VERSION=0.9\n",
		"a tag":         "ARG HERMES_VERSION=v" + testHermes + "\n",
		"trailing text": "ARG HERMES_VERSION=" + testHermes + "x\n",
		"twice":         "ARG HERMES_VERSION=" + testHermes + "\nARG HERMES_VERSION=" + testHermes + "\n",
		"a branch":      "ARG HERMES_VERSION=main\n",
	} {
		t.Run(name, func(t *testing.T) {
			r.write("kits/kits/coder/coder.dockerfile", dockerfile)
			if _, err := ReadKit(registry.Registry{Root: root}, "ada"); err == nil || !strings.Contains(err.Error(), "HERMES_VERSION") {
				t.Fatalf("err %v", err)
			}
		})
	}
	r.write("kits/kits/coder/coder.dockerfile", realHermes)
	if err := os.Remove(filepath.Join(r.dir, "kits/kits/coder/coder.dockerfile")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKit(registry.Registry{Root: root}, "ada"); err == nil {
		t.Fatal("a workload without its dockerfile was accepted")
	}

	r2 := newRig(t)
	r2.write("fail-create", "")
	r2.run()
	var pre map[string]any
	if !readReceipt(r2.current("preflight.json"), &pre) || pre["target_hermes"] != testHermes || pre["schema"] != "replacement-v3" {
		t.Fatalf("preflight receipt %v", pre)
	}
	for _, gone := range []string{"target_image", "image_id"} {
		if _, ok := pre[gone]; ok {
			t.Fatalf("the receipt still records %s: %v", gone, pre)
		}
	}
}

// The declarations hash covers every file sbx builds and creates the VM from.
func TestDeclarationsCoverTheKitFiles(t *testing.T) {
	r := newRig(t)
	root := filepath.Join(r.dir, "kits")
	before := r.opts.Kit.Declarations
	for _, file := range []string{"kits/kits/coder/coder.dockerfile", "kits/kits/coder/coder.yaml", "kits/kits/coder/.dockerignore",
		"kits/agents/ada/ada.yaml", "kits/agents/ada/ada.dockerfile", "kits/agents/ada/.dockerignore", "kits/agents/ada/sbxenv.yaml", "kits/agents/ada/storage.env"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(r.dir, file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer os.WriteFile(path, original, 0o644)
			os.WriteFile(path, append(append([]byte(nil), original...), "# changed\n"...), 0o644)
			kit, err := ReadKit(registry.Registry{Root: root}, "ada")
			if err != nil {
				t.Fatal(err)
			}
			if kit.Declarations == before {
				t.Fatalf("changing %s left the declarations unchanged", file)
			}
		})
	}
	// The negative control: unchanged kits hash the same.
	if kit, err := ReadKit(registry.Registry{Root: root}, "ada"); err != nil || kit.Declarations != before {
		t.Fatalf("unchanged kits: %v", err)
	}
}

// A resumed recreate refuses kits whose workload dockerfile changed since its preflight.
func TestAChangedDockerfileIsRefusedOnResume(t *testing.T) {
	r := newRig(t)
	r.write("fail-create", "")
	r.run()
	os.Remove(filepath.Join(r.dir, "fail-create"))
	r.write("kits/kits/coder/coder.dockerfile", "FROM debian:trixie\nARG HERMES_VERSION="+testHermes+"\n")
	kit, err := ReadKit(registry.Registry{Root: filepath.Join(r.dir, "kits")}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	r.opts.Kit = kit
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "declarations_changed") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx env create") {
		t.Fatalf("created from changed kits:\n%s", r.calls())
	}
}

// Kits sbx env plan refuses stop the recreate before anything changes.
func TestKitsSbxRefusesStopThePreflight(t *testing.T) {
	r := newRig(t)
	r.write("fail-plan", "")
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "kit_invalid") {
		t.Fatalf("err %v", err)
	}
	for _, step := range []string{"job", "systemctl stop hermes-gateway", "sbx stop", "sbx rm", "sbx env create"} {
		if strings.Contains(r.calls(), step) {
			t.Fatalf("%s ran:\n%s", step, r.calls())
		}
	}
	if _, err := os.Stat(r.current("preflight.json")); err == nil {
		t.Fatal("a refused preflight left a receipt")
	}
}

// An open transaction an older troupe recorded (replacement-v2, image based) is never resumed.
func TestAnOlderReceiptSchemaIsRefused(t *testing.T) {
	r := newRig(t)
	r.write("fail-create", "")
	r.run()
	os.Remove(filepath.Join(r.dir, "fail-create"))
	data, err := os.ReadFile(r.current("preflight.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.current("preflight.json"), bytes.Replace(data, []byte("replacement-v3"), []byte("replacement-v2"), 1), 0o600)
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.check(); err == nil || !strings.Contains(err.Error(), "receipt_schema_outdated") {
		t.Fatalf("check: %v", err)
	}
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "receipt_schema_outdated") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx env create") || strings.Contains(r.calls(), "job") {
		t.Fatalf("resumed an older transaction:\n%s", r.calls())
	}
}

// The example kit in this repository reads as a complete kit.
func TestTheExampleKitReads(t *testing.T) {
	root, err := filepath.Abs("../../examples/kit")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ada", "grace"} {
		kit, err := ReadKit(registry.Registry{Root: root}, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !semver.MatchString(kit.Hermes) || kit.Base != filepath.Join(root, "kits", "coder") {
			t.Fatalf("%s: hermes %q base %s", name, kit.Hermes, kit.Base)
		}
	}
}

// A kit that does not build stops the run before anything is stopped or removed, and a resumed
// run that has not removed the old VM builds again first.
func TestAFailingKitBuildStopsBeforeRemoval(t *testing.T) {
	for _, kit := range []string{"coder", "ada"} {
		t.Run(kit, func(t *testing.T) {
			r := newRig(t)
			r.write("fail-build-"+kit, "")
			if err := r.run(); err == nil || !strings.Contains(err.Error(), "kit_build_failed: "+kit) {
				t.Fatalf("err %v\n%s", err, r.calls())
			}
			for _, step := range []string{"job drain", "systemctl stop hermes-gateway", "sbx stop", "sbx rm", "sbx env create"} {
				if strings.Contains(r.calls(), step) {
					t.Fatalf("%s ran after a failed build:\n%s", step, r.calls())
				}
			}
			if _, err := os.Stat(r.current("preflight.json")); err == nil {
				t.Fatal("a refused preflight left a receipt")
			}
		})
	}
	// Interrupted before the removal, then the kit stops building: the resumed run refuses.
	r := newRig(t)
	r.agent.Timing.StopWait = time.Millisecond
	r.write("fail-stop", "")
	r.run()
	os.Remove(filepath.Join(r.dir, "fail-stop"))
	r.write("fail-build-coder", "")
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "kit_build_failed: coder") {
		t.Fatalf("resume: %v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "sbx rm") || strings.Contains(r.calls(), "sbx env create") {
		t.Fatalf("removed or created after a failed build:\n%s", r.calls())
	}
}

// withoutDocker removes the fake docker and every PATH folder holding a docker, keeping the rest.
func (r *rig) withoutDocker() {
	os.Remove(filepath.Join(r.dir, "bin", "docker"))
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "docker")); err != nil {
			kept = append(kept, dir)
		}
	}
	r.t.Setenv("PATH", strings.Join(kept, string(os.PathListSeparator)))
}

// publishedRef is the pinned kit a published ada is created from.
const publishedRef = "ghcr.io/example/coder:1.2.0"

// published turns the rig's ada into a published agent: troupe's data folder holds her
// environment on a pinned, kept kit with no Slack and no tools, and docker also pulls.
func (r *rig) published() string {
	data := filepath.Join(r.dir, "data")
	kit := registry.KitFolder(data, publishedRef)
	rel := func(path string) string { p, _ := filepath.Rel(r.dir, path); return p }
	r.write(rel(filepath.Join(kit, registry.PublishedFile)), `{"reference":"`+publishedRef+`","type":"coder","version":"1.2.0","hermes":"`+testHermes+`"}`)
	r.write(rel(filepath.Join(kit, "services.json")), `{"services":[{"name":"venice","prompt":"Venice API key","required":true,"delivery":"native","domains":["api.venice.ai"]}]}`)
	r.write(rel(filepath.Join(kit, "files/home/.hermes/config.yaml")), "model: x\n")
	r.write("data/agents/ada/sbxenv.yaml", "schemaVersion: \"1\"\nname: ada\nagent: coder\n\nkits:\n  - \""+publishedRef+"\"\n  - \".\"\n\nsandboxOptions:\n  cpus: 2\n  memory: 4g\n")
	r.write("data/agents/ada/ada.yaml", "# syntax=docker/sandbox-kit:3\nschemaVersion: \"3\"\nkind: mixin\nversion: \"1.0.0\"\nprovides: [\"ada@1.0.0\"]\nrequires: [\"coder\"]\n")
	r.write("data/agents/ada/ada.dockerfile", "FROM scratch\n")
	r.write("data/agents/ada/.dockerignore", "sbxenv.yaml\nstorage.env\nsource.json\n")
	r.write("data/agents/ada/storage.env", "DOCKER_SANDBOXES_ROOT_SIZE=20g\n")
	r.write("data/agents/ada/files/home/.hermes/.env", "# written by troupe\n")
	r.write("data/agents/ada/files/home/.hermes/SOUL.md", "I am Ada.\n")
	r.write("bin/docker", `#!/bin/sh
d=$(dirname "$0")/..
if [ "$1" = pull ]; then echo "docker pull $3" >> "$d/calls"; [ -f "$d/fail-pull" ] && exit 1; exit 0; fi
echo "docker build $(basename "$3") $4 $(basename "$5") $6 $7" >> "$d/calls"
exit 0
`)
	os.Chmod(filepath.Join(r.dir, "bin/docker"), 0o755)
	return data
}

// Recreating a published agent (reset, resize) makes sbx accept its kit's registry before the old
// VM is removed: sbx env create reads the kit after, and a fresh sbx accepts only docker.io/.
// A setting troupe cannot read stops it while the old VM still runs.
func TestRecreatingAPublishedAgentAllowsItsRegistryFirst(t *testing.T) {
	setup := func(t *testing.T) *rig {
		r := newRig(t)
		data := r.published()
		r.agent.Slack = false
		kit, err := ReadKit(registry.Registry{Data: data}, "ada")
		if err != nil {
			t.Fatal(err)
		}
		services, err := secret.Services("ada", []string{"venice"}, kit.Base)
		if err != nil {
			t.Fatal(err)
		}
		r.opts.Secrets.Services = services
		r.opts.Kit = kit
		return r
	}
	r := setup(t)
	r.run()
	prefix := path.Dir(publishedRef) + "/"
	r.order(`sbx settings set ["docker.io/","`+prefix+`"]`, "sbx env plan", "docker pull "+publishedRef, "sbx rm")
	if !strings.Contains(r.out.String(), "Allowed sbx to read kits from "+prefix) {
		t.Fatalf("output:\n%s", r.out.String())
	}

	r = setup(t)
	r.write("sources", "null\n")
	if err := r.run(); failureClass(err) != "kit_source_not_allowed" || strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("err %v\n%s", err, r.calls())
	}
}

func TestCreateAPublishedAgent(t *testing.T) {
	r := newRig(t)
	data := r.published()
	r.write("inventory.json", `[]`)
	r.write("active", "inactive\n")
	r.write("mainpid", "0\n")
	r.agent.Slack = false
	kit, err := ReadKit(registry.Registry{Data: data}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if kit.Reference != publishedRef || kit.Hermes != testHermes || kit.Slack != nil || kit.Setup != nil {
		t.Fatalf("kit %+v", kit)
	}
	services, err := secret.Services("ada", []string{"venice"}, kit.Base)
	if err != nil {
		t.Fatal(err)
	}
	r.opts.Secrets.Services = services
	r.opts.Kit, r.opts.Create = kit, true
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	// A creation, resumed or not, has no plan: sbx accepts the registry before it reads the kit.
	r.order(`sbx settings set ["docker.io/","`+path.Dir(publishedRef)+`/"]`, "sbx env create")
	if !strings.Contains(r.out.String(), "Ada creation: Getting the published kit, adding Ada's settings and creating the new VM.") {
		t.Fatalf("output:\n%s", r.out.String())
	}
	// A creation has no old VM to protect: no plan, pull or prebuild before sbx env create,
	// which pulls and builds the kits itself (troupe create already pulled the kit to read it).
	r.order("created with 20g", "job fresh", "systemctl start hermes-gateway@ada.service")
	for _, early := range []string{"sbx env plan", "docker pull", "docker build"} {
		if strings.Contains(r.calls(), early) {
			t.Fatalf("a creation ran %s before sbx env create:\n%s", early, r.calls())
		}
	}
}

func TestAPublishedKitsRecordIsPartOfItsDeclarations(t *testing.T) {
	r := newRig(t)
	data := r.published()
	first, err := ReadKit(registry.Registry{Data: data}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(registry.KitFolder(data, publishedRef), registry.PublishedFile)
	text, _ := os.ReadFile(record)
	os.WriteFile(record, []byte(strings.Replace(string(text), `"version":"1.2.0"`, `"version":"1.2.1"`, 1)), 0o644)
	second, err := ReadKit(registry.Registry{Data: data}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if first.Declarations == second.Declarations {
		t.Fatal("the published kit's record is not declared")
	}
	os.WriteFile(record, []byte(strings.Replace(string(text), testHermes, "nope", 1)), 0o644)
	if _, err := ReadKit(registry.Registry{Data: data}, "ada"); err == nil {
		t.Fatal("read a published kit with no Hermes version")
	}
}

func TestSelfTestShowsTheReplyEscaped(t *testing.T) {
	r := newRig(t)
	r.write("answer-selftest", `{"status": "passed", "reply": "I am \u001b[31mready\n\u202e now, café"}`)
	if err := r.agent.SelfTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := r.out.String()
	if !strings.Contains(out, `Ada answered: I am \u001b[31mready \u202e now, caf\u00e9`) || strings.ContainsAny(out, "\x1b\u202eé") {
		t.Fatalf("output %q", out)
	}
	// A Slack agent is pointed to Slack, where it is checked end to end; another to troupe chat.
	if !strings.Contains(out, "Say hi in Slack: its manager sends @Ada a DM; it should answer there (in this terminal: troupe chat ada)") {
		t.Fatalf("output %q", out)
	}
	r.out.Reset()
	r.agent.Slack = false
	if err := r.agent.SelfTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := r.out.String(); !strings.Contains(out, "Talk to it: troupe chat ada") || strings.Contains(out, "Slack") {
		t.Fatalf("output %q", out)
	}
}

func TestSelfTestFailureNamesTheCause(t *testing.T) {
	for answer, want := range map[string]string{
		`{"status": "failed", "class": "key_rejected"}`: "key_rejected; the provider refused the agent's key; set it again with troupe secret set ada SERVICE",
		`{"status": "failed", "class": "\u001b[2Jx"}`:   "unknown; the turn failed",
		`not json`: "selftest_failed; the turn failed",
	} {
		t.Run(want[:10], func(t *testing.T) {
			r := newRig(t)
			r.write("answer-selftest", answer)
			err := r.agent.SelfTest(context.Background())
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "The agent is created") {
				t.Fatalf("err %v", err)
			}
		})
	}
}

// deleteRig is a published ada with every record troupe keeps of an agent, beside grace.
func deleteRig(t *testing.T) (*rig, string, []string) {
	r := newRig(t)
	data := r.published()
	r.write("image-ada", "sandbox-kit-assembled:aaa")
	r.write("image-grace", "sandbox-kit-assembled:bbb")
	home := filepath.Join(r.dir, "home")
	records := []string{
		filepath.Join(home, ".local/state/credentials/ada/venice.cred"),
		filepath.Join(r.agent.State, "ada/reseed/completed/x/ready.json"),
		filepath.Join(r.dir, "ada-updates/ada-state.zip"),
		filepath.Join(r.dir, "live-apply/ada/plan.json"),
		filepath.Join(home, ".local/state/troupe/ada/create.log"),
		filepath.Join(r.dir, "pairs/ada.json"),
	}
	for _, path := range records {
		rel, _ := filepath.Rel(r.dir, path)
		r.write(rel, "x")
	}
	r.write("pairs/ada.json", `{"phase":"complete"}`)
	r.write("pairs/grace.json", "x")
	r.write("live-apply/grace/plan.json", "x")
	r.write("templates.json", `[{"repository":"docker.io/library/sbx-kit-src","tag":"ada-0123456789ab"},
 {"repository":"docker.io/library/sbx-kit-src","tag":"ada-2-0123456789ab"},
 {"repository":"docker.io/library/sbx-kit-src","tag":"grace-0123456789ab"},
 {"repository":"docker.io/library/other","tag":"ada-0123456789ab"}]`)
	return r, filepath.Join(data, "agents/ada"), records
}

func TestDeleteRemovesTheAgentAndEveryRecordOfIt(t *testing.T) {
	r, folder, records := deleteRig(t)
	if err := r.agent.Delete(context.Background(), folder); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	r.order("systemctl disable --now hermes-gateway@ada.service", "sbx stop", "sbx rm", "sbx template template rm --force sandbox-kit-assembled:aaa",
		"sbx template template rm --force docker.io/library/sbx-kit-src:ada-0123456789ab")
	// Only ada's own kit sources: not ada-2's, grace's, or another repository's.
	if n := strings.Count(r.calls(), "template rm"); n != 2 {
		t.Fatalf("%d template removals:\n%s", n, r.calls())
	}
	for _, path := range append(records, folder, filepath.Join(r.agent.State, "ada")) {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s remains", path)
		}
	}
	// Other agents' records and the kept published kit stay.
	for _, path := range []string{"pairs/grace.json", "live-apply/grace/plan.json", "data/kits"} {
		if _, err := os.Stat(filepath.Join(r.dir, path)); err != nil {
			t.Fatalf("%s was removed", path)
		}
	}
	if !strings.Contains(r.out.String(), "revoke the keys ada used at their providers") {
		t.Fatalf("output %s", r.out.String())
	}
}

func TestDeleteRefusesWhileACopyIsInProgress(t *testing.T) {
	r, folder, records := deleteRig(t)
	r.write("pairs/ada.json", `{"phase":"copying"}`)
	if err := r.agent.Delete(context.Background(), folder); err == nil || !strings.Contains(err.Error(), "a_copy_is_in_progress") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatal("removed the VM")
	}
	for _, path := range append(records, folder) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed", path)
		}
	}
}

func TestDeleteKeepsAnImageAnotherSandboxUses(t *testing.T) {
	r, folder, _ := deleteRig(t)
	r.write("image-grace", "sandbox-kit-assembled:aaa")
	if err := r.agent.Delete(context.Background(), folder); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.calls(), "sandbox-kit-assembled") {
		t.Fatalf("removed a shared image:\n%s", r.calls())
	}
}

func TestDeleteKeepsALocalAgentsFolderAndTheBaseImage(t *testing.T) {
	r, _, _ := deleteRig(t)
	r.write("image-ada", "ghcr.io/example/coder@sha256:"+strings.Repeat("a", 64))
	if err := r.agent.Delete(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.calls(), "rm --force ghcr.io") {
		t.Fatalf("removed an image that is not the VM's own:\n%s", r.calls())
	}
	if _, err := os.Stat(filepath.Join(r.dir, "kits/agents/ada/sbxenv.yaml")); err != nil {
		t.Fatal("removed the local agent's kit folder")
	}
}

func TestAnInterruptedDeleteFinishesWhenRunAgain(t *testing.T) {
	r, folder, records := deleteRig(t)
	r.write("fail-rm", "")
	if err := r.agent.Delete(context.Background(), folder); err == nil || !strings.Contains(err.Error(), "sandbox_remove_failed") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(records[0]); err != nil {
		t.Fatal("keys removed before the VM")
	}
	os.Remove(filepath.Join(r.dir, "fail-rm"))
	if err := r.agent.Delete(context.Background(), folder); err != nil {
		t.Fatal(err)
	}
	// With no sandbox left, a further run only clears what remains.
	if err := r.agent.Delete(context.Background(), folder); err != nil {
		t.Fatal(err)
	}
}

func TestChatAttachesHermesToTheTerminalOfARunningAgent(t *testing.T) {
	r := newRig(t)
	if err := r.agent.Chat(context.Background(), os.Stdin, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if !strings.Contains(r.calls(), "sbx exec -it -w /home/agent/workspace ada /home/agent/.local/bin/hermes --tui") {
		t.Fatalf("calls:\n%s", r.calls())
	}
}

func TestChatRefusesAStoppedAgent(t *testing.T) {
	for name, prepare := range map[string]func(r *rig){
		"vm stopped":      func(r *rig) { r.write("inventory.json", `[{"name":"ada","id":"old","status":"stopped"}]`) },
		"gateway stopped": func(r *rig) { r.write("active", "inactive\n") },
		"no vm":           func(r *rig) { r.write("inventory.json", `[]`) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			prepare(r)
			err := r.agent.Chat(context.Background(), os.Stdin, os.Stdout, os.Stderr)
			if err == nil || !strings.Contains(err.Error(), "start it first: troupe start ada") {
				t.Fatalf("err %v", err)
			}
			if strings.Contains(r.calls(), "exec -it") {
				t.Fatal("opened a chat that would boot the VM behind troupe")
			}
		})
	}
}

// slackPublished gives the rig's published ada Slack: its kit's manifest and Slack services, its
// .env home and allowlist, and its own slack.env with no workspace yet and no boundary channels.
func (r *rig) slackPublished() string {
	data := r.published()
	kit := registry.KitFolder(data, publishedRef)
	rel := func(path string) string { p, _ := filepath.Rel(r.dir, path); return p }
	r.write(rel(filepath.Join(kit, "slack-app-manifest.json")), `{"display_information":{"name":"AGENT"},"features":{"bot_user":{"display_name":"AGENT"}},"oauth_config":{"scopes":{"bot":["chat:write","im:history"]}}}`)
	r.write(rel(filepath.Join(kit, "services.json")), `{"services":[
 {"name":"venice","prompt":"Venice API key","required":true,"delivery":"native","domains":["api.venice.ai"]},
 {"name":"slack-bot","prompt":"Slack bot token","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-bot"},
 {"name":"slack-app","prompt":"Slack app token","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_APP_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-app"}]}`)
	r.write("data/agents/ada/files/home/.hermes/.env", "SLACK_MANAGER=U0EEEEEEEE\nSLACK_HOME_CHANNEL=U0EEEEEEEE\nSLACK_ALLOWED_USERS=U0EEEEEEEE\n")
	r.write("data/agents/ada/slack.env", "SLACK_WORKSPACE=\nSLACK_BOUNDARY_PUBLIC=\nSLACK_BOUNDARY_PRIVATE=\n")
	return data
}

func TestAPublishedAgentsFirstSlackSetupPinsItsWorkspace(t *testing.T) {
	r := newRig(t)
	data := r.slackPublished()
	r.write("inventory.json", `[]`)
	r.write("active", "inactive\n")
	r.write("mainpid", "0\n")
	r.write("answer-slack-setup", `{"status": "passed", "team": "T0LEARNED"}`)
	kit, err := ReadKit(registry.Registry{Data: data}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if kit.Slack == nil || kit.Slack.Team != "" || kit.Slack.File != filepath.Join(data, "agents/ada/slack.env") {
		t.Fatalf("slack %+v", kit.Slack)
	}
	// No boundary channel is set, so none is sent: an empty one would pass vacuously.
	var request struct {
		Home     map[string]string `json:"home"`
		Boundary map[string]string `json:"boundary"`
	}
	json.Unmarshal(kit.slackRequest(map[string]string{}), &request)
	if len(request.Boundary) != 0 || request.Home["scope_id"] != "" {
		t.Fatalf("request %+v", request)
	}
	services, err := secret.Services("ada", []string{"venice"}, kit.Base)
	if err != nil {
		t.Fatal(err)
	}
	r.opts.Secrets.Services = services
	r.ciphertexts(r.opts.Secrets)
	r.opts.Kit, r.opts.Create = kit, true
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if got, _ := os.ReadFile(filepath.Join(data, "agents/ada/slack.env")); !strings.Contains(string(got), "SLACK_WORKSPACE=T0LEARNED\n") {
		t.Fatalf("slack.env %q", got)
	}
	if !strings.Contains(r.out.String(), "Pinned to the Slack workspace T0LEARNED") {
		t.Fatalf("output %s", r.out.String())
	}
	// The machine's boundary settings apply as they are when the kit is read, even ones set after
	// the agent was created.
	bounded, err := ReadKit(registry.Registry{Data: data, Boundary: [2]string{"C0PUBLIC1", ""}}, "ada")
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(bounded.slackRequest(map[string]string{}), &request)
	if len(request.Boundary) != 1 || request.Boundary["public"] != "C0PUBLIC1" {
		t.Fatalf("boundary %+v", request.Boundary)
	}
	// Read again, the agent is now held to that workspace.
	again, err := ReadKit(registry.Registry{Data: data}, "ada")
	if err != nil || again.Slack.Team != "T0LEARNED" {
		t.Fatalf("again %+v %v", again.Slack, err)
	}
}

func TestAPublishedAgentsSlackSettingsAreFormatChecked(t *testing.T) {
	for name, text := range map[string]string{
		"a malformed workspace": "SLACK_WORKSPACE=nope\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			data := r.slackPublished()
			r.write("data/agents/ada/slack.env", text)
			if _, err := ReadKit(registry.Registry{Data: data}, "ada"); err == nil {
				t.Fatal("read")
			}
		})
	}
	// A local agent's base kit must still name its workspace and both boundaries.
	r := newRig(t)
	r.write("kits/kits/coder/slack.env", "SLACK_WORKSPACE=\nSLACK_BOUNDARY_PUBLIC=C0BBBBBBBB\nSLACK_BOUNDARY_PRIVATE=C0CCCCCCCC\n")
	if _, err := ReadKit(registry.Registry{Root: filepath.Join(r.dir, "kits")}, "ada"); err == nil {
		t.Fatal("a local kit without its workspace was read")
	}
}

// imageRig is ada on sandbox-kit-assembled:old beside grace on her own image; the reset's new
// VM runs sandbox-kit-assembled:new (the fake sbx env create).
func imageRig(t *testing.T) *rig {
	r := newRig(t)
	r.write("image-ada", "sandbox-kit-assembled:old")
	r.write("image-grace", "sandbox-kit-assembled:grace")
	return r
}

// A reset removes the replaced VM's image once the new VM is ready, never before.
func TestAResetRemovesTheReplacedVMsImage(t *testing.T) {
	r := imageRig(t)
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	r.order("sbx rm", "created with 20g", "job ready --platform slack", "sbx template template rm --force sandbox-kit-assembled:old")
	if !strings.Contains(r.out.String(), "Ada reset: Removed the replaced VM's image sandbox-kit-assembled:old.") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

// The image stays when the new VM runs it too, when another sandbox runs it, and when sbx
// refuses; none of these fails the reset.
func TestAResetKeepsAnImageStillInUse(t *testing.T) {
	for name, set := range map[string]func(*rig){
		"the new VM runs it":        func(r *rig) { r.write("image-ada", "sandbox-kit-assembled:new") },
		"another sandbox runs it":   func(r *rig) { r.write("image-grace", "sandbox-kit-assembled:old") },
		"another sandbox's unknown": func(r *rig) { os.Remove(filepath.Join(r.dir, "image-grace")) },
		"another sandbox runs it, named in full": func(r *rig) {
			r.write("image-grace", "docker.io/library/sandbox-kit-assembled:old")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := imageRig(t)
			set(r)
			if err := r.run(); err != nil {
				t.Fatalf("%v\n%s", err, r.calls())
			}
			if strings.Contains(r.calls(), "template rm") {
				t.Fatalf("removed an image in use:\n%s", r.calls())
			}
		})
	}
	r := imageRig(t)
	r.write("fail-template-rm", "")
	if err := r.run(); err != nil || !strings.Contains(r.out.String(), "The VM's image sandbox-kit-assembled:old stays") {
		t.Fatalf("%v\n%s", err, r.out.String())
	}
}

// A reset resumed after the old VM is gone still removes its image: the removal recorded it.
func TestAResumedResetRemovesTheImageItRecorded(t *testing.T) {
	r := imageRig(t)
	r.write("fail-create", "")
	if err := r.run(); err == nil {
		t.Fatal("the planted create failure passed")
	}
	if strings.Contains(r.calls(), "template rm") {
		t.Fatalf("removed the image before a new VM ran:\n%s", r.calls())
	}
	os.Remove(filepath.Join(r.dir, "fail-create"))
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if !strings.Contains(r.calls(), "sbx template template rm --force sandbox-kit-assembled:old") {
		t.Fatalf("calls:\n%s", r.calls())
	}
}

// A run that stopped after its last receipt, before it could remove the replaced image, has it
// removed when the agent's next reset files that run away.
func TestTheNextRunRemovesAnImageAnEarlierRunLeft(t *testing.T) {
	r := imageRig(t)
	r.write("image-ada", "sandbox-kit-assembled:mid")
	rel := func(path string) string { p, _ := filepath.Rel(r.dir, path); return p }
	r.write(rel(r.current("removal-authorized.json")), `{"mode":"recreate","old_id":"older","old_image":"sandbox-kit-assembled:old"}`)
	r.write(rel(r.current("finalized.json")), `{"mode":"recreate","new_id":"old","finalized_at":"2026-01-01T00:00:00Z"}`)
	if err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	r.order("sbx template template rm --force sandbox-kit-assembled:old", "sbx rm", "sbx template template rm --force sandbox-kit-assembled:mid")
}
