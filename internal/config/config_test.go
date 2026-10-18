package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolate gives the test its own machine settings folder and working folder.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Chdir(t.TempDir())
	return filepath.Join(dir, "troupe", "config.json")
}

func TestSettingsRoundTrip(t *testing.T) {
	path := isolate(t)
	if _, set, err := Get("collector-timer"); err != nil || set {
		t.Fatalf("fresh machine: set=%v err=%v", set, err)
	}
	if err := Set("collector-timer", "versions.timer"); err != nil {
		t.Fatal(err)
	}
	if err := Set("timezone", "Europe/Paris"); err != nil {
		t.Fatal(err)
	}
	if value, set, err := Get("collector-timer"); err != nil || !set || value != "versions.timer" {
		t.Fatalf("get: %q %v %v", value, set, err)
	}
	machine, err := LoadMachine()
	if err != nil || machine.CollectorTimer != "versions.timer" || machine.Location().String() != "Europe/Paris" {
		t.Fatalf("load: %+v %v", machine, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
	if err := Unset("collector-timer"); err != nil {
		t.Fatal(err)
	}
	machine, _ = LoadMachine()
	if machine.CollectorTimer != "" || machine.Timezone != "Europe/Paris" {
		t.Fatalf("after unset: %+v", machine)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "config.json.*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestSettingsRefuseBadValuesAndKeys(t *testing.T) {
	isolate(t)
	for key, values := range map[string][]string{
		"collector-timer": {"versions", "", "a b.timer"},
		"timezone":        {"Mars/Base", "", "Local"},
	} {
		for _, value := range values {
			if err := Set(key, value); err == nil {
				t.Errorf("set %s %q accepted", key, value)
			}
		}
	}
	if err := Set("max-agents", "1"); err == nil || !strings.Contains(err.Error(), "collector-timer") {
		t.Errorf("unknown key: %v", err)
	}
	if _, _, err := Get("nope"); err == nil {
		t.Error("get of an unknown key accepted")
	}
}

func TestHandEditedBadValueIsRefused(t *testing.T) {
	path := isolate(t)
	writeFile(t, path, `{"collector-timer": "versions"}`)
	if _, err := LoadMachine(); err == nil || !strings.Contains(err.Error(), "collector-timer") {
		t.Fatalf("load: %v", err)
	}
}

func TestEmptyTimezoneIsTheHostZone(t *testing.T) {
	isolate(t)
	machine, err := LoadMachine()
	if err != nil || machine.Location().String() != "Local" {
		t.Fatalf("%v %v", machine.Location(), err)
	}
}

func TestNullFileAndStoredTypes(t *testing.T) {
	path := isolate(t)
	writeFile(t, path, "null")
	if err := Set("timezone", "UTC"); err != nil {
		t.Fatalf("set over a null file: %v", err)
	}
	for _, text := range []string{`{"collector-timer": 2}`, `{"timezone": 5}`} {
		writeFile(t, path, text)
		if _, err := LoadMachine(); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
}

func TestRegistryDefaultsToStagingAndTakesARepositoryPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if m, err := LoadMachine(); err != nil || m.Registry != DefaultRegistry {
		t.Fatalf("registry %q, %v", m.Registry, err)
	}
	for _, good := range []string{"ghcr.io/example", "registry.example.com:5000/team/kits"} {
		if err := Set("registry", good); err != nil {
			t.Fatalf("%s: %v", good, err)
		}
		if m, _ := LoadMachine(); m.Registry != good {
			t.Fatalf("registry %q", m.Registry)
		}
	}
	for _, bad := range []string{"ghcr.io", "https://ghcr.io/example", "ghcr.io/Example", "ghcr.io/example/", "ghcr.io//x"} {
		if err := Set("registry", bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// The resource budget replaced max-running-agents: a file that still holds it says so, and
// unset removes it, keeping every other setting.
func TestARetiredSettingIsNamedAndUnset(t *testing.T) {
	path := isolate(t)
	writeFile(t, path, `{"max-running-agents": 1, "timezone": "UTC"}`)
	if _, err := LoadMachine(); err == nil || !strings.Contains(err.Error(), "max-running-agents is retired: the resource budget replaces it; run troupe config unset max-running-agents") {
		t.Fatalf("load: %v", err)
	}
	if err := Set("max-running-agents", "2"); err == nil {
		t.Fatal("set a retired setting")
	}
	if err := Unset("max-running-agents"); err != nil {
		t.Fatal(err)
	}
	if m, err := LoadMachine(); err != nil || m.Timezone != "UTC" {
		t.Fatalf("after unset: %+v %v", m, err)
	}
}
