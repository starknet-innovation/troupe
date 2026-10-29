package cli

import (
	"bytes"
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
		want = "agents budget chat config create delete doctor guard harvest reseed reset resize restart secret start stop storage update upgrade version"
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
	forms := [][]string{{}, {"create", "coder"}, {"create", "hermes"}, {"secret", "set"}, {"secret", "check"},
		{"config", "get"}, {"guard", "probe"}, {"guard", "check"}, {"units", "render"}, {"gateway", "stop"}}
	if devBuild {
		forms = append(forms, []string{"agents", "list"}, []string{"agents", "show"}, []string{"reseed", "skills"}, []string{"budget", "check"},
			[]string{"storage", "check"})
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

// Each type has its own create help with its own sizes and questions; create's lists the types.
func TestCreateHelpIsPerType(t *testing.T) {
	_, coder, _ := run("create", "coder", "--help")
	_, hermes, _ := run("create", "--name", "x", "hermes", "-h")
	_, short, _ := run("create", "--help")
	for _, want := range []string{"Default: 8.", "Default: 100.", "  organizations ", "  manager "} {
		if !strings.Contains(coder, want) {
			t.Errorf("coder lacks %q:\n%s", want, coder)
		}
	}
	for _, want := range []string{"Default: 6.", "Default: 50.", "  venice "} {
		if !strings.Contains(hermes, want) || strings.Contains(hermes, "Slack member") {
			t.Errorf("hermes help lacks %q or names Slack:\n%s", want, hermes)
		}
	}
	if !strings.Contains(short, "coder or hermes") || !strings.Contains(short, "troupe create hermes --help") ||
		strings.Contains(short, "--memory") {
		t.Errorf("short help:\n%s", short)
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

// The type whose help is shown is the first positional argument, never a flag's value.
func TestTypeHelpIsTheFirstPositional(t *testing.T) {
	for args, want := range map[string]string{
		"--name coder hermes -h": "hermes", "coder --help": "coder", "--name=x hermes -h": "hermes",
		"--force coder -h": "coder",
	} {
		got, ok := typeHelpRequested(strings.Fields(args))
		if !ok || got.Name != want {
			t.Errorf("%s: %q %v", args, got.Name, ok)
		}
	}
	if _, ok := typeHelpRequested([]string{"coder", "--name", "x"}); ok {
		t.Error("help without -h")
	}
}

// create notices a pulled kit that no longer matches its type's help: sizes, order, or a value
// that turned optional; the manager comes right after the last Slack service.
func TestAKitThatNoLongerMatchesItsTypeIsNoticed(t *testing.T) {
	kit := t.TempDir()
	write := func(services string) {
		os.WriteFile(filepath.Join(kit, "services.json"), []byte(`{"services":[`+services+`]}`), 0o644)
	}
	venice := `{"name":"venice","prompt":"V","required":true,"delivery":"native","domains":["api.venice.ai"]}`
	hermes, _ := published.TypeNamed("hermes")
	write(venice)
	asks, err := kitAsks(kit, nil)
	if err != nil || hermes.Differs(hermes.Defaults, asks) {
		t.Fatalf("%v %v", asks, err)
	}
	if !hermes.Differs(published.Defaults{CPUs: 4, MemoryGiB: 8, RootGiB: 50, DockerGiB: 10}, asks) {
		t.Error("other sizes unnoticed")
	}
	write(strings.Replace(venice, `"required":true`, `"required":false`, 1))
	if asks, _ := kitAsks(kit, nil); !hermes.Differs(hermes.Defaults, asks) {
		t.Error("an optional key unnoticed")
	}
	write(`{"name":"slack-bot","prompt":"B","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"p-{agent}"},` + venice)
	asks, _ = kitAsks(kit, []published.Setting{{Env: "TEAM"}})
	var names []string
	for _, a := range asks {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "TEAM,slack-bot,manager,venice" {
		t.Errorf("order %v", names)
	}
}
