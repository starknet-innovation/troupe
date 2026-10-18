package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/registry"
)

func TestAgentNames(t *testing.T) {
	for given, want := range map[string][2]string{
		"Jane":              {"Jane", "jane"},
		"Mr. Doyle":         {"Mr. Doyle", "mr-doyle"},
		"Ana-María":         {"Ana-María", "ana-maria"},
		"  Jane  Doe! ":     {"Jane  Doe!", "jane-doe"},
		"Łukasz Żółć":       {"Łukasz Żółć", "lukasz-zolc"},
		"Straße 9":          {"Straße 9", "strasse-9"},
		"jane":              {"jane", "jane"},
		"R2-D2 (the droid)": {"R2-D2 (the droid)", "r2-d2-the-droid"},
	} {
		display, id, err := agentNames(given)
		if err != nil || display != want[0] || id != want[1] {
			t.Errorf("%q: %q %q %v, want %q %q", given, display, id, err, want[0], want[1])
		}
	}
	for _, refused := range []string{"", "   ", "!!!", "佐藤", strings.Repeat("a", 36), "Jane\nDoe", "Jane\x1b[31m", "\xff", "D0CTORWHO42"} {
		if _, _, err := agentNames(refused); err == nil {
			t.Errorf("%q accepted", refused)
		}
	}
}

func TestDisplayNameReadsTheRecordedName(t *testing.T) {
	data := t.TempDir()
	reg := registry.Registry{Data: data}
	folder := filepath.Join(data, "agents", "mr-doyle")
	os.MkdirAll(folder, 0o700)
	os.WriteFile(filepath.Join(folder, "source.json"), []byte(`{"display_name":"Mr. Doyle"}`), 0o600)
	if got := displayName(reg, "mr-doyle"); got != "Mr. Doyle" {
		t.Fatalf("got %q", got)
	}
	// A record whose name would make another ID is not this agent's: the ID is shown instead.
	os.WriteFile(filepath.Join(folder, "source.json"), []byte(`{"display_name":"Someone Else"}`), 0o600)
	if got := displayName(reg, "mr-doyle"); got != "Mr-doyle" {
		t.Fatalf("got %q", got)
	}
	if got := displayName(reg, "grace-like"); got != "Grace-like" {
		t.Fatalf("a local agent: got %q", got)
	}
}
