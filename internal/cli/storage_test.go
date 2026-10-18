//go:build dev

package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// fakeStorage is an sbx that answers the inventory from inventory.json, keeps the guest
// install stream, and answers the job with result.json; calls records every probe and call.
type fakeStorage struct {
	dir    string
	runner sbx.Runner
}

func newFakeStorage(t *testing.T, inventory, result string, exit ...string) *fakeStorage {
	t.Helper()
	dir := t.TempDir()
	f := &fakeStorage{dir: dir}
	write := func(name, text string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("inventory.json", inventory, 0o644)
	write("result.json", result, 0o644)
	if len(exit) > 0 {
		write("exit", exit[0], 0o644)
	}
	write("sbx", `#!/bin/sh
d=$(dirname "$0")
printf 'sbx %s' "$*" | tr '\n' ' ' >> "$d/calls"; echo >> "$d/calls"
case "$1" in
  ls) cat "$d/inventory.json" ;;
  exec)
    cat > "$d/install.tar"
    cat "$d/result.json"
    exit "$(cat "$d/exit" 2>/dev/null || echo 0)" ;;
esac
`, 0o755)
	f.runner = sbx.Runner{Binary: filepath.Join(dir, "sbx"), Log: filepath.Join(dir, "native.log"),
		Probe: func(context.Context) error { return appendLine(filepath.Join(dir, "calls"), "probe") }}
	return f
}

func appendLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.WriteString(file, line+"\n")
	return err
}

func (f *fakeStorage) calls(t *testing.T) []string {
	data, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

const oneSandbox = `[{"name":"ada","status":"running"},{"name":"grace"}]`
const passed = `{"step": "storage", "status": "passed", "root_gib": 200, "docker_gib": 50, "host_workspaces": []}`

func TestStorageCheckPasses(t *testing.T) {
	f := newFakeStorage(t, oneSandbox, passed)
	class, err := checkStorage(env{ctx: context.Background()}, f.runner, "ada", "200", "50", "")
	if err != nil || class != "" {
		t.Fatalf("class %q err %v", class, err)
	}
	calls := f.calls(t)
	want := []string{
		"probe", "sbx ls --json",
		"probe", "sbx exec -i --workdir /opt/hermes ada sh -c ", // the run script, then its arguments
	}
	if len(calls) != len(want) {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(calls[i], want[i]) {
			t.Errorf("call %d = %q, want prefix %q", i, calls[i], want[i])
		}
	}
	if !strings.HasSuffix(calls[3], " sh "+guest.Python+" "+guest.Hermes+" storage 200 50 -") {
		t.Errorf("job not run on Hermes's Python: %q", calls[3])
	}

	// The stream sent with the job is a tar of exactly the embedded guest files.
	stream, _ := os.ReadFile(filepath.Join(f.dir, "install.tar"))
	reader := tar.NewReader(bytes.NewReader(stream))
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if strings.Join(names, " ") != "__main__.py align.py archive.py backup.py drain.py fresh.py harvest.py install.py kit.py ready.py restore.py selftest.py slack.py stop.py stopped.py storage.py update.py" {
		t.Fatalf("installed %v", names)
	}
}

func TestStorageCheckFailureClasses(t *testing.T) {
	cases := []struct {
		title, inventory, result, exit, workspace, class string
	}{
		{"guest reports a class", oneSandbox, `{"step": "storage", "status": "failed", "class": "device_capacity_mismatch"}`, "1", "", "device_capacity_mismatch"},
		{"sandbox missing", `[{"name":"grace"}]`, passed, "0", "", "sandbox_missing_or_ambiguous"},
		// The check execs into the VM; a stopped one would boot, so it is refused untouched.
		{"sandbox stopped", `[{"name":"ada","status":"stopped"}]`, passed, "0", "", "sandbox_not_running"},
		{"sandbox ambiguous", `[{"name":"ada"},{"name":"ada"}]`, passed, "0", "", "sandbox_missing_or_ambiguous"},
		// sbx 0.48 lists no workspaces: the guest reports a host mount that does not match.
		{"guest reports a host mount", oneSandbox, `{"step": "storage", "status": "failed", "class": "unexpected_host_directory_mount"}`, "1", "", "unexpected_host_directory_mount"},
		{"inventory unreadable", `not json`, passed, "0", "", "inventory_invalid"},
		// The listing before sbx 0.48, whose sandboxes would otherwise read as missing.
		{"inventory in the old wrapped shape", `{"sandboxes":[{"name":"ada","status":"running"}]}`, passed, "0", "", "inventory_invalid"},
		{"inventory null", `null`, passed, "0", "", "inventory_invalid"},
		{"guest crashed", oneSandbox, `Traceback`, "1", "", "guest_check_failed"},
		{"guest answered nonsense", oneSandbox, `Traceback`, "0", "", "guest_result_invalid"},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			f := newFakeStorage(t, c.inventory, c.result, c.exit)
			class, err := checkStorage(env{ctx: context.Background()}, f.runner, "ada", "200", "50", c.workspace)
			if err == nil || class != c.class {
				t.Fatalf("class %q err %v, want %q", class, err, c.class)
			}
		})
	}
}

func TestStorageCheckWithDeclaredWorkspace(t *testing.T) {
	f := newFakeStorage(t, oneSandbox, passed)
	if class, err := checkStorage(env{ctx: context.Background()}, f.runner, "ada", "200", "0", "/srv/x"); err != nil {
		t.Fatalf("class %q err %v", class, err)
	}
	calls := f.calls(t)
	if last := calls[len(calls)-1]; !strings.HasSuffix(last, "storage 200 0 /srv/x") {
		t.Fatalf("job call %q", last)
	}
}
