// Package budget compares what an agent would add to this machine against what the machine has,
// after a reserve for the host and each VM's own overhead. It counts every sandbox sbx runs,
// troupe's or another tool's: they share the machine.
//
// Over budget is a warning the operator confirms; only what cannot work at all (one agent larger
// than the machine) is refused. The figures are measured on the host that runs the agents:
//   - a VM's host memory (its containerd-shim-nerdbox process) peaked about 1 GiB above its guest
//     size, with the guest full during a heavy Rust build: its anonymous memory (guest RAM and the
//     VMM's own) stayed within the declared size, and the rest was the kit image's layers mapped
//     in. With the VM's sbx exec process (about 100 MiB once settled), each running VM counts
//     1.25 GiB beyond its declared memory;
//   - VM disks are sparse files that grow as the guest fills them, so a disk counts what it may
//     still grow: its declared size minus what it already holds.
package budget

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
	// OverheadMiB is each running VM's host memory beyond its declared memory.
	OverheadMiB = 1280
	// ReserveGiB is the memory kept back for the host itself, beyond sandboxd's own.
	ReserveGiB = 2
	// DiskReservePercent of the disks' filesystem stays free.
	DiskReservePercent = 10
	// CPUOvercommitPercent is how far running VMs' vCPUs may go past the machine's CPU threads.
	// VMs idle between bursts, so shared CPUs only slow a burst down; memory is never
	// overcommitted (the owner, 2026-10-28).
	CPUOvercommitPercent = 50
)

// Host is this machine's figures. Zero values mean unknown.
type Host struct {
	Memory      int64 // bytes of memory (MemTotal)
	Cores       int
	SandboxdRSS int64 // bytes the sandbox daemon itself holds
	DiskFree    int64 // bytes free on the filesystem holding the VM disks
	DiskSize    int64 // that filesystem's size
	DiskGrowth  int64 // bytes the existing VM disks may still grow
}

// Sandbox is one sandbox sbx runs, as sbx ls reports it.
type Sandbox struct {
	Name      string
	CPUs      int
	MemoryMiB int
	Running   bool
}

// Want is what an agent asks for: the sizes it would run at, and the disk it would add.
type Want struct {
	Name          string
	CPUs          int
	MemoryGiB     int
	DiskGrowthGiB int // new disk the change adds (a create: both disks; a start: none)
	// NewVM: the disks are a new VM's, already counted in the host's growth once its sandbox
	// exists (a resumed create).
	NewVM bool
	// Count is how many VMs of these sizes are wanted at once (0 means one).
	Count int
}

// Line is one resource over budget, in whole units.
type Line struct {
	Resource             string
	Committed, Available int64
	Unit                 string
}

func (l Line) String() string {
	return fmt.Sprintf("%s: %d %s committed, %d %s available, %d %s over", l.Resource, l.Committed, l.Unit,
		l.Available, l.Unit, l.Committed-l.Available, l.Unit)
}

// Result is a check's outcome: Impossible (a refusal no confirmation overrides), the resources
// over budget, and notes on figures that could not be read.
type Result struct {
	Impossible string
	Over       []Line
	Notes      []string
}

// Check counts the running sandboxes other than the wanted agent, plus the agent at its wanted
// size (Count of them), against the host. One VM larger than the machine is impossible; more
// VMs than fit are only over budget. Memory is counted in MiB, so a margin under a GiB is not
// rounded away.
func Check(h Host, sandboxes []Sandbox, w Want) Result {
	var r Result
	count := int64(max(w.Count, 1))
	if h.Memory == 0 || h.Cores == 0 {
		r.Notes = append(r.Notes, "this machine's memory or cores could not be read, so they were not checked")
	} else {
		memory := (h.Memory-h.SandboxdRSS)/mib - ReserveGiB*1024
		wanted := int64(w.MemoryGiB)*1024 + OverheadMiB
		if wanted > memory {
			r.Impossible = fmt.Sprintf("%s alone needs %d GiB of memory with its VM's overhead; this machine has %d GiB for agents",
				w.Name, ceilGiB(wanted), memory/1024)
			return r
		}
		if w.CPUs > h.Cores {
			r.Impossible = fmt.Sprintf("%s alone asks for %d CPUs; this machine has %d", w.Name, w.CPUs, h.Cores)
			return r
		}
		usedMemory, usedCPUs := count*wanted, count*int64(w.CPUs)
		for _, s := range sandboxes {
			if s.Name == w.Name {
				continue
			}
			if s.Running {
				usedMemory += int64(s.MemoryMiB) + OverheadMiB
				usedCPUs += int64(s.CPUs)
			}
		}
		if usedMemory > memory {
			r.Over = append(r.Over, Line{"Memory", ceilGiB(usedMemory), memory / 1024, "GiB"})
		}
		if cpus := int64(h.Cores) * (100 + CPUOvercommitPercent) / 100; usedCPUs > cpus {
			r.Over = append(r.Over, Line{"CPUs", usedCPUs, cpus, "vCPUs"})
		}
	}
	if h.DiskSize == 0 {
		r.Notes = append(r.Notes, "the VM disks' free space could not be read, so disk was not checked")
	} else {
		disks := count * int64(w.DiskGrowthGiB)
		for _, s := range sandboxes {
			if s.Name == w.Name && w.NewVM {
				disks -= int64(w.DiskGrowthGiB) // its disks exist already: the host's growth counts them
			}
		}
		available := (h.DiskFree - h.DiskSize*DiskReservePercent/100) / gib
		growth := h.DiskGrowth/gib + disks
		if growth > available {
			r.Over = append(r.Over, Line{"Disk", growth, available, "GiB"})
		}
	}
	return r
}

