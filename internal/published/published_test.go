package published

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/secret"
)

const (
	repository = "ghcr.io/example/hermes"
	reference  = repository + ":1.2.0"
	hermes     = "0.22.2"
)

// fakeDocker answers pull, image inspect, buildx imagetools inspect, login, create, cp and rm
// like Docker, recording each call. Files in its folder change its answers: inspect (the
// version labels' line), label (the troupe label), labels (every label, as the registry gives
// them), fail-ARG1, refuse (401), missing (no such kit), missing-version, no-login, and
// troupe.tar (what cp streams).
const fakeDocker = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
[ -f "$d/refuse" ] && { echo "Error response from daemon: Head \"https://ghcr.io/v2/x\": unauthorized" >&2; exit 1; }
# What ghcr.io answered a login that may read there, for a kit it does not hold (2026-11-18).
for last; do :; done
[ -f "$d/missing" ] && case "$1" in
  pull) echo "Error response from daemon: failed to resolve reference \"$last\": $last: not found" >&2; exit 1 ;;
  buildx) echo "ERROR: $4: not found" >&2; exit 1 ;;
esac
[ -f "$d/missing-version" ] && [ "$1" = pull ] && case "$last" in *:latest) ;; *) echo "Error response from daemon: failed to resolve reference \"$last\": $last: not found" >&2; exit 1 ;; esac
# Other failures end in "not found" too: an image with nothing for this machine's platform.
[ -f "$d/other-not-found" ] && { echo "Error response from daemon: no match for platform in manifest: not found" >&2; exit 1; }
[ -f "$d/fail-$1" ] && { echo "failed" >&2; exit 1; }
case "$1" in
  pull) exit 0 ;;
  image) case "$*" in *org.host.troupe.kit*) cat "$d/label" ;; *) cat "$d/inspect" ;; esac ;;
  buildx) cat "$d/labels" ;;
  login) [ -f "$d/no-login" ] && { echo "error: cannot perform an interactive login from a non-TTY device" >&2; exit 1; }; echo "Login Succeeded" ;;
  create) echo 0123456789abcdef0123456789abcdef ;;
  cp) cat "$d/troupe.tar" ;;
  rm) echo "$2" ;;
  manifest) [ -n "$DOCKER_AUTH_CONFIG" ] && exit 0
            [ -f "$d/private" ] && { [ -z "$DOCKER_CONFIG" ] || [ -n "$(ls -A "$DOCKER_CONFIG")" ]; exit; }; exit 0 ;;
esac
`

type fake struct {
	t    *testing.T
	bin  string
	data string
	log  bytes.Buffer
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, bin: t.TempDir(), data: t.TempDir()}
	if err := os.WriteFile(filepath.Join(f.bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.bin+":"+os.Getenv("PATH"))
	f.set("inspect", `1.2.0 `+hermes)
	f.set("label", label)
	f.archive(complete())
	return f
}

// with is files plus one more.
func with(files map[string]string, name, text string) map[string]string {
	files[name] = text
	return files
}

// label is a kit's troupe label: sizes, one setting and one service.
const label = `{"format":1,"defaults":{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":10},` +
	`"settings":[{"env":"TEAM","prompt":"Team","pattern":"[a-z]+"}],` +
	`"services":[{"name":"venice","prompt":"Venice API key","required":true,"delivery":"native","domains":["api.venice.ai"]}]}`

// complete is a kit's whole /usr/share/troupe/: what Extract requires, a template and a skill.
func complete() map[string]string {
	return map[string]string{
		"troupe/SOUL.md":                 "# {agent}\n",
		"troupe/seeds/config.yaml":       "model: x\n",
		"troupe/seeds/skills/a/SKILL.md": "# a\n",
	}
}

func (f *fake) set(name, text string) {
	if err := os.WriteFile(filepath.Join(f.bin, name), []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// archive writes what docker cp streams: the folders implied, then each file.
func (f *fake) archive(files map[string]string, extra ...*tar.Header) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	w.WriteHeader(&tar.Header{Name: "troupe/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, text := range files {
		w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(text))})
		w.Write([]byte(text))
	}
	for _, h := range extra {
		w.WriteHeader(h)
	}
	w.Close()
	f.set("troupe.tar", buf.String())
}

func (f *fake) calls() string {
	data, _ := os.ReadFile(filepath.Join(f.bin, "calls"))
	return string(data)
}

func (f *fake) docker() Docker { return Docker{Log: &f.log} }

func TestResolvePinsTheLatestVersion(t *testing.T) {
	f := newFake(t)
	img, err := f.docker().Resolve(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Image{Reference: reference, Type: "hermes", Version: "1.2.0", Hermes: hermes}); img != want {
		t.Fatalf("image %+v, want %+v", img, want)
	}
	// latest names the version; the agent is pinned to that version's own tag, pulled too.
	if !strings.HasPrefix(f.calls(), "pull --quiet "+repository+":latest\nimage inspect "+repository+":latest --format") ||
		!strings.Contains(f.calls(), "\npull --quiet "+reference+"\n") {
		t.Fatalf("calls:\n%s", f.calls())
	}
}

func TestResolveRefusesAnImageItCannotPin(t *testing.T) {
	cases := map[string]string{
		"not a semantic version": `latest ` + hermes,
		"a leading zero":         `01.2.0 ` + hermes,
		"no version label":       ` ` + hermes,
		"no Hermes version":      `1.2.0 abc`,
		"a third field":          `1.2.0 ` + hermes + ` x`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.set("inspect", line)
			if _, err := f.docker().Resolve(context.Background(), repository); err == nil {
				t.Fatal("resolved")
			}
		})
	}
}

