package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/published"
)

func run(args ...string) (int, string, string) {
	var out, errs bytes.Buffer
	code := Main(args, &out, &errs)
	return code, out.String(), errs.String()
}

// Help lists what users run directly; commands that units and other commands call are hidden.
func TestHelpListsOnlyUserCommands(t *testing.T) {
	code, out, _ := run("--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	listed := regexp.MustCompile(`(?m)^  ([a-z]+) {2,}`).FindAllStringSubmatch(out, -1)
	var names []string
	for _, m := range listed {
		names = append(names, m[1])
	}
	want := "chat config create delete doctor guard reset resize restart secret start stop update upgrade version"
	if devBuild {
		want = "agents budget chat config create delete doctor guard harvest kit reseed reset resize restart secret start stop storage update upgrade version"
	}
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("listed: %s", got)
	}
	_, guardOut, _ := run("guard", "--help")
	if !strings.Contains(guardOut, "guard probe") || !strings.Contains(guardOut, "guard check NAME") {
		t.Fatalf("guard help:\n%s", guardOut)
	}
}

// Hidden commands still run and keep their own help.
func TestHiddenCommandsStillRun(t *testing.T) {
	for _, args := range [][]string{{"drain", "--help"}, {"gateway", "--help"}, {"guard", "check", "--help"}, {"units", "--help"}} {
		code, out, _ := run(args...)
		if code != 0 || !strings.Contains(out, "Usage:") {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
	if code, _, _ := run("drain"); code != 2 {
		t.Errorf("drain without an agent: exit %d", code)
	}
}

// A broken settings file never stops a unit's ExecStop: gateway stop reaches its own checks
// (here a probe against an empty sandboxd folder) instead of failing on the settings.
func TestGatewayStopIgnoresABrokenSettingsFile(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TROUPE_SANDBOXD_DIR", t.TempDir())
	os.MkdirAll(config+"/troupe", 0o700)
	os.WriteFile(config+"/troupe/config.json", []byte(`{"timezone": 5}`), 0o600)
	code, _, errs := run("gateway", "stop", "ada")
	if code != 1 || !strings.Contains(errs, "sandboxd_unhealthy") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if code, _, _ := run("config", "list"); code != 1 {
		t.Fatalf("config list accepted a broken file: exit %d", code)
	}
}

// Every command and subcommand of this build answers --help on stdout, within the width, and
// every flag it defines is described (subcommand refuses one that is not).
func TestEveryHelpFitsAndDescribesItsFlags(t *testing.T) {
	// A docker that fails: a type's help reads no registry here, and says so within the width.
	home(t)
	forms := [][]string{{}, {"create", "hermes"}, {"secret", "set"}, {"secret", "check"},
		{"config", "get"}, {"guard", "probe"}, {"guard", "check"}, {"units", "render"}, {"gateway", "stop"}}
	if devBuild {
		forms = append(forms, []string{"agents", "list"}, []string{"agents", "show"}, []string{"reseed", "skills"}, []string{"budget", "check"},
			[]string{"storage", "check"}, []string{"kit", "label"})
	}
	for _, c := range commandList {
		forms = append(forms, []string{c.name})
	}
	for _, form := range forms {
		code, out, errs := run(append(form, "--help")...)
		if code != 0 || !strings.Contains(out, "\nUsage:\n") || errs != "" {
			t.Errorf("%v: exit %d\n%s%s", form, code, out, errs)
		}
		for _, line := range strings.Split(out, "\n") {
			if len([]rune(line)) > 100 {
				t.Errorf("%v: %d columns: %q", form, len([]rune(line)), line)
			}
		}
	}
}

// create's own help is for any type and documents every flag; a type's help is its kit's: the
// sizes, the description and the questions of its latest label, read from the registry without
// a pull. A registry that cannot be read leaves create's own help, with the reason.
func TestCreateHelpIsTheKits(t *testing.T) {
	_, bin := home(t)
	if code, _, errs := run("config", "set", "registry", "ghcr.io/example"); code != 0 {
		t.Fatal(errs)
	}
	sizes := `"defaults":{"cpus":8,"memory_gib":12,"root_gib":100,"docker_gib":20}`
	coder := `{"format":1,` + sizes + `,"settings":[{"env":"GITHUB_ORGANIZATIONS","prompt":"GitHub organizations it may work in","pattern":"x"}],"services":[` +
		`{"name":"venice","prompt":"Venice API key","required":true,"delivery":"native","domains":["api.venice.ai"]},` +
		`{"name":"slack-bot","prompt":"Slack bot token (xoxb-…)","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"p-{agent}"},` +
		`{"name":"extra","prompt":"An extra key.","delivery":"native","domains":["api.example.com"]}]}`
	labels, _ := json.Marshal(map[string]string{published.LabelName: coder, "org.opencontainers.image.description": "A coding agent."})
	os.WriteFile(filepath.Join(bin, "labels"), labels, 0o644)
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"docker $*\" >> \""+bin+"/calls\"\n"+
		"[ \"$1\" = buildx ] && [ -f \""+bin+"/labels\" ] && exec cat \""+bin+"/labels\"\nexit 1\n"), 0o755)

	code, out, errs := run("create", "coder", "--help")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{"Create a new coder agent", "Default: 8.", "Default: 12.", "Default: 100.", "Default: 20.", "The kit: A coding agent.",
		"  GITHUB_ORGANIZATIONS  GitHub organizations it may work in. Optional.", "  venice                Venice API key. Required.",
		"  slack-bot             Slack bot token (xoxb-…). Required.", "  manager ", "  extra                 An extra key. Optional."} {
		if !strings.Contains(out, want) {
			t.Errorf("coder's help lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "  slack-bot ") > strings.Index(out, "  manager ") || strings.Index(out, "  manager ") > strings.Index(out, "  extra ") {
		t.Errorf("the manager comes right after the last Slack service:\n%s", out)
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "calls"))
	if !strings.Contains(string(calls), "docker buildx imagetools inspect ghcr.io/example/coder:latest ") || strings.Contains(string(calls), "pull") {
		t.Fatalf("calls:\n%s", calls)
	}

	// A registry that cannot be read: create's own help, and why. Help never fails.
	os.Remove(filepath.Join(bin, "labels"))
	code, out, errs = run("create", "--name", "x", "nosuchtype", "-h")
	if code != 0 || errs != "" || !strings.Contains(out, "nosuchtype's own sizes and questions are not shown: ghcr.io/example/nosuchtype could not be read") ||
		!strings.Contains(out, "  --memory GiB|max ") || !strings.Contains(out, "Default: the kit's.") {
		t.Fatalf("exit %d: %s%s", code, out, errs)
	}

	// create's own help asks no registry.
	os.Remove(filepath.Join(bin, "calls"))
	code, out, _ = run("create", "--help")
	if code != 0 || !strings.Contains(out, "create TYPE --name NAME") || !strings.Contains(out, "  --memory GiB|max ") ||
		!strings.Contains(out, "troupe create TYPE --help") || strings.Contains(out, "coder") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "calls")); err == nil {
		t.Fatal("create --help ran docker")
	}
}

