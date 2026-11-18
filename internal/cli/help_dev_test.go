//go:build dev

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/published"
)

// troupe harvest reads the agent from troupe's kits root.
func TestHarvestUsesTheKitsRoot(t *testing.T) {
	// An empty sandboxd folder fails the probe: even a broken passthrough never reaches sbx.
	t.Setenv("TROUPE_SANDBOXD_DIR", t.TempDir())
	root := t.TempDir()
	code, out, errs := run("--kits", root, "harvest", "nobody")
	if code != 1 || !strings.Contains(out+errs, "unregistered_or_unresolvable_agent") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

// docs/development.md's example works as written: troupe --kits examples/kit agents list.
func TestTheReadmeExampleListsTheExampleAgents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, out, errs := run("--kits", "../../examples/kit", "agents", "list")
	if code != 0 || out != "ada\ngrace\n" {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
}

// The dev build says so in its version line, which hermes-agents' scripts check.
func TestTheDevBuildSaysSo(t *testing.T) {
	if _, out, _ := run("version"); !strings.HasSuffix(out, " (dev build)\n") {
		t.Fatalf("version %q", out)
	}
}

// A release build refuses --kits; the dev build's help names it.
func TestTheDevBuildNamesTheKitsRoot(t *testing.T) {
	_, out, _ := run("--help")
	if !strings.Contains(out, "troupe [--kits DIR] COMMAND") || !strings.Contains(out, "  --kits DIR ") {
		t.Fatalf("help:\n%s", out)
	}
}

// troupe kit label checks a kit's source and prints the label create reads back; the example
// kit is a publishable one. A kit create would refuse prints nothing and exits 1.
func TestKitLabelPrintsTheLabelCreateReads(t *testing.T) {
	code, out, errs := run("kit", "label", "../../examples/kit/kits/coder")
	if code != 0 || errs != "" || strings.Count(out, "\n") != 1 {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	kit, err := published.DecodeLabel(out)
	if err != nil || kit.Format != published.Format || kit.Defaults.CPUs != 4 || len(kit.Settings) != 1 || len(kit.Services) != 3 {
		t.Fatalf("%+v, %v", kit, err)
	}
	broken := t.TempDir()
	os.MkdirAll(filepath.Join(broken, "troupe"), 0o755)
	os.WriteFile(filepath.Join(broken, "troupe/defaults.json"), []byte(`{"cpus":4,"memory_gib":6,"root_gib":50,"docker_gib":10}`), 0o644)
	os.WriteFile(filepath.Join(broken, "troupe/services.json"), []byte(`{"services":[{"name":"venice","prompt":"V","requierd":true,"delivery":"native","domains":["api.venice.ai"]}]}`), 0o644)
	code, out, errs = run("kit", "label", broken)
	if code != 1 || out != "" || !strings.Contains(errs, "requierd") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errs)
	}
	if code, _, errs := run("kit", "label"); code != 2 || !strings.Contains(errs, "one kit folder") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}