func TestResolveTellsARefusedLoginApart(t *testing.T) {
	f := newFake(t)
	f.set("refuse", "")
	if _, err := f.docker().Resolve(context.Background(), repository); !errors.Is(err, ErrAccess) {
		t.Fatalf("err %v, want ErrAccess", err)
	}
	f = newFake(t)
	f.set("fail-pull", "")
	if _, err := f.docker().Resolve(context.Background(), repository); err == nil || errors.Is(err, ErrAccess) {
		t.Fatalf("err %v, want another failure", err)
	}
}

func image() Image {
	return Image{Reference: reference, Type: "hermes", Version: "1.2.0", Hermes: hermes}
}

func TestExtractKeepsTheKitsFilesLikeALocalKit(t *testing.T) {
	f := newFake(t)
	folder, err := f.docker().Extract(context.Background(), f.data, image())
	if err != nil {
		t.Fatal(err)
	}
	if folder != filepath.Join(f.data, "kits", "hermes-1.2.0") {
		t.Fatalf("folder %s", folder)
	}
	for _, name := range []string{"SOUL.md", "files/home/.hermes/config.yaml", "files/home/.hermes/skills/a/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The label, kept as the files the rest of troupe reads in a kit folder.
	sizes, err := ReadDefaults(folder)
	if err != nil || sizes != (Defaults{CPUs: 4, MemoryGiB: 6, RootGiB: 50, DockerGiB: 10}) {
		t.Fatalf("sizes %+v, %v", sizes, err)
	}
	settings, err := ReadSettings(folder)
	if err != nil || len(settings) != 1 || settings[0].Env != "TEAM" {
		t.Fatalf("settings %+v, %v", settings, err)
	}
	services, err := secret.KitServices(folder)
	if err != nil || len(services) != 1 || services[0].Name != "venice" || !services[0].Required {
		t.Fatalf("services %+v, %v", services, err)
	}
	published, err := registry.ReadPublished(folder)
	if err != nil || published != (registry.Published{Reference: reference, Type: "hermes", Version: "1.2.0", Hermes: hermes, Format: 1}) {
		t.Fatalf("record %+v, %v", published, err)
	}
	if !strings.Contains(f.calls(), "rm 0123456789abcdef0123456789abcdef") {
		t.Fatalf("the container was not removed:\n%s", f.calls())
	}
	// A kit already kept is reused without Docker.
	os.Remove(filepath.Join(f.bin, "calls"))
	if again, err := f.docker().Extract(context.Background(), f.data, image()); err != nil || again != folder || f.calls() != "" {
		t.Fatalf("again %s, %v, calls %q", again, err, f.calls())
	}
	// The same type and version from another registry is not that kit.
	other := image()
	other.Reference = "ghcr.io/other/hermes:1.2.0"
	if _, err := f.docker().Extract(context.Background(), f.data, other); err == nil {
		t.Fatal("reused another registry's kit")
	}
}

func TestExtractRefusesAnUnsafeOrIncompleteKit(t *testing.T) {
	big := strings.Repeat("x", fileLimit+1)
	cases := map[string]func(f *fake){
		"a link": func(f *fake) {
			f.archive(complete(), &tar.Header{Name: "troupe/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
		},
		"a path outside": func(f *fake) { f.archive(with(complete(), "troupe/../../x", "x")) },
		"another folder": func(f *fake) { f.archive(with(complete(), "etc/passwd", "x")) },
		"too large":      func(f *fake) { f.archive(with(complete(), "troupe/x", big)) },
		"no seeds":       func(f *fake) { f.archive(map[string]string{"troupe/SOUL.md": "x"}) },
		"a failed copy":  func(f *fake) { f.set("fail-cp", "") },
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			prepare(f)
			if _, err := f.docker().Extract(context.Background(), f.data, image()); err == nil {
				t.Fatal("extracted")
			}
			if registry.IsPublished(registry.KitFolder(f.data, reference)) {
				t.Fatal("a refused kit was kept")
			}
			if entries, _ := os.ReadDir(filepath.Join(f.data, "kits")); len(entries) != 0 {
				t.Fatalf("left behind: %v", entries)
			}
			if !strings.Contains(f.calls(), "rm 0123456789abcdef0123456789abcdef") {
				t.Fatalf("the container was not removed:\n%s", f.calls())
			}
		})
	}
}

func TestWriteMakesTheAgentsFolder(t *testing.T) {
	data := t.TempDir()
	a := Agent{Name: "jane", Image: image(), Sizes: Defaults{CPUs: 4, MemoryGiB: 6, RootGiB: 50, DockerGiB: 10},
		Soul: []byte("# Jane\n"), Created: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
		Natives: []Native{{Name: "venice", Command: "systemd-creds decrypt /x -", Domains: []string{"api.venice.ai"}}}}
	folder, err := Write(data, a)
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(folder, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	env := read("sbxenv.yaml")
	for _, want := range []string{"name: jane\n", "agent: hermes\n", `  - "` + reference + `"` + "\n  - \".\"\n",
		"  cpus: 4\n  memory: 6g\n", "  venice:\n    apiKey:\n      domains:\n        - api.venice.ai\n",
		// Five minutes, never on-demand: that ran the TPM for every request (sbx.KeyRefresh).
		"  venice:\n    command: systemd-creds decrypt /x -\n    refresh: 5m\n"} {
		if !strings.Contains(env, want) {
			t.Fatalf("sbxenv.yaml lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, ":latest") {
		t.Fatal("the agent follows latest")
	}
	if got := read("storage.env"); got != "DOCKER_SANDBOXES_ROOT_SIZE=50g\nDOCKER_SANDBOXES_DOCKER_SIZE=10g\n" {
		t.Fatalf("storage.env %q", got)
	}
	if !strings.Contains(read("jane.yaml"), "kind: mixin") || !strings.Contains(read("jane.yaml"), `requires: ["hermes"]`) {
		t.Fatal("jane.yaml is not a mixin on hermes")
	}
	if read("files/home/.hermes/SOUL.md") != "# Jane\n" || !strings.Contains(read("source.json"), `"version": "1.2.0"`) {
		t.Fatal("SOUL or source.json")
	}
	if _, err := Write(data, a); err == nil {
		t.Fatal("wrote over an existing agent")
	}
	// No Docker disk and no SOUL: neither is written.
	a.Name, a.Sizes.DockerGiB, a.Soul = "joe", 0, nil
	folder, err = Write(data, a)
	if err != nil {
		t.Fatal(err)
	}
	if got := read("storage.env"); got != "DOCKER_SANDBOXES_ROOT_SIZE=50g\n" {
		t.Fatalf("storage.env %q", got)
	}
	if _, err := os.Stat(filepath.Join(folder, "files/home/.hermes/SOUL.md")); err == nil {
		t.Fatal("wrote an empty SOUL")
	}
}

func TestReadDefaultsRefusesMalformedSizes(t *testing.T) {
	for name, text := range map[string]string{
		"no cpus":       `{"memory_gib":6,"root_gib":50,"docker_gib":0}`,
		"unknown field": `{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":0,"gpus":1}`,
		"negative":      `{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "defaults.json"), []byte(text), 0o644)
			if _, err := ReadDefaults(dir); err == nil {
				t.Fatal("read")
			}
		})
	}
}

func TestReadSettingsReadsTheKitsSettingsInOrder(t *testing.T) {
	dir := t.TempDir()
	if settings, err := ReadSettings(dir); err != nil || settings != nil {
		t.Fatalf("a kit without settings.json: %v %v", settings, err)
	}
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"settings":[
 {"env":"TEAM_NAME","prompt":"Team name","help":"Change it later in chat.","pattern":"[a-z]+"},
 {"env":"REGION","prompt":"Region","pattern":"eu|us"}]}`), 0o644)
	settings, err := ReadSettings(dir)
	if err != nil || len(settings) != 2 || settings[0].Env != "TEAM_NAME" || settings[1].Pattern != "eu|us" {
		t.Fatalf("%+v %v", settings, err)
	}
}

func TestReadSettingsRefusesMalformedSettings(t *testing.T) {
	for name, entry := range map[string]string{
		"unknown field":   `{"env":"A","prompt":"A","pattern":"x","secret":true}`,
		"lowercase env":   `{"env":"team","prompt":"A","pattern":"x"}`,
		"a Slack setting": `{"env":"SLACK_HOME_CHANNEL","prompt":"A","pattern":"x"}`,
		"no prompt":       `{"env":"A","prompt":" ","pattern":"x"}`,
		"no pattern":      `{"env":"A","prompt":"A","pattern":""}`,
		"a bad pattern":   `{"env":"A","prompt":"A","pattern":"("}`,
		"a control":       `{"env":"A","prompt":"A\u001b[2J","pattern":"x"}`,
		"repeated":        `{"env":"A","prompt":"A","pattern":"x"},{"env":"A","prompt":"B","pattern":"y"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"settings":[`+entry+`]}`), 0o644)
			if _, err := ReadSettings(dir); err == nil {
				t.Fatal("read")
			}
		})
	}
}

