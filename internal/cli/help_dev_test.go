//go:build dev

package cli

import (
	"strings"
	"testing"
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
	if !strings.Contains(out, "troupe [--kits DIR] <command>") || !strings.Contains(out, "kits machine") {
		t.Fatalf("help:\n%s", out)
	}
}
