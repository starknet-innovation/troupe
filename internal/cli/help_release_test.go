//go:build !dev

package cli

import (
	"strings"
	"testing"
)

// A release build has no kits root and no maintainer command.
func TestAReleaseHasNoKitsRootOrMaintainerCommands(t *testing.T) {
	if code, _, errs := run("--kits", t.TempDir(), "--help"); code != 2 || !strings.Contains(errs, "-kits") {
		t.Fatalf("--kits: exit %d: %s", code, errs)
	}
	for _, name := range []string{"agents", "harvest", "storage", "update", "recreate", "migrate", "dev"} {
		if code, _, errs := run(name, "--help"); code != 2 || !strings.Contains(errs, "unknown command") {
			t.Errorf("%s: exit %d: %s", name, code, errs)
		}
	}
	if _, out, _ := run("version"); strings.Contains(out, "dev build") {
		t.Fatalf("version %q", out)
	}
	if _, out, _ := run("--help"); strings.Contains(out, "kits") {
		t.Fatalf("help names the kits root:\n%s", out)
	}
}
