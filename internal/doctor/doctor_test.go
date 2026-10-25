package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/budget"
)

const gib = int64(1) << 30

// ready is a machine that passes every check: fake device files, a 32 GiB, 8 core machine
// with 500 GiB free, and programs that answer like this host's.
func ready(t *testing.T) Host {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kvm"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	answers := map[string]string{
		"systemctl --version":                         "systemd 259 (259.5-0ubuntu3.4)\n+PAM\n",
		"systemd-creds --version":                     "systemd 259 (259.5-0ubuntu3.4)\n",
		"systemd-analyze has-tpm2 --quiet":            "",
		"docker version --format {{.Server.Version}}": "29.9.0\n",
		"docker buildx version":                       "github.com/docker/buildx v0.38.0\n",
		"dpkg-query -W -f=${Version} docker-sbx":      "0.48.0-1~ubuntu.26.04~resolute",
	}
	return Host{
		Arch: "amd64", KVM: filepath.Join(dir, "kvm"),
		Budget: budget.Paths{Sandboxd: filepath.Join(dir, "missing/sandboxd")},
		Measure: func(budget.Paths) budget.Host {
			return budget.Host{Memory: 32 * gib, Cores: 8, DiskFree: 500 * gib, DiskSize: 1000 * gib}
		},
		Run: func(name string, args ...string) (string, error) {
			out, ok := answers[strings.Join(append([]string{name}, args...), " ")]
			if !ok {
				return "", errors.New("not found")
			}
			return out, nil
		},
		LookPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
		Units:    func() error { return nil },
	}
}

func levels(lines []Line) map[string]Level {
	m := map[string]Level{}
	for _, l := range lines {
		m[l.Name] = l.Level
	}
	return m
}

func TestAReadyMachinePassesEveryCheck(t *testing.T) {
	lines := Check(ready(t))
	for _, l := range lines {
		if l.Level != OK {
			t.Errorf("%s: %d %s", l.Name, l.Level, l.Message)
		}
	}
	if len(lines) != 9 || Failed(lines) {
		t.Fatalf("lines %v", lines)
	}
}

// Each piece of hardware troupe cannot do without fails the check.
func TestMissingHardwareFails(t *testing.T) {
	for name, c := range map[string]struct {
		line   string
		break_ func(*Host)
		want   string
	}{
		"an arm CPU":        {"cpu", func(h *Host) { h.Arch = "arm64" }, "x86_64"},
		"no KVM":            {"kvm", func(h *Host) { os.Remove(h.KVM) }, "virtualization"},
		"KVM not yours":     {"kvm", func(h *Host) { os.Chmod(h.KVM, 0) }, "kvm group"},
		"unreadable memory": {"room", func(h *Host) { h.Measure = fixed(budget.Host{Cores: 8, DiskFree: 500 * gib, DiskSize: 1000 * gib}) }, "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "KVM not yours" && os.Geteuid() == 0 {
				t.Skip("root opens any file")
			}
			h := ready(t)
			c.break_(&h)
			lines := Check(h)
			var got Line
			for _, l := range lines {
				if l.Name == c.line {
					got = l
				}
			}
			if got.Level != Fail || !strings.Contains(got.Message, c.want) || !Failed(lines) {
				t.Fatalf("%+v", got)
			}
			for other, level := range levels(lines) {
				if other != c.line && level != OK {
					t.Errorf("%s also %d", other, level)
				}
			}
		})
	}
}

// A machine too small for a coder agent at its default size is a note, never a failure: create
// checks each agent's own sizes, so a smaller agent may still fit (4 and 5 CPUs are below the
// coder default of 6 and still hold a 4 CPU agent).
func TestNoRoomForADefaultCoderIsANote(t *testing.T) {
	for name, c := range map[string]struct {
		host budget.Host
		want string
	}{
		"too little memory": {budget.Host{Memory: 8 * gib, Cores: 8, DiskFree: 500 * gib, DiskSize: 1000 * gib}, "memory"},
		"two CPUs":          {budget.Host{Memory: 32 * gib, Cores: 2, DiskFree: 500 * gib, DiskSize: 1000 * gib}, "CPUs"},
		"four CPUs":         {budget.Host{Memory: 32 * gib, Cores: 4, DiskFree: 500 * gib, DiskSize: 1000 * gib}, "CPUs"},
		"five CPUs":         {budget.Host{Memory: 32 * gib, Cores: 5, DiskFree: 500 * gib, DiskSize: 1000 * gib}, "CPUs"},
		"too little disk":   {budget.Host{Memory: 32 * gib, Cores: 8, DiskFree: 100 * gib, DiskSize: 1000 * gib}, "Disk"},
	} {
		t.Run(name, func(t *testing.T) {
			h := ready(t)
			h.Measure = fixed(c.host)
			lines := Check(h)
			var got Line
			for _, l := range lines {
				if l.Name == "room" {
					got = l
				}
			}
			if got.Level != Note || !strings.Contains(got.Message, c.want) || !strings.Contains(got.Message, "own sizes") || Failed(lines) {
				t.Fatalf("%+v", got)
			}
		})
	}
	h := ready(t)
	h.Measure = fixed(budget.Host{Memory: 32 * gib, Cores: 6, DiskFree: 500 * gib, DiskSize: 1000 * gib})
	if got := levels(Check(h)); got["room"] != OK {
		t.Fatalf("six CPUs hold the coder default: %v", got)
	}
}

