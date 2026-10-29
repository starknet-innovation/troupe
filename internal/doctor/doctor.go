// Package doctor checks whether this machine can host troupe's agents. Hardware it cannot do
// without fails; software it can install later warns. Every fact comes through Host, so tests
// run it on fake system files and programs.
package doctor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/published"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// Level is how a requirement stands.
type Level int

const (
	OK   Level = iota
	Note       // worth knowing, never in the way
	Warn       // asks for the user's action
	Fail
)

// Line is one requirement's result: what was checked, how it stands, and what to do.
type Line struct {
	Name    string
	Level   Level
	Message string
}

// Host is where the checks read this machine.
type Host struct {
	Arch     string // runtime.GOARCH
	KVM      string // /dev/kvm
	Budget   budget.Paths
	Measure  func(budget.Paths) budget.Host                    // budget.Measure; tests give fixed figures
	Run      func(name string, args ...string) (string, error) // a program's standard output
	LookPath func(name string) (string, error)
	Units    func() error // troupe's units are installed, current and running the sandbox daemon
	SignAuth string       // sbx's sign-in store, a folder
}

// Default is this machine.
func Default(sandboxd string, units func() error) Host {
	config, _ := os.UserConfigDir()
	return Host{
		Arch: runtime.GOARCH, KVM: "/dev/kvm",
		Budget:  budget.Paths{Meminfo: "/proc/meminfo", Proc: "/proc", Sandboxd: sandboxd},
		Measure: budget.Measure,
		Run: func(name string, args ...string) (string, error) {
			// Every call has a deadline: a hung program never hangs create.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return string(out), err
		},
		LookPath: exec.LookPath,
		Units:    units,
		SignAuth: filepath.Join(config, "com.docker.sandboxes", "com.docker.sandboxes-auth", "sandboxes-auth"),
	}
}

// One agent at the coder type's default size: what a machine must hold beyond its reserves.
var (
	coder          = published.Coder().Defaults
	agentCPUs      = coder.CPUs
	agentMemoryGiB = coder.MemoryGiB
	agentDiskGiB   = coder.RootGiB + coder.DockerGiB
)

// Check runs every check, hardware first.
func Check(h Host) []Line {
	lines := []Line{arch(h), kvm(h), keys(h), room(h)}
	return append(lines, systemd(h), docker(h), sbxVersion(h), signIn(h), sudo(h), units(h))
}

// Failed reports whether any line failed.
func Failed(lines []Line) bool {
	for _, l := range lines {
		if l.Level == Fail {
			return true
		}
	}
	return false
}

func arch(h Host) Line {
	if h.Arch != "amd64" {
		return Line{"cpu", Fail, fmt.Sprintf("this machine is %s; troupe's agents need an x86_64 (amd64) CPU", h.Arch)}
	}
	return Line{"cpu", OK, "x86_64"}
}

func kvm(h Host) Line {
	file, err := os.OpenFile(h.KVM, os.O_RDWR, 0)
	if err != nil {
		fix := "enable virtualization (VT-x) in the firmware, or on a cloud VM choose an instance with nested virtualization"
		if errors.Is(err, os.ErrPermission) {
			fix = "add your user to the kvm group (sudo usermod -aG kvm $USER), then log in again"
		}
		return Line{"kvm", Fail, fmt.Sprintf("%s is not usable (%v): %s", h.KVM, plain(err), fix)}
	}
	file.Close()
	return Line{"kvm", OK, h.KVM + " is usable"}
}

// keys is the strongest protection this machine gives agents' keys. Anything below the TPM is
// a note, never in the way: create states it and asks before it stores a key.
func keys(h Host) Line {
	tier := secret.Detect(h.Run)
	if tier == secret.TPM {
		return Line{"keys", OK, "TPM 2.0: " + tier.Describe()}
	}
	if missing := tpmLibraries(h); tier == secret.Host && missing != "" {
		return Line{"keys", Note, tier.Describe() + "; create asks before using it. This machine has a TPM 2.0, but systemd lacks its libraries (" +
			missing + "): install them (on Ubuntu, the libtss2 packages, such as libtss2-rc0t64), and keys are sealed by it"}
	}
	return Line{"keys", Note, tier.Describe() + "; create asks before using it. TPM sealing needs " + tier.Upgrade()}
}

// tpmLibraries names the libraries systemd lacks when they are all that stands between it and a
// working TPM, or is "" otherwise. Ubuntu 26.04's cloud image ships without libtss2-rc, so an EC2
// instance with NitroTPM reads as having none (2026-10-29). has-tpm2 lists each part as + or -.
func tpmLibraries(h Host) string {
	out, _ := h.Run("systemd-analyze", "has-tpm2")
	parts := strings.Fields(out)
	for _, want := range []string{"+firmware", "+driver", "+system", "+subsystem", "-libraries"} {
		if !slices.Contains(parts, want) {
			return ""
		}
	}
	var missing []string
	for _, p := range parts {
		if strings.HasPrefix(p, "-lib") && p != "-libraries" {
			missing = append(missing, strings.TrimPrefix(p, "-"))
		}
	}
	if len(missing) == 0 {
		return "unnamed"
	}
	return strings.Join(missing, ", ")
}

