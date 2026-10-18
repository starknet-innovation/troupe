//go:build dev

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// A local agent's sizes are its own folder's files: new sizes are refused.
func TestResizeRefusesNewSizesForALocalAgent(t *testing.T) {
	example, _ := filepath.Abs("../../examples/kit")
	home(t)
	if code, _, errs := run("--kits", example, "resize", "ada", "--memory", "8"); code != 1 || !strings.Contains(errs, "sizes are in its own folder") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

// A local agent moves at the sizes in its own folder: resize without sizes goes on to the move,
// which here stops at the sandboxd probe.
func TestResizeMovesALocalAgentAtItsOwnSizes(t *testing.T) {
	example, _ := filepath.Abs("../../examples/kit")
	home(t)
	t.Setenv("TROUPE_SANDBOXD_DIR", t.TempDir())
	code, _, errs := run("--kits", example, "resize", "ada")
	if code == 0 || strings.Contains(errs, "sizes are in its own folder") || !strings.Contains(errs, "sandboxd") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}
