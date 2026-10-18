//go:build dev

package agent

import "testing"

// checkInstalled reads a reset's receipts as update does: the composed seeds' bytes.
func checkInstalled(t *testing.T, completed, receipt string) {
	t.Helper()
	installed, err := ReadInstalled(completed, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"SOUL.md": "I am Ada.\n", "config.yaml": "model: x\n"} {
		if from, data := installed.Bytes(rel, installed.SHA256[rel]); from != receipt || string(data) != want {
			t.Errorf("%s: kept %q from %q", rel, data, from)
		}
	}
}