// Room is the most one agent may take: the largest sizes Check passes without asking. A figure
// this machine's could not be read for is -1, never a guess.
type Room struct {
	CPUs      int
	MemoryGiB int // whole GiB of guest memory, after this VM's own overhead
	DiskGiB   int // whole GiB of disk the agent may add, for both its disks together
}

// Max is the room for name on this machine: the reserves Check keeps, less every other running
// sandbox. CPUs are all the machine's threads unless other VMs would pass the overcommit: CPU is
// time-shared, so the host is still scheduled; memory is the hard limit.
func Max(h Host, sandboxes []Sandbox, name string) Room {
	room := Room{CPUs: -1, MemoryGiB: -1, DiskGiB: -1}
	if h.Memory > 0 && h.Cores > 0 {
		memory := (h.Memory-h.SandboxdRSS)/mib - ReserveGiB*1024 - OverheadMiB
		cpus := int64(h.Cores) * (100 + CPUOvercommitPercent) / 100
		for _, s := range sandboxes {
			if s.Name != name && s.Running {
				memory -= int64(s.MemoryMiB) + OverheadMiB
				cpus -= int64(s.CPUs)
			}
		}
		room.MemoryGiB = int(max(memory, 0) / 1024)
		room.CPUs = int(max(min(cpus, int64(h.Cores)), 0))
	}
	if h.DiskSize > 0 {
		available := (h.DiskFree-h.DiskSize*DiskReservePercent/100)/gib - h.DiskGrowth/gib
		room.DiskGiB = int(max(available, 0))
	}
	return room
}

// ceilGiB rounds MiB up to whole GiB, so a committed figure over budget never prints as equal.
func ceilGiB(m int64) int64 { return (m + 1023) / 1024 }

// Paths are where Measure reads the host's figures; tests point them at fakes.
type Paths struct {
	Meminfo  string // /proc/meminfo
	Proc     string // /proc
	Sandboxd string // the sandbox daemon's state folder (its pidfile, and containerd's disks below)
}

// Measure reads this machine's figures; a figure it cannot read stays zero.
func Measure(p Paths) Host {
	h := Host{Cores: runtime.NumCPU()}
	h.Memory = meminfo(p.Meminfo, "MemTotal")
	if pid, err := os.ReadFile(filepath.Join(p.Sandboxd, "sandboxd.pid")); err == nil {
		h.SandboxdRSS = meminfo(filepath.Join(p.Proc, strings.TrimSpace(string(pid)), "status"), "VmRSS")
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(p.Sandboxd, &fs) == nil {
		h.DiskFree = int64(fs.Bavail) * int64(fs.Bsize)
		h.DiskSize = int64(fs.Blocks) * int64(fs.Bsize)
	}
	h.DiskGrowth = growth(p.Sandboxd)
	return h
}

// meminfo reads one "Key: N kB" line of a /proc file, in bytes.
func meminfo(path, key string) int64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == key+":" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return n * 1024
		}
	}
	return 0
}

// diskGlobs are where sandboxd keeps each VM's root disk and Docker disk (sbx 0.48).
var diskGlobs = []string{
	"containerd/root/io.containerd.snapshotter.v1.erofs/snapshots/*/rwlayer.img",
	"containerd/root/com.docker.volume.driver.v1.block/images/*.img",
}

// growth is how much the existing VM disks may still grow: each sparse file's size minus the
// blocks it holds.
func growth(sandboxd string) int64 {
	var total int64
	for _, pattern := range diskGlobs {
		matches, _ := filepath.Glob(filepath.Join(sandboxd, pattern))
		for _, path := range matches {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			if room := info.Size() - st.Blocks*512; room > 0 {
				total += room
			}
		}
	}
	return total
}

// ErrDeclined is the operator answering no; ErrOver, a check that only reports.
var (
	ErrDeclined = errors.New("over_budget; not confirmed (--force confirms)")
	ErrOver     = errors.New("over_budget; the run would ask to confirm (--force confirms)")
)
