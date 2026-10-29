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
)

const (
	repository = "ghcr.io/example/hermes"
	reference  = repository + ":1.2.0"
	hermes     = "0.22.2"
)

// fakeDocker answers pull, image inspect, create, cp and rm like Docker, recording each call.
// Files in its folder change its answers: inspect (the inspect line), fail-ARG1, refuse (401),
// and troupe.tar (what cp streams).
const fakeDocker = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
[ -f "$d/refuse" ] && { echo "Error response from daemon: Head \"https://ghcr.io/v2/x\": unauthorized" >&2; exit 1; }
[ -f "$d/fail-$1" ] && { echo "failed" >&2; exit 1; }
case "$1" in
  pull) exit 0 ;;
  image) cat "$d/inspect" ;;
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
	f.archive(complete())
	return f
}

// with is files plus one more.
func with(files map[string]string, name, text string) map[string]string {
	files[name] = text
	return files
}

// complete is a kit's whole /usr/share/troupe/: what Extract requires, and a skill.
func complete() map[string]string {
	return map[string]string{
		"troupe/services.json":           `{"services":[]}`,
		"troupe/defaults.json":           `{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":10}`,
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
	for _, name := range []string{"services.json", "defaults.json", "files/home/.hermes/config.yaml", "files/home/.hermes/skills/a/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Fatal(err)
		}
	}
	published, err := registry.ReadPublished(folder)
	if err != nil || published != (registry.Published{Reference: reference, Type: "hermes", Version: "1.2.0", Hermes: hermes}) {
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
		"no services": func(f *fake) {
			f.archive(map[string]string{"troupe/defaults.json": "{}", "troupe/seeds/config.yaml": "x"})
		},
		"no seeds (pre 1.2)": func(f *fake) {
			f.archive(map[string]string{"troupe/defaults.json": "{}", "troupe/services.json": "{}"})
		},
		"a failed copy": func(f *fake) { f.set("fail-cp", "") },
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
		"  venice:\n    command: systemd-creds decrypt /x -\n    refresh: on-demand\n"} {
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
	restore, err := Resize(folder, 2, 8, 60)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml"))
	want := strings.Replace(strings.Replace(string(before), "  cpus: 4\n", "  cpus: 2\n", 1), "  memory: 6g\n", "  memory: 8g\n", 1)
	if string(after) != want {
		t.Fatalf("sbxenv.yaml:\n%s", after)
	}
	if storage, _ := os.ReadFile(filepath.Join(folder, "storage.env")); string(storage) != "DOCKER_SANDBOXES_ROOT_SIZE=60g\nDOCKER_SANDBOXES_DOCKER_SIZE=10g\n" {
		t.Fatalf("storage.env %q", storage)
	}
	// A file not as Write wrote it is refused, and nothing changes.
	os.WriteFile(filepath.Join(folder, "storage.env"), []byte("DOCKER_SANDBOXES_ROOT_SIZE=60g\nDOCKER_SANDBOXES_ROOT_SIZE=70g\n"), 0o600)
	if _, err := Resize(folder, 3, 9, 80); err == nil {
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