// room is one coder agent at its default size against an empty machine, with the reserves create
// uses: the machine's memory, cores and free disk, not what existing sandboxes hold or may grow
// (create weighs those, and asks). Not fitting is a note: create checks each agent's own sizes, so
// a smaller agent may still fit. Only a machine that cannot be measured fails. The sandbox
// daemon's folder may not exist yet: its nearest existing parent holds the disks.
func room(h Host) Line {
	paths := h.Budget
	for dir := paths.Sandboxd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); err == nil {
			paths.Sandboxd = dir
			break
		}
	}
	host := h.Measure(paths)
	host.SandboxdRSS, host.DiskGrowth = 0, 0
	result := budget.Check(host, nil, budget.Want{Name: "one agent", CPUs: agentCPUs,
		MemoryGiB: agentMemoryGiB, DiskGrowthGiB: agentDiskGiB})
	need := fmt.Sprintf("room for a coder agent at its default size (%d CPUs, %d GiB memory, %d GiB disk) beyond the reserves", agentCPUs, agentMemoryGiB, agentDiskGiB)
	smaller := "; create checks each agent's own sizes, so a smaller one (--cpu, --memory, --storage) may still fit"
	switch {
	case len(result.Notes) > 0:
		return Line{"room", Fail, strings.Join(result.Notes, "; ")}
	case result.Impossible != "":
		return Line{"room", Note, "no " + need + ": " + result.Impossible + smaller}
	case len(result.Over) > 0:
		var over []string
		for _, l := range result.Over {
			over = append(over, l.String())
		}
		return Line{"room", Note, "no " + need + ": " + strings.Join(over, "; ") + smaller}
	}
	return Line{"room", OK, need}
}

var firstNumber = regexp.MustCompile(`[0-9]+`)

func systemd(h Host) Line {
	out, err := h.Run("systemctl", "--version")
	version, _ := strconv.Atoi(firstNumber.FindString(out))
	if err != nil || version == 0 {
		return Line{"systemd", Warn, "systemd was not found: troupe runs each agent's gateway as a systemd unit"}
	}
	// The keys line says what an older systemd, or one without systemd-creds, means for keys.
	if version < 259 {
		return Line{"systemd", Note, fmt.Sprintf("systemd %d is older than 259, the version troupe is tested on", version)}
	}
	return Line{"systemd", OK, fmt.Sprintf("systemd %d", version)}
}

func docker(h Host) Line {
	if _, err := h.Run("docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return Line{"docker", Warn, "Docker does not answer: install Docker Engine and make sure your user can run docker"}
	}
	if _, err := h.Run("docker", "buildx", "version"); err != nil {
		return Line{"docker", Warn, "Docker has no buildx: install the docker-buildx plugin"}
	}
	return Line{"docker", OK, "Docker with buildx"}
}

var sbxVersionPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)`)

// sbxVersion reads docker-sbx's package version. It never runs sbx: an sbx command before the
// sandboxd probe can start a daemon outside troupe's unit.
func sbxVersion(h Host) Line {
	if _, err := h.LookPath("sbx"); err != nil {
		return Line{"sbx", Warn, "Docker Sandboxes (sbx) was not found: install docker-sbx 0.48 or newer from Docker's apt repository"}
	}
	out, err := h.Run("dpkg-query", "-W", "-f=${Version}", "docker-sbx")
	m := sbxVersionPattern.FindStringSubmatch(strings.TrimSpace(out))
	if err != nil || m == nil {
		return Line{"sbx", Warn, "sbx's version could not be read from its docker-sbx package: install docker-sbx 0.48 or newer from Docker's apt repository"}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major == 0 && minor < 48 {
		return Line{"sbx", Warn, fmt.Sprintf("sbx %s.%s.%s is older than 0.48: upgrade docker-sbx", m[1], m[2], m[3])}
	}
	return Line{"sbx", OK, fmt.Sprintf("sbx %s.%s.%s", m[1], m[2], m[3])}
}

// signIn is sbx's Docker sign-in: signed out, sbx refuses every sandbox command and sandboxd
// answers the probe with a 401, so troupe's daemon unit cannot start (sbx 0.50.1). It reads
// sbx's sign-in store, never sbx: each entry is named by its key in base64, and a signed-in sbx
// holds docker/auth/hub/ACCOUNT. A store sbx lays out differently reads as signed out, and the
// probe's 401 stays the backstop.
func signIn(h Host) Line {
	entries, _ := os.ReadDir(h.SignAuth)
	for _, e := range entries {
		key, err := base64.StdEncoding.DecodeString(e.Name())
		if err == nil && strings.HasPrefix(string(key), "docker/auth/hub/") {
			return Line{"sign-in", OK, "sbx is signed in to Docker"}
		}
	}
	// sbx keeps its secrets in a running desktop keyring (Secret Service) instead, by its own
	// strings (unmeasured): there troupe cannot tell, so it is a note, never in the way.
	if out, err := h.Run("busctl", "--user", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus",
		"org.freedesktop.DBus", "NameHasOwner", "s", "org.freedesktop.secrets"); err == nil && strings.TrimSpace(out) == "b true" {
		return Line{"sign-in", Note, "sbx's Docker sign-in is not in its folder, and this desktop runs a keyring, where sbx may keep it; " +
			"if sbx is signed out, the sandbox daemon's start says so (sbx login)"}
	}
	return Line{"sign-in", Warn, "sbx is not signed in to Docker: run sbx login"}
}

func sudo(h Host) Line {
	if _, err := h.LookPath("sudo"); err != nil {
		return Line{"sudo", Warn, "sudo was not found: troupe asks for it to manage agents' units and keys"}
	}
	return Line{"sudo", OK, "sudo"}
}

func units(h Host) Line {
	if h.Units == nil {
		return Line{"units", Warn, "troupe's systemd units could not be checked"}
	}
	if err := h.Units(); err != nil {
		return Line{"units", Warn, err.Error()} // each names its own fix
	}
	return Line{"units", OK, "troupe's systemd units are installed and current, and run the sandbox daemon"}
}

// plain is an error without its path, which the line already names.
func plain(err error) string {
	var p *os.PathError
	if errors.As(err, &p) {
		return p.Err.Error()
	}
	return err.Error()
}