func TestResizeRewritesOnlyTheSizes(t *testing.T) {
	data := t.TempDir()
	folder, err := Write(data, Agent{Name: "jane", Image: image(), Sizes: Defaults{CPUs: 4, MemoryGiB: 6, RootGiB: 50, DockerGiB: 10},
		Created: time.Now(), Natives: []Native{{Name: "venice", Command: "c", Domains: []string{"api.venice.ai"}}}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml"))
	restore, err := Resize(folder, 2, 8, 60, 30)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml"))
	want := strings.Replace(strings.Replace(string(before), "  cpus: 4\n", "  cpus: 2\n", 1), "  memory: 6g\n", "  memory: 8g\n", 1)
	if string(after) != want {
		t.Fatalf("sbxenv.yaml:\n%s", after)
	}
	if storage, _ := os.ReadFile(filepath.Join(folder, "storage.env")); string(storage) != "DOCKER_SANDBOXES_ROOT_SIZE=60g\nDOCKER_SANDBOXES_DOCKER_SIZE=30g\n" {
		t.Fatalf("storage.env %q", storage)
	}
	// A file not as Write wrote it is refused, and nothing changes.
	os.WriteFile(filepath.Join(folder, "storage.env"), []byte("DOCKER_SANDBOXES_ROOT_SIZE=60g\nDOCKER_SANDBOXES_ROOT_SIZE=70g\n"), 0o600)
	if _, err := Resize(folder, 3, 9, 80, 10); err == nil {
		t.Fatal("rewrote an unexpected file")
	}
	if again, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml")); string(again) != want {
		t.Fatal("changed sbxenv.yaml after a refusal")
	}
	// restore puts back both files as they were before the resize.
	restore()
	if back, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml")); string(back) != string(before) {
		t.Fatalf("sbxenv.yaml not restored:\n%s", back)
	}
	if storage, _ := os.ReadFile(filepath.Join(folder, "storage.env")); string(storage) != "DOCKER_SANDBOXES_ROOT_SIZE=50g\nDOCKER_SANDBOXES_DOCKER_SIZE=10g\n" {
		t.Fatalf("storage.env not restored: %q", storage)
	}
}

