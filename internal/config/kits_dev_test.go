//go:build dev

package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The kits root is --kits, else the kits setting; a troupe.json or TROUPE_KITS names none.
func TestResolveTakesTheFlagThenTheSetting(t *testing.T) {
	isolate(t)
	flagDir, machineDir, marked := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(marked, "troupe.json"), "{}\n")
	t.Chdir(marked)
	t.Setenv("TROUPE_KITS", t.TempDir())
	if _, err := Resolve(""); !errors.Is(err, ErrNoKits) {
		t.Fatalf("a troupe.json or TROUPE_KITS named a kits root: %v", err)
	}
	if err := Set("kits", machineDir); err != nil {
		t.Fatal(err)
	}
	if kits, err := Resolve(""); err != nil || kits.Root != machineDir {
		t.Fatalf("setting: %v %v", kits.Root, err)
	}
	if kits, err := Resolve(flagDir); err != nil || kits.Root != flagDir {
		t.Fatalf("flag: %v %v", kits.Root, err)
	}
}

func TestResolveFailures(t *testing.T) {
	isolate(t)
	if _, err := Resolve(""); !errors.Is(err, ErrNoKits) || !strings.Contains(err.Error(), "troupe config set kits") {
		t.Errorf("no kits root: %v", err)
	}
	if _, err := Resolve(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("accepted a missing folder")
	}
	if err := Set("kits", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("set kits to a missing folder")
	}
}

// A kits folder that moved after it was set still loads, so commands that don't need the
// kits keep working; resolving the kits root then names the setting.
func TestMovedKitsFolder(t *testing.T) {
	path := isolate(t)
	gone := filepath.Join(t.TempDir(), "moved")
	writeFile(t, path, `{"kits": "`+gone+`", "timezone": "UTC"}`)
	machine, err := LoadMachine()
	if err != nil || machine.Timezone != "UTC" {
		t.Fatalf("load: %+v %v", machine, err)
	}
	if _, err := Resolve(""); err == nil || !strings.Contains(err.Error(), "troupe config set kits") {
		t.Fatalf("resolve: %v", err)
	}
}

// The dev build checks a stored kits root's shape like any setting.
func TestADevBuildRefusesARelativeKitsRoot(t *testing.T) {
	path := isolate(t)
	writeFile(t, path, `{"kits": "relative/path"}`)
	if _, err := LoadMachine(); err == nil {
		t.Fatal("accepted a relative kits root")
	}
}

func TestTheKitsSettingIsSecond(t *testing.T) {
	if len(Settings) < 2 || Settings[0].Key != "timezone" || Settings[1].Key != "kits" {
		t.Fatalf("settings %v", Settings)
	}
}
