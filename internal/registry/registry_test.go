package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const example = "../../examples/kit"

func exampleRegistry(t *testing.T) Registry {
	t.Helper()
	root, err := filepath.Abs(example)
	if err != nil {
		t.Fatal(err)
	}
	return Registry{Root: root}
}

func TestNamesFindsOnlyAgents(t *testing.T) {
	names, err := exampleRegistry(t).Names()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ada", "grace"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
}

// Each rule of an agent folder, broken once in a copy of the example kit.
func TestNamesRejectsFoldersThatAreNotAgents(t *testing.T) {
	cases := map[string]string{
		"name differs from folder": "name: other\nkits:\n  - \"../../kits/coder\"\n  - \".\"\n",
		"standalone kit":           "name: ada\nkits:\n  - \".\"\n",
		"base only, as a Lab":      "name: ada\nkits:\n  - \"../../kits/coder\"\n",
		"self not last":            "name: ada\nkits:\n  - \".\"\n  - \"../../kits/coder\"\n",
		"base is a mixin":          "name: ada\nkits:\n  - \"../grace\"\n  - \".\"\n",
		"base not a relative path": "name: ada\nkits:\n  - \"kits/coder\"\n  - \".\"\n",
		"three kits":               "name: ada\nkits:\n  - \"../../kits/coder\"\n  - \"../grace\"\n  - \".\"\n",
	}
	for title, sbxenv := range cases {
		t.Run(title, func(t *testing.T) {
			root := copyKit(t)
			write(t, filepath.Join(root, "agents", "ada", "sbxenv.yaml"), sbxenv)
			names, err := Registry{Root: root}.Names()
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"grace"}; !reflect.DeepEqual(names, want) {
				t.Fatalf("names %v, want %v", names, want)
			}
		})
	}
}

func TestKitsSecretsAndEnv(t *testing.T) {
	r := exampleRegistry(t)
	kits, err := r.Kits("ada")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(r.Root, "kits", "coder"), filepath.Join(r.Root, "agents", "ada")}; !reflect.DeepEqual(kits, want) {
		t.Fatalf("kits %v, want %v", kits, want)
	}
	secrets, err := r.Secrets("ada")
	if err != nil || !reflect.DeepEqual(secrets, []string{"provider"}) {
		t.Fatalf("secrets %v, %v", secrets, err)
	}
	env, err := r.Env("ada")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"AGENT_NAME": "Ada", "AGENT_ROLE": "reviewer"}; !reflect.DeepEqual(env, want) {
		t.Fatalf("env %v, want %v", env, want)
	}
}

func TestUnknownAgent(t *testing.T) {
	_, err := exampleRegistry(t).Kits("nobody")
	var unknown UnknownAgentError
	if !errors.As(err, &unknown) || !strings.Contains(err.Error(), "nobody is not an agent in") {
		t.Fatalf("err %v", err)
	}
}