func TestRepinRewritesOnlyThePin(t *testing.T) {
	data := t.TempDir()
	old := image()
	folder, err := Write(data, Agent{Name: "jane", DisplayName: "Jane", Image: old, Sizes: Defaults{CPUs: 4, MemoryGiB: 6, RootGiB: 50, DockerGiB: 10},
		Created: time.Now(), Services: []string{"slack-bot"}})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml"))
	source, _ := os.ReadFile(filepath.Join(folder, "source.json"))
	next := Image{Type: old.Type, Version: "9.9.9", Hermes: old.Hermes, Reference: strings.TrimSuffix(old.Reference, old.Version) + "9.9.9"}
	restore, err := Repin(folder, old.Reference, next)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml"))
	if string(after) != strings.Replace(string(env), strconv.Quote(old.Reference), strconv.Quote(next.Reference), 1) {
		t.Fatalf("sbxenv.yaml:\n%s", after)
	}
	var fields map[string]any
	data2, _ := os.ReadFile(filepath.Join(folder, "source.json"))
	json.Unmarshal(data2, &fields)
	if fields["reference"] != next.Reference || fields["version"] != "9.9.9" || fields["display_name"] != "Jane" ||
		fmt.Sprint(fields["services"]) != "[slack-bot]" {
		t.Fatalf("source.json %v", fields)
	}
	// A pin not as recorded is refused, and nothing changes.
	if _, err := Repin(folder, old.Reference, next); err == nil {
		t.Fatal("repinned from a reference the folder no longer names")
	}
	restore()
	if back, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml")); string(back) != string(env) {
		t.Fatalf("sbxenv.yaml not restored:\n%s", back)
	}
	if back, _ := os.ReadFile(filepath.Join(folder, "source.json")); string(back) != string(source) {
		t.Fatalf("source.json not restored:\n%s", back)
	}
}

