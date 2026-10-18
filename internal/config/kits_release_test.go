//go:build !dev

package config

import (
	"os"
	"strings"
	"testing"
)

// A release build has no kits root: no kits setting to set or list.
func TestAReleaseHasNoKitsSetting(t *testing.T) {
	isolate(t)
	for _, s := range Settings {
		if s.Key == "kits" {
			t.Fatal("the kits setting is listed")
		}
	}
	if err := Set("kits", t.TempDir()); err == nil {
		t.Fatal("set kits")
	}
}

// A kits root a dev build stored on this machine never breaks a release, which can unset it.
func TestAReleaseIgnoresAndUnsetsAStoredKitsRoot(t *testing.T) {
	path := isolate(t)
	writeFile(t, path, `{"kits": "/somewhere", "timezone": "UTC"}`)
	if m, err := LoadMachine(); err != nil || m.Timezone != "UTC" {
		t.Fatalf("load: %+v %v", m, err)
	}
	if err := Unset("kits"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "kits") {
		t.Fatalf("still stored: %s", data)
	}
}
