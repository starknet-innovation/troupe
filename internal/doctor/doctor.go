// Package doctor checks whether this machine can host troupe's agents. Hardware it cannot do
// without fails; software it can install later warns. Every fact comes through Host, so tests
// run it on fake system files and programs.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/published"
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
	TPM      string // /dev/tpmrm0
	Budget   budget.Paths
	Measure  func(budget.Paths) budget.Host                    // budget.Measure; tests give fixed figures
	Run      func(name string, args ...string) (string, error) // a program's standard output
	LookPath func(name string) (string, error)
	Units    func() error // troupe's units are installed, current and running the sandbox daemon
}

// Default is this machine.
func Default(sandboxd string, units func() error) Host {
	return Host{
		Arch: runtime.GOARCH, KVM: "/dev/kvm", TPM: "/dev/tpmrm0",
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
	lines := []Line{arch(h), kvm(h), tpm(h), room(h)}
	return append(lines, systemd(h), docker(h), sbxVersion(h), sudo(h), units(h))
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

func tpm(h Host) Line {
	if _, err := os.Stat(h.TPM); err != nil {
		return Line{"tpm", Fail, h.TPM + " is missing: troupe encrypts every key with a TPM 2.0; enable it in the firmware, or on EC2 use an AMI registered with NitroTPM"}
	}
	return Line{"tpm", OK, "TPM 2.0 at " + h.TPM}
}

// room is one agent at the default size against an empty machine, with the reserves create uses:
// the machine's memory, cores and free disk, not what existing sandboxes hold or may grow (create
// weighs those, and asks). The sandbox daemon's folder may not exist yet: its nearest existing
// parent holds the disks.
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
	need := fmt.Sprintf("room for one agent (%d CPUs, %d GiB memory, %d GiB disk) beyond the reserves", agentCPUs, agentMemoryGiB, agentDiskGiB)
	switch {
	case len(result.Notes) > 0:
		return Line{"room", Fail, strings.Join(result.Notes, "; ")}
	case result.Impossible != "":
		return Line{"room", Fail, "no " + need + ": " + result.Impossible}
	case len(result.Over) > 0:
		var over []string
		for _, l := range result.Over {
			over = append(over, l.String())
		}
		return Line{"room", Fail, "no " + need + ": " + strings.Join(over, "; ")}
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
	if _, err := h.Run("systemd-creds", "--version"); err != nil {
		return Line{"systemd", Warn, "systemd-creds was not found: troupe encrypts keys with it"}
	}
	// troupe encrypts with systemd-creds --uid and decrypts with --user, both from systemd 256.
	if version < 256 {
		return Line{"systemd", Warn, fmt.Sprintf("systemd %d is older than 256: troupe's key encryption needs systemd-creds --user (256 or newer)", version)}
	}
	if version < 259 {
		return Line{"systemd", Note, fmt.Sprintf("systemd %d is older than 259, the version troupe is tested on", version)}
	}
	return Line{"systemd", OK, fmt.Sprintf("systemd %d with systemd-creds", version)}
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