// A flag missing from its command's help is an error, never silently undocumented.
func TestAnUndocumentedFlagIsRefused(t *testing.T) {
	set := flag.NewFlagSet("x", flag.ContinueOnError)
	set.Bool("force", false, "")
	set.Int("cap-minutes", 60, "")
	if err := documented("x", "Flags:\n  --force  Do it.\n", set); err == nil || !strings.Contains(err.Error(), "--cap-minutes") {
		t.Fatalf("%v", err)
	}
	if err := documented("x", "  --force  Do it.\n  --cap-minutes N  Wait.\n", set); err != nil {
		t.Fatal(err)
	}
}

// The type whose help is shown is the first positional argument, never a flag's value, and only
// a name that can be a kit's.
func TestTypeHelpIsTheFirstPositional(t *testing.T) {
	for args, want := range map[string]string{
		"--name coder hermes -h": "hermes", "coder --help": "coder", "--name=x hermes -h": "hermes",
		"--force coder -h": "coder", "my-new-type -h": "my-new-type",
	} {
		got, ok := typeHelpRequested(strings.Fields(args))
		if !ok || got != want {
			t.Errorf("%s: %q %v", args, got, ok)
		}
	}
	for _, args := range []string{"coder --name x", "--help", "../x --help", "--name coder -h"} {
		if got, ok := typeHelpRequested(strings.Fields(args)); ok {
			t.Errorf("%s: help for %q", args, got)
		}
	}
}