// Software can be installed later: each missing piece asks for action and never fails.
func TestMissingSoftwareWarns(t *testing.T) {
	for name, c := range map[string]struct {
		line   string
		answer string // the program call that answers differently
		out    string // "" means it fails
	}{
		"no systemd":     {"systemd", "systemctl --version", ""},
		"no Docker":      {"docker", "docker version --format {{.Server.Version}}", ""},
		"no buildx":      {"docker", "docker buildx version", ""},
		"no sbx package": {"sbx", "dpkg-query -W -f=${Version} docker-sbx", ""},
		"old sbx":        {"sbx", "dpkg-query -W -f=${Version} docker-sbx", "0.47.2-1~ubuntu.26.04"},
	} {
		t.Run(name, func(t *testing.T) {
			h := ready(t)
			run := h.Run
			h.Run = func(program string, args ...string) (string, error) {
				if strings.Join(append([]string{program}, args...), " ") == c.answer {
					if c.out == "" {
						return "", errors.New("not found")
					}
					return c.out, nil
				}
				return run(program, args...)
			}
			lines := Check(h)
			if got := levels(lines); got[c.line] != Warn || Failed(lines) {
				t.Fatalf("%v", lines)
			}
		})
	}
	h := ready(t)
	h.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if got := levels(Check(h)); got["sudo"] != Warn || got["sbx"] != Warn {
		t.Fatalf("no sudo, no sbx: %v", got)
	}
	h = ready(t)
	h.Units = func() error { return errors.New("sbx-daemon.service is not installed: run the installer again") }
	if lines := Check(h); levels(lines)["units"] != Warn || !strings.Contains(lines[8].Message, "run the installer again") {
		t.Fatalf("units: %v", lines)
	}
}

// Keys below the TPM are a note naming what they get, never in the way: create asks instead.
func TestKeysLineNamesTheTier(t *testing.T) {
	for name, c := range map[string]struct {
		answer, out string // the program call that answers differently; "" out means it fails
		level       Level
		want        string
	}{
		"a TPM":               {"", "", OK, "sealed by this machine's TPM"},
		"no TPM":              {"systemd-analyze has-tpm2 --quiet", "", Note, "host key only"},
		"no systemd-creds":    {"systemd-creds --version", "", Note, "plain files"},
		"systemd before 256":  {"systemctl --version", "systemd 255 (255.4-1ubuntu8)\n", Note, "plain files"},
		"has-tpm2 before 257": {"systemd-analyze has-tpm2 --quiet", "fallback", OK, "sealed by this machine's TPM"},
	} {
		t.Run(name, func(t *testing.T) {
			h := ready(t)
			run := h.Run
			h.Run = func(program string, args ...string) (string, error) {
				call := strings.Join(append([]string{program}, args...), " ")
				if c.out == "fallback" {
					// systemd 256: only systemd-creds has the verb.
					if call == "systemd-creds has-tpm2 --quiet" {
						return "", nil
					}
					if call == c.answer {
						return "", errors.New("unknown verb")
					}
				} else if call == c.answer {
					if c.out == "" {
						return "", errors.New("not found")
					}
					return c.out, nil
				}
				return run(program, args...)
			}
			lines := Check(h)
			var got Line
			for _, l := range lines {
				if l.Name == "keys" {
					got = l
				}
			}
			if got.Level != c.level || !strings.Contains(got.Message, c.want) || Failed(lines) {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func fixed(h budget.Host) func(budget.Paths) budget.Host {
	return func(budget.Paths) budget.Host { return h }
}

// An older systemd is worth knowing, but troupe may still work on it: a note, never in the way.
func TestAnOlderSystemdIsANote(t *testing.T) {
	h := ready(t)
	run := h.Run
	h.Run = func(program string, args ...string) (string, error) {
		if program == "systemctl" {
			return "systemd 257 (257.4-1ubuntu3)\n", nil
		}
		return run(program, args...)
	}
	if got := levels(Check(h)); got["systemd"] != Note {
		t.Fatalf("%v", got)
	}
}

// The room check is an empty machine's: what existing sandboxes hold or may still grow is
// create's to weigh. It measures the nearest folder that exists when the daemon's does not yet.
func TestRoomIsAnEmptyMachines(t *testing.T) {
	h := ready(t)
	var measured string
	h.Measure = func(p budget.Paths) budget.Host {
		measured = p.Sandboxd
		return budget.Host{Memory: 32 * gib, Cores: 8, DiskFree: 500 * gib, DiskSize: 1000 * gib, DiskGrowth: 800 * gib, SandboxdRSS: 20 * gib}
	}
	if got := levels(Check(h)); got["room"] != OK {
		t.Fatalf("%v", got)
	}
	if want := filepath.Dir(filepath.Dir(h.Budget.Sandboxd)); measured != want {
		t.Fatalf("measured %s, want %s", measured, want)
	}
}

// The doctor never runs sbx: before the sandboxd probe, an sbx command can start a daemon of its
// own, outside troupe's unit.
func TestTheDoctorNeverRunsSbx(t *testing.T) {
	h := ready(t)
	run := h.Run
	h.Run = func(program string, args ...string) (string, error) {
		if program == "sbx" {
			t.Fatalf("ran sbx %v", args)
		}
		return run(program, args...)
	}
	Check(h)
}