func TestVersionsCompareNumerically(t *testing.T) {
	for _, c := range []struct {
		a, b string
		sign int
	}{{"2.0.1", "2.0.2", -1}, {"2.0.10", "2.0.9", 1}, {"2.1.0", "2.1.0", 0}, {"10.0.0", "9.9.9", 1}} {
		got, err := Compare(c.a, c.b)
		if err != nil || (got < 0 && c.sign >= 0) || (got > 0 && c.sign <= 0) || (got == 0 && c.sign != 0) {
			t.Errorf("Compare(%s, %s) = %d, %v", c.a, c.b, got, err)
		}
	}
	if _, err := Compare("2.0", "2.0.1"); err == nil {
		t.Error("compared a version that is not MAJOR.MINOR.PATCH")
	}
	if Major("2.0.1") != "2" || Major("x") != "" {
		t.Error("Major")
	}
}

// A public kit reads without a login: Docker is asked with an empty configuration, never with
// the operator's own login.
func TestPublicReadsWithoutALogin(t *testing.T) {
	f := newFake(t)
	d := Docker{Log: &f.log}
	if !d.Public(context.Background(), reference) {
		t.Fatalf("public kit read as private:\n%s", f.log.String())
	}
	f.set("private", "")
	if d.Public(context.Background(), reference) {
		t.Fatal("private kit read as public")
	}
	// Credentials in the environment never reach the check.
	t.Setenv("DOCKER_AUTH_CONFIG", `{"auths":{"ghcr.io":{"auth":"x"}}}`)
	if d.Public(context.Background(), reference) {
		t.Fatal("DOCKER_AUTH_CONFIG made a private kit public")
	}
	if calls, _ := os.ReadFile(filepath.Join(f.bin, "calls")); !strings.Contains(string(calls), "manifest inspect "+reference) {
		t.Fatalf("calls %s", calls)
	}
}

