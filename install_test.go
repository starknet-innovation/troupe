package troupe

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeTroupe answers like troupe: its doctor fails or warns as FAKE_DOCTOR says, and it
// renders each unit as UNIT_TEXT plus the unit's name.
const fakeTroupe = `#!/bin/sh
case "$1" in
  version) echo "troupe 1.0.0" ;;
  doctor)
    case "$FAKE_DOCTOR" in
      slow) sleep 1 ;;
      fail) echo "[10:00:00] This machine kvm: FAILED. /dev/kvm is not usable."; exit 1 ;;
      warn) echo "[10:00:00] This machine sbx: ACTION. sbx was not found." ;;
      *) echo "[10:00:00] This machine kvm: ok." ;;
    esac ;;
  units) [ "$FAKE_RENDER" = fail ] && { echo "troupe: render failed" >&2; exit 1; }; printf '%s %s\n' "$UNIT_TEXT" "$3" ;;
esac
`

// installer is one install.sh run's machine: a home, a unit folder, and fakes first on PATH
// (sudo runs what it is given, logging it; systemctl only logs; uname reports FAKE_ARCH).
type installer struct {
	t                *testing.T
	home, bin, units string
	binary, previous string
	env              []string
}

func newInstaller(t *testing.T) *installer {
	dir := t.TempDir()
	i := &installer{t: t, home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "bin"), units: filepath.Join(dir, "units"),
		binary: filepath.Join(dir, "troupe-build")}
	for _, d := range []string{i.home, i.bin, i.units} {
		os.MkdirAll(d, 0o755)
	}
	i.write(i.binary, fakeTroupe, 0o755)
	i.write(filepath.Join(i.bin, "sudo"), "#!/bin/sh\necho \"sudo $*\" >> \""+i.bin+"/calls\"\n[ \"$1\" = -v ] && exit 0\nexec \"$@\"\n", 0o755)
	// systemctl: enable --now makes the unit enabled and active (unless fail-enable exists);
	// show prints the unit's MainPID from the mainpid file.
	i.write(filepath.Join(i.bin, "systemctl"), `#!/bin/sh
d=$(dirname "$0")
case "$1" in
  is-active) [ -f "$d/active" ]; exit $? ;;
  show)
    case "$*" in
      *UnitFileState*)
        if [ -f "$d/unitfilestate" ]; then cat "$d/unitfilestate"; elif [ -f "$d/active" ]; then echo enabled; else echo disabled; fi ;;
      *) cat "$d/mainpid" 2>/dev/null ;;
    esac
    exit 0 ;;
esac
echo "systemctl $*" >> "$d/calls"
[ "$1" = enable ] && { [ -f "$d/fail-enable" ] && exit 1; touch "$d/active"; rm -f "$d/unitfilestate"; }
exit 0
`, 0o755)
	i.write(filepath.Join(i.bin, "id"), "#!/bin/sh\necho \"${FAKE_UID:-1000}\"\n", 0o755)
	i.write(filepath.Join(i.bin, "uname"), "#!/bin/sh\n[ \"$1\" = -m ] && { echo \"${FAKE_ARCH:-x86_64}\"; exit 0; }\necho Linux\n", 0o755)
	i.env = []string{"HOME=" + i.home, "PATH=" + i.bin + ":/usr/bin:/bin", "TROUPE_UNIT_DIR=" + i.units,
		"TROUPE_BINARY=" + i.binary, "UNIT_TEXT=v1", "TROUPE_SBX=/bin/sh", "TROUPE_SANDBOXD_DIR=" + filepath.Join(dir, "sandboxd")}
	return i
}

func (i *installer) write(path, text string, mode os.FileMode) {
	i.t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		i.t.Fatal(err)
	}
}

func (i *installer) run(env ...string) (int, string) {
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(append([]string{}, i.env...), env...)
	out, _ := cmd.CombinedOutput()
	return cmd.ProcessState.ExitCode(), string(out)
}

func (i *installer) calls() string {
	data, _ := os.ReadFile(filepath.Join(i.bin, "calls"))
	return string(data)
}

