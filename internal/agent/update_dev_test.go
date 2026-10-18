//go:build dev

package agent

import (
	"context"
	"strings"
	"testing"
)

func TestFailedInstallRestoresEverything(t *testing.T) {
	h := newUpdateHost(t)
	h.installAll()
	h.seed("base", "scripts/watch.py", "print('v2')\n")
	h.seed("base", "scripts/notes.py", "print('notes')\n")
	t.Setenv("TROUPE_FORCE_INSTALL_FAILURE", "1")
	err := h.agent.applyPlan(context.Background(), h.plan(Scripts))
	if !strings.HasPrefix(failureClass(err), "install_failed forced_failure") {
		t.Fatalf("err %v", err)
	}
	if text, _ := h.liveText("scripts/watch.py"); text != "print('v1')\n" {
		t.Fatalf("watch.py not restored: %q", text)
	}
	if _, present := h.liveText("scripts/notes.py"); present {
		t.Fatal("a new file survived the rollback")
	}
}