// A kit's own copies of the files the label owns are never read: the label is the only source.
func TestExtractIgnoresTheImagesCopiesOfTheLabelsFiles(t *testing.T) {
	f := newFake(t)
	files := complete()
	files["troupe/defaults.json"] = `{"cpus":64,"memory_gib":64,"root_gib":64,"docker_gib":64}`
	files["troupe/services.json"] = `{"services":[{"name":"other","prompt":"O","delivery":"native","domains":["evil.example"]}]}`
	files["troupe/settings.json"] = `not json`
	f.archive(files)
	folder, err := f.docker().Extract(context.Background(), f.data, image())
	if err != nil {
		t.Fatal(err)
	}
	if sizes, _ := ReadDefaults(folder); sizes.CPUs != 4 {
		t.Fatalf("sizes %+v", sizes)
	}
	if services, _ := secret.KitServices(folder); len(services) != 1 || services[0].Name != "venice" {
		t.Fatalf("services %+v", services)
	}
}

// A kit troupe cannot read stops before any of its files is copied out, with what to do.
func TestExtractRefusesAKitByItsLabel(t *testing.T) {
	sizes := `"defaults":{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":10}`
	venice := `{"name":"venice","prompt":"V","delivery":"native","domains":["api.venice.ai"]}`
	for name, c := range map[string]struct{ label, want string }{
		"no label":                         {"\n", "too old for this troupe"},
		"a newer format":                   {`{"format":2,"anything":{"at":"all"}}`, "troupe update"},
		"a newer format with other shapes": {`{"format":2,"defaults":"none","services":{"venice":true}}`, "troupe update"},
		"another shape in this format":     {`{"format":1,` + sizes + `,"services":{"venice":true}}`, "another type"},
		"a label padded past the cap":      {label + strings.Repeat(" ", labelLimit) + "x", "larger than"},
		"no format":                        {`{` + sizes + `,"services":[` + venice + `]}`, "no format"},
		"not JSON":                         {`{"format":1`, "not a JSON document"},
		"no services":                      {`{"format":1,` + sizes + `}`, "no services"},
		"malformed sizes":                  {`{"format":1,"defaults":{"cpus":0},"services":[` + venice + `]}`, "malformed defaults"},
		"a bad service":                    {`{"format":1,` + sizes + `,"services":[{"name":"x","prompt":"X","delivery":"other"}]}`, `service "x" is malformed`},
		"a bad setting":                    {`{"format":1,` + sizes + `,"settings":[{"env":"SLACK_X","prompt":"X","pattern":"x"}],"services":[` + venice + `]}`, `setting "SLACK_X"`},
		"too large":                        {`{"format":1,"pad":"` + strings.Repeat("x", labelLimit) + `"}`, "larger than"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.set("label", c.label)
			_, err := f.docker().Extract(context.Background(), f.data, image())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v", err)
			}
			if strings.Contains(f.calls(), "create") || strings.Contains(f.calls(), "cp ") {
				t.Fatalf("the kit's files were read:\n%s", f.calls())
			}
			if entries, _ := os.ReadDir(filepath.Join(f.data, "kits")); len(entries) != 0 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}
}

// A field this troupe does not know is skipped: a kit may add one without a new format.
func TestALabelMayCarryFieldsThisTroupeDoesNotKnow(t *testing.T) {
	k, err := DecodeLabel(strings.Replace(label, `"format":1,`, `"format":1,"later":{"x":1},`, 1))
	if err != nil || k.Defaults.CPUs != 4 || len(k.Services) != 1 {
		t.Fatalf("%+v, %v", k, err)
	}
	k, err = DecodeLabel(strings.Replace(label, `"name":"venice",`, `"name":"venice","later":true,`, 1))
	if err != nil || k.Services[0].Name != "venice" {
		t.Fatalf("%+v, %v", k, err)
	}
}

