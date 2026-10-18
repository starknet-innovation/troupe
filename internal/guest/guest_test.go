package guest

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe"
)

// hermesRoot is a stand-in Hermes folder whose package manager answers project_python with
// the body given.
func hermesRoot(t *testing.T, parent, lookup string) string {
	t.Helper()
	dir, err := os.MkdirTemp(parent, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir+"/pm", 0o755)
	os.WriteFile(dir+"/pm/__init__.py", nil, 0o644)
	os.WriteFile(dir+"/pm/environments.py", []byte("from pathlib import Path\ndef project_python(root):\n"+lookup+"\n"), 0o644)
	return dir
}

// The run script, run here with the real stream and a local Python standing in for
// Hermes's, unpacks the jobs into a fresh folder, passes the job's exit status through and
// leaves nothing behind.
func TestRunScriptRunsAFreshCopyAndCleansUp(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("the guest jobs' own tests need python3 on the developer machine")
	}
	stream, err := archive(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hermes := hermesRoot(t, t.TempDir(), "    return Path("+strconv.Quote(python)+")")
	tmp := t.TempDir()
	run := func(args ...string) (int, string) {
		cmd := exec.Command("sh", append([]string{"-c", runScript, "sh", python, hermes}, args...)...)
		cmd.Stdin = bytes.NewReader(stream)
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
		out, _ := cmd.CombinedOutput()
		return cmd.ProcessState.ExitCode(), string(out)
	}
	if code, out := run("no-such-job"); code != 2 || !strings.Contains(out, "selftest") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if code, _ := run("update"); code != 2 {
		t.Fatalf("job usage error exit %d", code)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// Hermes runs on the interpreter its package manager selected: the job runs there, and a
// lookup that fails or names no interpreter stops the call.
func TestJobsRunOnTheInterpreterHermesUses(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("the guest jobs' own tests need python3 on the developer machine")
	}
	stream, err := archive(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	// The interpreter PM names: python3, leaving a mark that it ran.
	selected := tmp + "/selected-python"
	mark := tmp + "/ran"
	os.WriteFile(selected, []byte("#!/bin/sh\ntouch "+mark+"\nexec "+python+" \"$@\"\n"), 0o755)
	run := func(hermes string) (int, string) {
		cmd := exec.Command("sh", "-c", runScript, "sh", python, hermes, "no-such-job")
		cmd.Stdin = bytes.NewReader(stream)
		out, _ := cmd.CombinedOutput()
		return cmd.ProcessState.ExitCode(), string(out)
	}
	if code, out := run(hermesRoot(t, tmp, "    return Path("+strconv.Quote(selected)+")")); code != 2 || !strings.Contains(out, "selftest") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("the job did not run on the interpreter PM selected")
	}
	for name, lookup := range map[string]string{
		"a failing lookup":      "    raise RuntimeError('no environment')",
		"a missing interpreter": "    return Path(" + strconv.Quote(tmp+"/gone") + ")",
	} {
		if code, out := run(hermesRoot(t, tmp, lookup)); code != 1 || !strings.Contains(out, `"hermes_python_unknown"`) {
			t.Fatalf("%s: exit %d, output %q", name, code, out)
		}
	}
	if code, out := run(tmp + "/no-package-manager"); code != 1 || !strings.Contains(out, `"hermes_python_unknown"`) {
		t.Fatalf("no package manager: exit %d, output %q", code, out)
	}
}

func TestArchiveHoldsTheJobsAndIsStable(t *testing.T) {
	all, err := files()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range all {
		names = append(names, f.name)
	}
	// The release ships every job but the dev build's harvest, which only the dev build adds.
	want := "__main__.py align.py archive.py backup.py drain.py fresh.py install.py kit.py ready.py restore.py selftest.py slack.py stop.py stopped.py storage.py update.py"
	if _, err := fs.Stat(troupe.GuestDev, "guest/harvest.py"); err == nil {
		want = "__main__.py align.py archive.py backup.py drain.py fresh.py harvest.py install.py kit.py ready.py restore.py selftest.py slack.py stop.py stopped.py storage.py update.py"
	}
	if strings.Join(names, " ") != want {
		t.Fatalf("files %v", names)
	}
	x, _ := archive(nil, nil)
	y, _ := archive(nil, nil)
	if !bytes.Equal(x, y) {
		t.Fatal("archive bytes differ between calls")
	}
}

// Each build's jobs stand alone: every module imports with only that build's files beside it
// (a release job importing a dev build's module would fail inside the VM, after its removal).
func TestEveryShippedJobImportsWithOnlyItsBuildsFiles(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("the guest jobs' own tests need python3 on the developer machine")
	}
	all, err := files()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, f := range all {
		if err := os.WriteFile(dir+"/"+f.name, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range all {
		module := strings.TrimSuffix(f.name, ".py")
		if module == "__main__" {
			continue
		}
		cmd := exec.Command(python, "-B", "-I", "-c", "import sys; sys.path.insert(0, sys.argv[1]); import importlib; importlib.import_module(sys.argv[2])", dir, module)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", module, err, out)
		}
	}
}
