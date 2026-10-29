package sudo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSudo puts a sudo on PATH that records its calls: -n true answers nopasswd's exit, -v
// answers verify's.
func fakeSudo(t *testing.T, nopasswd, verify int) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"sudo $*\" >> \"" + bin + "/calls\"\n" +
		"[ \"$1\" = -n ] && exit " + string(rune('0'+nopasswd)) + "\n" +
		"[ \"$1\" = -v ] && exit " + string(rune('0'+verify)) + "\nexit 9\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return filepath.Join(bin, "calls")
}

func calls(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

// Passwordless sudo asks nothing: sudo-rs would prompt on -v for a password the user lacks.
func TestPasswordlessSudoSkipsTheVerify(t *testing.T) {
	log := fakeSudo(t, 0, 1)
	if err := Ask(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, log); got != "sudo -n true\nsudo -n -v" {
		t.Fatalf("calls %q", got)
	}
}

func TestSudoWithAPasswordAsksOnce(t *testing.T) {
	log := fakeSudo(t, 1, 0)
	if err := Ask(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, log); got != "sudo -n true\nsudo -v" {
		t.Fatalf("calls %q", got)
	}
}

func TestARefusedSudoIsAnError(t *testing.T) {
	fakeSudo(t, 1, 1)
	if err := Ask(context.Background(), nil); err == nil {
		t.Fatal("no error")
	}
}