// The label troupe prints is the label it reads back.
func TestALabelRoundTrips(t *testing.T) {
	k, err := DecodeLabel(label)
	if err != nil {
		t.Fatal(err)
	}
	text, err := k.Encode()
	if err != nil || text != label {
		t.Fatalf("%v\n%s\n%s", err, text, label)
	}
}

// ReadSource reads a kit's source folder strictly, and refuses what create would.
func TestReadSourceChecksAKitBeforeItIsPublished(t *testing.T) {
	kit := func(t *testing.T, files map[string]string) string {
		dir := t.TempDir()
		base := map[string]string{
			"troupe/defaults.json": `{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":10}`,
			"troupe/services.json": `{"services":[{"name":"venice","prompt":"V","required":true,"delivery":"native","domains":["api.venice.ai"]}]}`,
		}
		for name, text := range files {
			base[name] = text
		}
		for name, text := range base {
			if text == "" {
				continue
			}
			os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
			os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
		}
		return dir
	}
	slack := `{"services":[{"name":"slack-bot","prompt":"B","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"p-{agent}"}]}`
	k, err := ReadSource(kit(t, nil))
	if err != nil || k.Format != Format || k.Defaults.CPUs != 4 || len(k.Services) != 1 || len(k.Settings) != 0 {
		t.Fatalf("%+v, %v", k, err)
	}
	if _, err := ReadSource(kit(t, map[string]string{"troupe/services.json": slack, "slack-app-manifest.json": "{}", "templates/env": "A=1\n"})); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"a misspelled field": {map[string]string{"troupe/services.json": `{"services":[{"name":"venice","prompt":"V","requierd":true,"delivery":"native","domains":["api.venice.ai"]}]}`}, "requierd"},
		"no sizes":           {map[string]string{"troupe/defaults.json": ""}, "defaults.json"},
		"a setting the template sets": {map[string]string{"templates/env": "TEAM=x\n",
			"troupe/settings.json": `{"settings":[{"env":"TEAM","prompt":"T","pattern":"x"}]}`}, "already sets TEAM"},
		"Slack without the manifest": {map[string]string{"troupe/services.json": slack, "templates/env": "A=1\n"}, "slack-app-manifest.json"},
		"Slack without the template": {map[string]string{"troupe/services.json": slack, "slack-app-manifest.json": "{}"}, "templates/env"},
		"a manifest without Slack":   {map[string]string{"slack-app-manifest.json": "{}"}, "without a Slack service"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadSource(kit(t, c.files)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v", err)
			}
		})
	}
}

// Describe reads a kit's label and description from the registry's labels, without a pull.
func TestDescribeReadsTheRegistrysLabels(t *testing.T) {
	f := newFake(t)
	labels, _ := json.Marshal(map[string]string{LabelName: label, "org.opencontainers.image.description": "A kit.\x1b[31m\nSecond line."})
	f.set("labels", string(labels))
	k, description, err := f.docker().Describe(context.Background(), repository+":latest")
	if err != nil || k.Defaults.RootGiB != 50 || description != "A kit. [31m Second line." {
		t.Fatalf("%+v, %q, %v", k, description, err)
	}
	if strings.Contains(f.calls(), "pull") || !strings.Contains(f.calls(), "buildx imagetools inspect "+repository+":latest") {
		t.Fatalf("calls:\n%s", f.calls())
	}
	f.set("labels", `{"org.opencontainers.image.version":"1.0.0"}`)
	if _, _, err := f.docker().Describe(context.Background(), repository+":latest"); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("%v", err)
	}
}

// LoggedIn asks Docker itself whether it holds a login the registry accepts.
func TestLoggedInAsksDocker(t *testing.T) {
	f := newFake(t)
	if !f.docker().LoggedIn(context.Background(), "ghcr.io") || !strings.Contains(f.calls(), "login ghcr.io") {
		t.Fatalf("calls:\n%s", f.calls())
	}
	f.set("no-login", "")
	if f.docker().LoggedIn(context.Background(), "ghcr.io") {
		t.Fatal("a login without one")
	}
}