func TestComposeMergesKitsAndSkipsBytecode(t *testing.T) {
	root := copyKit(t)
	write(t, filepath.Join(root, "kits", "coder", seedHome, "scripts", "__pycache__", "x.pyc"), "bytecode")
	seeds, err := Registry{Root: root}.Compose("ada")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, seed := range seeds {
		paths = append(paths, seed.Path)
	}
	want := []string{".env", "SOUL.md", "config.yaml", "skills/house-rules/SKILL.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}

func TestComposeRefusesAPathTwoKitsProvide(t *testing.T) {
	root := copyKit(t)
	write(t, filepath.Join(root, "agents", "ada", seedHome, "config.yaml"), "agent override\n")
	_, err := Registry{Root: root}.Compose("ada")
	if err == nil || err.Error() != "seed path provided by two kits: config.yaml" {
		t.Fatalf("err %v", err)
	}
}

func TestMaterializeKeepsBytesModeAndTime(t *testing.T) {
	root := copyKit(t)
	source := filepath.Join(root, "agents", "ada", seedHome, "SOUL.md")
	stamp := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := (Registry{Root: root}).Materialize("ada", out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(out, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.ModTime().Equal(stamp) {
		t.Fatalf("mode %v time %v", info.Mode().Perm(), info.ModTime())
	}
	got, _ := os.ReadFile(filepath.Join(out, "skills", "house-rules", "SKILL.md"))
	want, _ := os.ReadFile(filepath.Join(root, "kits", "coder", seedHome, "skills", "house-rules", "SKILL.md"))
	if string(got) != string(want) {
		t.Fatal("materialized bytes differ")
	}
}

func TestSlackManifestNamesTheAppAndKeepsOrder(t *testing.T) {
	manifest, err := exampleRegistry(t).SlackManifest("grace")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Display  struct{ Name string } `json:"display_information"`
		Features struct {
			Bot struct {
				DisplayName string `json:"display_name"`
			} `json:"bot_user"`
		} `json:"features"`
	}
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Display.Name != "Grace" || decoded.Features.Bot.DisplayName != "Grace" {
		t.Fatalf("names %+v", decoded)
	}
	text := string(manifest)
	if strings.Index(text, "display_information") > strings.Index(text, "oauth_config") {
		t.Fatal("key order not kept")
	}
	if !strings.Contains(text, "\n  \"display_information\": {\n    \"name\": \"Grace\",") {
		t.Fatalf("not indented like the source:\n%s", text)
	}
}

func TestSlackManifestRefusesANamedBaseManifest(t *testing.T) {
	root := copyKit(t)
	path := filepath.Join(root, "kits", "coder", "slack-app-manifest.json")
	data, _ := os.ReadFile(path)
	write(t, path, strings.Replace(string(data), `"name": "AGENT"`, `"name": "Someone"`, 1))
	if _, err := (Registry{Root: root}).SlackManifest("ada"); err == nil {
		t.Fatal("a manifest that already names the app was accepted")
	}
}

func TestEncodeStringMatchesPythonJSON(t *testing.T) {
	cases := map[string]string{
		"plain":         `"plain"`,
		"quote\"back\\": `"quote\"back\\"`,
		"tab\tnl\n":     `"tab\tnl\n"`,
		"\x01":          `"\u0001"`,
		"café <&>":      `"café <&>"`,
	}
	for in, want := range cases {
		var out bytes.Buffer
		encodeString(&out, in)
		if out.String() != want {
			t.Errorf("encodeString(%q) = %s, want %s", in, out.String(), want)
		}
	}
}

// A linked agent folder takes its base from its real location, and a linked seed folder is
// read through the link, as the Python registry does.
func TestSymlinkedFolders(t *testing.T) {
	root := copyKit(t)
	elsewhere := t.TempDir()
	os.RemoveAll(filepath.Join(root, "agents", "grace"))
	if err := os.MkdirAll(filepath.Join(elsewhere, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "agents", "ada"), filepath.Join(elsewhere, "agents", "ada")); err != nil {
		t.Fatal(err)
	}
	// The real folder sits beside a base of its own, so ../../kits/coder must resolve there.
	if err := os.Rename(filepath.Join(root, "kits"), filepath.Join(elsewhere, "kits")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "agents", "ada"), filepath.Join(root, "agents", "ada")); err != nil {
		t.Fatal(err)
	}
	names, err := Registry{Root: root}.Names()
	if err != nil || !reflect.DeepEqual(names, []string{"ada"}) {
		t.Fatalf("names %v err %v", names, err)
	}

	seeds := filepath.Join(elsewhere, "agents", "ada", seedHome)
	moved := filepath.Join(t.TempDir(), "seeds")
	if err := os.Rename(seeds, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, seeds); err != nil {
		t.Fatal(err)
	}
	composed, err := Registry{Root: root}.Compose("ada")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, seed := range composed {
		paths = append(paths, seed.Path)
	}
	if want := []string{".env", "SOUL.md", "config.yaml", "skills/house-rules/SKILL.md"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}

// copyKit copies the example kit into a temporary root a test may change.
func copyKit(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.Walk(example, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(example, path)
		target := filepath.Join(root, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Kits and agents may sit one level deeper (team/kits/coder, team/agents/ada): agents are
// found up to three levels below the root, and their kits resolve from their own folder.
func TestNamesFindsAgentsOneLevelDeeper(t *testing.T) {
	root := copyKit(t)
	team := filepath.Join(root, "team")
	if err := os.Mkdir(team, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kits", "agents", "labs"} {
		if err := os.Rename(filepath.Join(root, name), filepath.Join(team, name)); err != nil {
			t.Fatal(err)
		}
	}
	r := Registry{Root: root}
	names, err := r.Names()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ada", "grace"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	kits, err := r.Kits("ada")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(team, "kits", "coder"), filepath.Join(team, "agents", "ada")}; !reflect.DeepEqual(kits, want) {
		t.Fatalf("kits %v, want %v", kits, want)
	}
	seeds, err := r.Compose("ada")
	if err != nil || len(seeds) == 0 {
		t.Fatalf("compose: %v %v", seeds, err)
	}
}

// An agent folder nested inside its workload kit (coder/agents/ada composing "../.." and ".")
// is found at depth three, and the workload folder itself is never an agent.
func TestAgentsInsideTheirWorkloadFolder(t *testing.T) {
	root := copyKit(t)
	coder := filepath.Join(root, "kits", "coder")
	if err := os.Rename(coder, filepath.Join(root, "coder")); err != nil {
		t.Fatal(err)
	}
	coder = filepath.Join(root, "coder")
	if err := os.Rename(filepath.Join(root, "agents"), filepath.Join(coder, "agents")); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(root, "labs"))
	for _, name := range []string{"ada", "grace"} {
		write(t, filepath.Join(coder, "agents", name, "sbxenv.yaml"), "name: "+name+"\nkits:\n  - \"../..\"\n  - \".\"\n")
	}
	// The workload's own Lab: kits ".." and ".", named NAME-lab.
	write(t, filepath.Join(coder, "lab", "sbxenv.yaml"), "name: coder-lab\nkits:\n  - \"..\"\n  - \".\"\n")
	r := Registry{Root: root}
	names, err := r.Names()
	if want := []string{"ada", "grace"}; err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, err %v", names, err)
	}
	if kits, err := r.Kits("ada"); err != nil || kits[0] != coder {
		t.Fatalf("kits %v, err %v", kits, err)
	}
}

// A base is a v3 workload: the folder holds FOLDER.yaml saying kind: workload. A mixin, a
// descriptor not named after its folder, or a v2 spec.yaml is not one.
func TestABaseIsAV3WorkloadNamedAfterItsFolder(t *testing.T) {
	descriptor := func(root string) string { return filepath.Join(root, "kits", "coder", "coder.yaml") }
	cases := map[string]func(t *testing.T, root string){
		"kind mixin": func(t *testing.T, root string) {
			write(t, descriptor(root), "# syntax=docker/sandbox-kit:3\nkind: mixin\nname: coder\n")
		},
		"descriptor not named after the folder": func(t *testing.T, root string) {
			if err := os.Rename(descriptor(root), filepath.Join(root, "kits", "coder", "workload.yaml")); err != nil {
				t.Fatal(err)
			}
		},
		"v2 spec.yaml": func(t *testing.T, root string) {
			os.Remove(descriptor(root))
			write(t, filepath.Join(root, "kits", "coder", "spec.yaml"), "schemaVersion: \"2\"\nkind: sandbox\n")
		},
		"kind workload in spec.yaml only": func(t *testing.T, root string) {
			os.Remove(descriptor(root))
			write(t, filepath.Join(root, "kits", "coder", "spec.yaml"), "kind: workload\n")
		},
	}
	for title, plant := range cases {
		t.Run(title, func(t *testing.T) {
			root := copyKit(t)
			plant(t, root)
			names, err := Registry{Root: root}.Names()
			if err != nil || len(names) != 0 {
				t.Fatalf("names %v, err %v", names, err)
			}
		})
	}
	// The negative control: the untouched example's base is one.
	if !isBase(filepath.Join(exampleRegistry(t).Root, "kits", "coder")) {
		t.Fatal("the example workload is not a base")
	}
}

// Two agents with one name anywhere under the root are refused, never one picked.
func TestTwoAgentsWithOneNameAreRefused(t *testing.T) {
	root := copyKit(t)
	other := filepath.Join(root, "other")
	for _, name := range []string{"kits/coder", "agents/ada"} {
		if err := os.CopyFS(filepath.Join(other, name), os.DirFS(filepath.Join(root, name))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (Registry{Root: root}).Names(); err == nil || !strings.Contains(err.Error(), "two agents named ada") {
		t.Fatalf("err %v", err)
	}
}

// A Lab lives in a folder of its own name (labs/coder-lab), and composing the workload and a
// mixin of its own (kits: ["../../kits/coder", "."]) like an agent, it is still only a Lab.
func TestALabNamedLikeItsFolderIsNeverAnAgent(t *testing.T) {
	root := copyKit(t)
	write(t, filepath.Join(root, "labs", "coder-lab", "sbxenv.yaml"), "name: coder-lab\nkits:\n  - \"../../kits/coder\"\n  - \".\"\n")
	names, err := Registry{Root: root}.Names()
	if want := []string{"ada", "grace"}; err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, err %v", names, err)
	}
}

const (
	publishedRef = "ghcr.io/example/coder:1.2.0"
	publishedEnv = "schemaVersion: \"1\"\nname: jane\nagent: coder\n\nkits:\n  - \"" + publishedRef + "\"\n  - \".\"\n"
)

// publishedData is troupe's data folder holding one published agent, jane, on a kept kit.
func publishedData(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	kit := KitFolder(data, publishedRef)
	for name, text := range map[string]string{
		filepath.Join(kit, PublishedFile):                             `{"reference":"` + publishedRef + `","type":"coder","version":"1.2.0","commit":"` + strings.Repeat("a", 40) + `"}`,
		filepath.Join(kit, "files/home/.hermes/config.yaml"):          "x\n",
		filepath.Join(data, "agents/jane/sbxenv.yaml"):                publishedEnv,
		filepath.Join(data, "agents/jane/files/home/.hermes/SOUL.md"): "# Jane\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func TestPublishedAgentsAreFoundWithoutAKitsRoot(t *testing.T) {
	data := publishedData(t)
	r := Registry{Data: data}
	names, err := r.Names()
	if err != nil || !reflect.DeepEqual(names, []string{"jane"}) {
		t.Fatalf("names %v, %v", names, err)
	}
	kits, err := r.Kits("jane")
	if err != nil || !reflect.DeepEqual(kits, []string{KitFolder(data, publishedRef), filepath.Join(data, "agents/jane")}) {
		t.Fatalf("kits %v, %v", kits, err)
	}
	seeds, err := r.Compose("jane")
	if err != nil || len(seeds) != 2 || seeds[0].Path != "SOUL.md" || seeds[1].Path != "config.yaml" {
		t.Fatalf("seeds %v, %v", seeds, err)
	}
	// Beside a kits root, both kinds are agents.
	r.Root = exampleRegistry(t).Root
	if names, err := r.Names(); err != nil || !reflect.DeepEqual(names, []string{"ada", "grace", "jane"}) {
		t.Fatalf("names %v, %v", names, err)
	}
}

func TestAPublishedAgentNeedsItsPinnedKitKept(t *testing.T) {
	cases := map[string]func(data string){
		"the kit is not kept": func(data string) { os.RemoveAll(KitFolder(data, publishedRef)) },
		"the record names another": func(data string) {
			os.WriteFile(filepath.Join(KitFolder(data, publishedRef), PublishedFile), []byte(`{"reference":"ghcr.io/other/coder:1.2.0","type":"coder","version":"1.2.0","commit":"x"}`), 0o600)
		},
		"the agent follows a tag": func(data string) {
			os.WriteFile(filepath.Join(data, "agents/jane/sbxenv.yaml"), []byte(strings.Replace(publishedEnv, ":1.2.0", ":latest", 1)), 0o600)
		},
		"the name differs": func(data string) {
			os.WriteFile(filepath.Join(data, "agents/jane/sbxenv.yaml"), []byte(strings.Replace(publishedEnv, "name: jane", "name: joe", 1)), 0o600)
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			data := publishedData(t)
			breakIt(data)
			if names, err := (Registry{Data: data}).Names(); err != nil || len(names) != 0 {
				t.Fatalf("names %v, %v", names, err)
			}
		})
	}
}

func TestAPublishedAndALocalAgentCannotShareAName(t *testing.T) {
	data := publishedData(t)
	os.Rename(filepath.Join(data, "agents/jane"), filepath.Join(data, "agents/ada"))
	env := strings.Replace(publishedEnv, "name: jane", "name: ada", 1)
	os.WriteFile(filepath.Join(data, "agents/ada/sbxenv.yaml"), []byte(env), 0o600)
	if _, err := (Registry{Root: exampleRegistry(t).Root, Data: data}).Names(); err == nil || !strings.Contains(err.Error(), "two agents named ada") {
		t.Fatalf("err %v", err)
	}
}