func (i *installer) installed() string {
	data, err := os.ReadFile(filepath.Join(i.home, ".local/bin/troupe"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestInstallPutsTroupeInPlaceAndItsUnits(t *testing.T) {
	i := newInstaller(t)
	code, out := i.run()
	if code != 0 || i.installed() != fakeTroupe || !strings.Contains(out, "Next: troupe create coder --name NAME") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, unit := range []string{"sbx-daemon.service", "hermes-gateway@.service"} {
		if data, _ := os.ReadFile(filepath.Join(i.units, unit)); string(data) != "v1 "+unit+"\n" {
			t.Fatalf("%s: %q", unit, data)
		}
	}
	if !strings.Contains(i.calls(), "systemctl daemon-reload") || !strings.Contains(i.calls(), "systemctl enable --now sbx-daemon.service") {
		t.Fatalf("calls:\n%s", i.calls())
	}
	// Run again with the same units: nothing to install, so no sudo.
	os.Remove(filepath.Join(i.bin, "calls"))
	if code, out := i.run(); code != 0 || i.calls() != "" {
		t.Fatalf("exit %d, calls %q:\n%s", code, i.calls(), out)
	}
	// A changed unit is installed again, through sudo.
	if code, out := i.run("UNIT_TEXT=v2"); code != 0 || !strings.Contains(i.calls(), "sudo install") {
		t.Fatalf("exit %d, calls %q:\n%s", code, i.calls(), out)
	}
}

func TestInstallShowsWhatIsLeftToDo(t *testing.T) {
	i := newInstaller(t)
	if code, out := i.run("FAKE_DOCTOR=warn"); code != 0 || !strings.Contains(out, "sbx: ACTION. sbx was not found.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// Hardware troupe cannot do without undoes the install: no troupe, or the one that was there.
func TestInstallRefusesAMachineThatCannotHostAgents(t *testing.T) {
	i := newInstaller(t)
	code, out := i.run("FAKE_DOCTOR=fail")
	if code == 0 || i.installed() != "" || !strings.Contains(out, "/dev/kvm is not usable") || !strings.Contains(out, "troupe was not installed") {
		t.Fatalf("exit %d, installed %q:\n%s", code, i.installed(), out)
	}
	if i.calls() != "" {
		t.Fatalf("units installed: %s", i.calls())
	}
	i.write(filepath.Join(i.home, ".local/bin/troupe"), "the old troupe\n", 0o755)
	if code, out := i.run("FAKE_DOCTOR=fail"); code == 0 || i.installed() != "the old troupe\n" {
		t.Fatalf("exit %d, installed %q:\n%s", code, i.installed(), out)
	}
}

func TestInstallRefusesAMachineThatIsNotX86(t *testing.T) {
	i := newInstaller(t)
	if code, out := i.run("FAKE_ARCH=aarch64"); code == 0 || i.installed() != "" || !strings.Contains(out, "x86_64") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A release download is installed only when it matches its published checksum.
func TestInstallChecksTheReleaseChecksum(t *testing.T) {
	i := newInstaller(t)
	release := t.TempDir()
	i.write(filepath.Join(release, "troupe-linux-amd64"), fakeTroupe, 0o644)
	sum := sha256.Sum256([]byte(fakeTroupe))
	env := []string{"TROUPE_BINARY=", "TROUPE_RELEASE=file://" + release}
	i.write(filepath.Join(release, "SHA256SUMS"), strings.Repeat("0", 64)+"  troupe-linux-amd64\n", 0o644)
	if code, out := i.run(env...); code == 0 || i.installed() != "" || !strings.Contains(out, "does not match the release's checksum") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	i.write(filepath.Join(release, "SHA256SUMS"), hex.EncodeToString(sum[:])+"  troupe-linux-amd64\n", 0o644)
	if code, out := i.run(env...); code != 0 || i.installed() != fakeTroupe {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// An install that fails after troupe was replaced puts the old one back, and the next run tries
// the step again rather than trusting files alone.
func TestAFailedStepPutsTheOldTroupeBackAndIsRetried(t *testing.T) {
	i := newInstaller(t)
	os.MkdirAll(filepath.Join(i.home, ".local/bin"), 0o755)
	i.write(filepath.Join(i.home, ".local/bin/troupe"), "the old troupe\n", 0o755)
	if code, out := i.run("FAKE_RENDER=fail"); code == 0 || i.installed() != "the old troupe\n" {
		t.Fatalf("render failed: exit %d, installed %q:\n%s", code, i.installed(), out)
	}
	i.write(filepath.Join(i.bin, "fail-enable"), "", 0o644)
	if code, out := i.run(); code == 0 || i.installed() != "the old troupe\n" || !strings.Contains(out, "sbx-daemon.service did not start") {
		t.Fatalf("enable failed: exit %d, installed %q:\n%s", code, i.installed(), out)
	}
	// The units are written now, but the daemon is not running: the next run enables it again.
	os.Remove(filepath.Join(i.bin, "fail-enable"))
	os.Remove(filepath.Join(i.bin, "calls"))
	if code, out := i.run(); code != 0 || i.installed() != fakeTroupe || !strings.Contains(i.calls(), "systemctl enable --now sbx-daemon.service") {
		t.Fatalf("retry: exit %d, calls %q:\n%s", code, i.calls(), out)
	}
}

func TestInstallRefusesRoot(t *testing.T) {
	i := newInstaller(t)
	if code, out := i.run("FAKE_UID=0"); code == 0 || i.installed() != "" || !strings.Contains(out, "not root") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A sandbox daemon running outside sbx-daemon.service is never met by the unit's: the install
// stops before sudo and puts the old troupe back.
func TestInstallRefusesAnUnmanagedSandboxDaemon(t *testing.T) {
	i := newInstaller(t)
	sandboxd := filepath.Join(filepath.Dir(i.home), "sandboxd")
	os.MkdirAll(sandboxd, 0o700)
	i.write(filepath.Join(sandboxd, "sandboxd.pid"), strconv.Itoa(os.Getpid())+"\n", 0o600)
	code, out := i.run()
	if code == 0 || i.installed() != "" || !strings.Contains(out, "runs outside sbx-daemon.service") || i.calls() != "" {
		t.Fatalf("exit %d, calls %q:\n%s", code, i.calls(), out)
	}
	// The same daemon run by the unit is the unit's own.
	i.write(filepath.Join(i.bin, "mainpid"), strconv.Itoa(os.Getpid())+"\n", 0o644)
	if code, out := i.run(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// Ctrl+C during the install puts the old troupe back too.
func TestAnInterruptedInstallPutsTheOldTroupeBack(t *testing.T) {
	i := newInstaller(t)
	os.MkdirAll(filepath.Join(i.home, ".local/bin"), 0o755)
	i.write(filepath.Join(i.home, ".local/bin/troupe"), "the old troupe\n", 0o755)
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(append([]string{}, i.env...), "FAKE_DOCTOR=slow")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until the new troupe is in place and the doctor runs, then interrupt.
	for start := time.Now(); i.installed() != fakeTroupe; time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the new troupe was never installed")
		}
	}
	cmd.Process.Signal(os.Interrupt)
	cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code == 0 || i.installed() != "the old troupe\n" {
		t.Fatalf("exit %d, installed %q", code, i.installed())
	}
}

// Without sbx the daemon's unit could only fail and restart forever: troupe is installed, the
// units wait, and the user is told what to install.
func TestWithoutSbxTheUnitsWait(t *testing.T) {
	i := newInstaller(t)
	code, out := i.run("TROUPE_SBX=/nonexistent/sbx", "FAKE_DOCTOR=warn")
	if code != 0 || i.installed() != fakeTroupe || i.calls() != "" || !strings.Contains(out, "install docker-sbx, then run the installer again") ||
		!strings.Contains(out, "sbx: ACTION") {
		t.Fatalf("exit %d, calls %q:\n%s", code, i.calls(), out)
	}
	if entries, _ := os.ReadDir(i.units); len(entries) != 0 {
		t.Fatalf("units written: %v", entries)
	}
}

// A daemon unit that runs but is not enabled at boot (enabled only for this boot, say) is
// enabled again: the doctor and the installer agree on "enabled".
func TestAUnitNotEnabledAtBootIsEnabledAgain(t *testing.T) {
	i := newInstaller(t)
	if code, out := i.run(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	i.write(filepath.Join(i.bin, "unitfilestate"), "enabled-runtime\n", 0o644)
	os.Remove(filepath.Join(i.bin, "calls"))
	if code, out := i.run(); code != 0 || !strings.Contains(i.calls(), "systemctl enable --now sbx-daemon.service") {
		t.Fatalf("exit %d, calls %q:\n%s", code, i.calls(), out)
	}
}