// A kit folder an earlier troupe kept, from a kit that had no label, still serves the agents on
// it, but is never what a new create or an upgrade starts from: that kit is too old.
func TestExtractRefusesAFolderKeptBeforeTheLabel(t *testing.T) {
	f := newFake(t)
	folder := registry.KitFolder(f.data, reference)
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, registry.PublishedFile),
		[]byte(`{"reference":"`+reference+`","type":"hermes","version":"1.2.0","hermes":"`+hermes+`"}`), 0o644)
	if kept, err := registry.ReadPublished(folder); err != nil || kept.Format != 0 {
		t.Fatalf("an earlier troupe's record must still read: %+v, %v", kept, err)
	}
	_, err := f.docker().Extract(context.Background(), f.data, image())
	if err == nil || !strings.Contains(err.Error(), "too old for this troupe") || f.calls() != "" {
		t.Fatalf("%v, calls %q", err, f.calls())
	}
}

// A registry tells a login that may read there when it holds no such kit: that is said plainly,
// and is never the refusal that asks for a token.
func TestAMissingKitIsSaidPlainly(t *testing.T) {
	f := newFake(t)
	f.set("missing", "")
	_, err := f.docker().Resolve(context.Background(), "ghcr.io/example/codr")
	var noKit NoKitError
	if !errors.As(err, &noKit) || errors.Is(err, ErrAccess) || noKit.Repository != "ghcr.io/example/codr" ||
		err.Error() != "the registry holds no ghcr.io/example/codr kit" {
		t.Fatalf("%v", err)
	}
	if _, _, err := f.docker().Describe(context.Background(), "ghcr.io/example/codr:latest"); err == nil ||
		err.Error() != "the registry holds no such kit: check the type's name" {
		t.Fatalf("%v", err)
	}
	// latest naming a version that is not there is the publisher's fault, not a typo. That
	// version reads like a refusal (403), which a missing reference is never taken for.
	f = newFake(t)
	f.set("inspect", "1.403.0 "+hermes)
	f.set("missing-version", "")
	if _, err := f.docker().Resolve(context.Background(), repository); err == nil || errors.Is(err, ErrAccess) ||
		!strings.Contains(err.Error(), "names version 1.403.0, which the registry does not hold") {
		t.Fatalf("%v", err)
	}
}

// Only "REFERENCE: not found" is a missing kit. Docker ends other failures with "not found" too
// (no image for this platform), and other commands never mean a kit.
func TestOtherNotFoundFailuresAreNotAMissingKit(t *testing.T) {
	f := newFake(t)
	f.set("other-not-found", "")
	_, err := f.docker().Resolve(context.Background(), repository)
	var noKit NoKitError
	if err == nil || errors.As(err, &noKit) || err.Error() != "docker pull failed" {
		t.Fatalf("%v", err)
	}
	if _, _, err := f.docker().Describe(context.Background(), repository+":latest"); err == nil || strings.Contains(err.Error(), "no such kit") {
		t.Fatalf("%v", err)
	}
	for _, c := range []struct {
		args   []string
		stderr string
		want   bool
	}{
		{[]string{"pull", "--quiet", "ghcr.io/x/y:latest"}, `Error response from daemon: failed to resolve reference "ghcr.io/x/y:latest": ghcr.io/x/y:latest: not found`, true},
		{[]string{"buildx", "imagetools", "inspect", "ghcr.io/x/y:latest", "--format", "{{json .}}"}, "ERROR: ghcr.io/x/y:latest: not found\n", true},
		{[]string{"pull", "--quiet", "ghcr.io/x/y:latest"}, "ghcr.io/x/other:latest: not found", false},
		{[]string{"pull", "--quiet", "ghcr.io/x/y:latest"}, "content at https://ghcr.io/v2/x/y/blobs/sha256:0 not found: not found", false},
		{[]string{"image", "inspect", "ghcr.io/x/y:latest"}, "ghcr.io/x/y:latest: not found", false},
		{[]string{"cp", "0123:/usr/share/troupe", "-"}, "0123:/usr/share/troupe: not found", false},
	} {
		if got := missing(c.args, c.stderr); got != c.want {
			t.Errorf("%v %q: %v", c.args, c.stderr, got)
		}
	}
}
