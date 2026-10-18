package cli

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
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
	want := "chat config create delete doctor guard reset resize restart secret start stop upgrade version"
	if devBuild {
		want = "agents chat config create delete doctor guard harvest reset resize restart secret start stop storage update upgrade version"
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
