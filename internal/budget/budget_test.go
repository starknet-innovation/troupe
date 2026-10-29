package budget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// host is a 30 GiB, 16 core machine whose sandbox daemon holds 1 GiB, with 1000 GiB free on a
// 2000 GiB disk and no VM disk growth: 27 GiB of memory, 24 vCPUs (16
// cores and half again) and 800 GiB of disk for agents.
func host() Host {
	return Host{Memory: 30 * gib, Cores: 16, SandboxdRSS: 1 * gib, DiskFree: 1000 * gib, DiskSize: 2000 * gib}
}

var other = Sandbox{Name: "other", CPUs: 6, MemoryMiB: 10240, Running: true}

func TestAnAgentThatFitsPasses(t *testing.T) {
	r := Check(host(), []Sandbox{other}, Want{Name: "jane", CPUs: 4, MemoryGiB: 8, DiskGrowthGiB: 120})
	if r.Impossible != "" || len(r.Over) != 0 || len(r.Notes) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestEachResourceOverBudgetIsReported(t *testing.T) {
	cases := map[string]struct {
		h      func(Host) Host
		others []Sandbox
		want   Want
		line   string
	}{
		// 12+1.25 + 10+1.25 = 24.5 fits 27; another 12+1.25 does not.
		"memory": {func(h Host) Host { return h }, []Sandbox{other, {Name: "grace", CPUs: 4, MemoryMiB: 12288, Running: true}},
			Want{Name: "ada", CPUs: 4, MemoryGiB: 12}, "Memory: 38 GiB committed, 27 GiB available, 11 GiB over"},
		"cpus": {func(h Host) Host { h.Memory = 300 * gib; return h }, []Sandbox{{Name: "a", CPUs: 16, MemoryMiB: 1024, Running: true}, {Name: "b", CPUs: 6, MemoryMiB: 1024, Running: true}},
			Want{Name: "jane", CPUs: 4, MemoryGiB: 1}, "CPUs: 26 vCPUs committed, 24 vCPUs available, 2 vCPUs over"},
		"disk": {func(h Host) Host { h.DiskGrowth = 750 * gib; return h }, nil,
			Want{Name: "jane", CPUs: 4, MemoryGiB: 8, DiskGrowthGiB: 120}, "Disk: 870 GiB committed, 800 GiB available, 70 GiB over"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := Check(c.h(host()), c.others, c.want)
			if r.Impossible != "" || len(r.Over) != 1 || r.Over[0].String() != c.line {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestStoppedSandboxesAndTheAgentItselfAreNotCountedTwice(t *testing.T) {
	stopped := Sandbox{Name: "grace", CPUs: 8, MemoryMiB: 12288}
	running := Sandbox{Name: "ada", CPUs: 8, MemoryMiB: 12288, Running: true}
	// ada already runs: starting it again counts it once, at its size.
	if r := Check(host(), []Sandbox{other, stopped, running}, Want{Name: "ada", CPUs: 8, MemoryGiB: 12}); len(r.Over) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestOneAgentLargerThanTheMachineIsImpossible(t *testing.T) {
	for name, want := range map[string]Want{
		"memory": {Name: "jane", CPUs: 4, MemoryGiB: 26},
		"cpus":   {Name: "jane", CPUs: 17, MemoryGiB: 4},
	} {
		t.Run(name, func(t *testing.T) {
			if r := Check(host(), nil, want); r.Impossible == "" {
				t.Fatalf("%+v", r)
			}
		})
	}
	if r := Check(host(), nil, Want{Name: "jane", CPUs: 16, MemoryGiB: 25}); r.Impossible != "" {
		t.Fatalf("the largest agent that fits was refused: %s", r.Impossible)
	}
}

func TestUnreadableFiguresAreNotedNotRefused(t *testing.T) {
	r := Check(Host{}, []Sandbox{other}, Want{Name: "jane", CPUs: 400, MemoryGiB: 400, DiskGrowthGiB: 1 << 20})
	if r.Impossible != "" || len(r.Over) != 0 || len(r.Notes) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestMeasureReadsTheHost(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
	}
	write("meminfo", "MemTotal:       31457280 kB\nMemFree:  1 kB\n")
	write("sandboxd/sandboxd.pid", "4242\n")
	write("proc/4242/status", "Name: sandboxd\nVmRSS:\t  1048576 kB\n")
	// A sparse 3 GiB root disk holding one 4 KiB block, and a sparse 1 GiB Docker disk.
	for _, name := range []string{"sandboxd/containerd/root/io.containerd.snapshotter.v1.erofs/snapshots/7/rwlayer.img",
		"sandboxd/containerd/root/com.docker.volume.driver.v1.block/images/jane-docker.img"} {
		write(name, "")
	}
	root := filepath.Join(dir, "sandboxd/containerd/root/io.containerd.snapshotter.v1.erofs/snapshots/7/rwlayer.img")
	os.WriteFile(root, []byte(strings.Repeat("x", 4096)), 0o644)
	os.Truncate(root, 3*gib)
	os.Truncate(filepath.Join(dir, "sandboxd/containerd/root/com.docker.volume.driver.v1.block/images/jane-docker.img"), gib)
	h := Measure(Paths{Meminfo: filepath.Join(dir, "meminfo"), Proc: filepath.Join(dir, "proc"), Sandboxd: filepath.Join(dir, "sandboxd")})
	if h.Memory != 30*gib || h.SandboxdRSS != gib || h.Cores < 1 || h.DiskSize == 0 {
		t.Fatalf("%+v", h)
	}
	if h.DiskGrowth < 4*gib-64*1024 || h.DiskGrowth > 4*gib {
		t.Fatalf("growth %d", h.DiskGrowth)
	}
}

// A margin under a GiB is not rounded away: 27.5 GiB for agents holds 27.5 GiB of VMs, not 28.
func TestMemoryIsCountedInMiB(t *testing.T) {
	h := host()
	h.SandboxdRSS = gib / 2
	running := []Sandbox{{Name: "a", CPUs: 1, MemoryMiB: 15 * 1024, Running: true}}
	if r := Check(h, running, Want{Name: "b", CPUs: 1, MemoryGiB: 10}); len(r.Over) != 0 {
		t.Fatalf("%+v", r)
	}
	running[0].MemoryMiB++
	if r := Check(h, running, Want{Name: "b", CPUs: 1, MemoryGiB: 10}); len(r.Over) != 1 || r.Over[0].Committed != 28 {
		t.Fatalf("%+v", r)
	}
}

// A resumed create whose VM exists already counts its disks once, in the host's growth.
func TestAResumedCreatesDisksAreNotCountedTwice(t *testing.T) {
	h := host()
	h.DiskGrowth = 750 * gib
	want := Want{Name: "jane", CPUs: 4, MemoryGiB: 8, DiskGrowthGiB: 120, NewVM: true}
	if r := Check(h, nil, want); len(r.Over) != 1 {
		t.Fatalf("a new VM's disks: %+v", r)
	}
	if r := Check(h, []Sandbox{{Name: "jane"}}, want); len(r.Over) != 0 {
		t.Fatalf("an existing VM's disks counted again: %+v", r)
	}
	// Of three wanted, only the existing one's disks are already counted: two more are new.
	want.Count, want.MemoryGiB = 3, 1
	if r := Check(h, []Sandbox{{Name: "jane"}}, want); len(r.Over) != 1 || r.Over[0].String() != "Disk: 990 GiB committed, 800 GiB available, 190 GiB over" {
		t.Fatalf("the other new VMs' disks: %+v", r)
	}
}

// Count VMs of one size are counted Count times; more than fit is over budget, never impossible.
func TestCountCountsEachVM(t *testing.T) {
	// 27 GiB beside nothing: two of 12+1.25 fit (26.5), three (39.75) do not.
	two := Want{Name: "lab", CPUs: 4, MemoryGiB: 12, DiskGrowthGiB: 60, Count: 2}
	if r := Check(host(), nil, two); r.Impossible != "" || len(r.Over) != 0 {
		t.Fatalf("two: %+v", r)
	}
	three := two
	three.Count = 3
	r := Check(host(), nil, three)
	if r.Impossible != "" || len(r.Over) != 1 || r.Over[0].String() != "Memory: 40 GiB committed, 27 GiB available, 13 GiB over" {
		t.Fatalf("three: %+v", r)
	}
	// CPUs and disk too: five of 3 vCPUs and 200 GiB are 15 vCPUs (fits 24) and 1000 GiB (over 800).
	five := Want{Name: "lab", CPUs: 3, MemoryGiB: 1, DiskGrowthGiB: 200, Count: 5}
	r = Check(host(), nil, five)
	if len(r.Over) != 1 || r.Over[0].String() != "Disk: 1000 GiB committed, 800 GiB available, 200 GiB over" {
		t.Fatalf("five: %+v", r)
	}
	if r := Check(host(), nil, Want{Name: "lab", CPUs: 3, MemoryGiB: 1, Count: 9}); len(r.Over) != 1 || r.Over[0].String() != "CPUs: 27 vCPUs committed, 24 vCPUs available, 3 vCPUs over" {
		t.Fatalf("nine: %+v", r)
	}
	// Exactly the cores and half again fit; one vCPU more does not.
	if r := Check(host(), nil, Want{Name: "lab", CPUs: 4, MemoryGiB: 1, Count: 6}); len(r.Over) != 0 {
		t.Fatalf("all the vCPUs: %+v", r)
	}
	if r := Check(host(), []Sandbox{{Name: "a", CPUs: 1, MemoryMiB: 1024, Running: true}}, Want{Name: "lab", CPUs: 4, MemoryGiB: 1, Count: 6}); len(r.Over) != 1 || r.Over[0].String() != "CPUs: 25 vCPUs committed, 24 vCPUs available, 1 vCPUs over" {
		t.Fatalf("one vCPU more: %+v", r)
	}
}

// Max is the largest agent Check passes without asking. On host(): 27 GiB for agents, less the
// VM's own 1.25 GiB, is 25 GiB; all 16 cores; 800 GiB of disk.
func TestMaxIsTheLargestAgentCheckPasses(t *testing.T) {
	room := Max(host(), nil, "jane")
	if room != (Room{CPUs: 16, MemoryGiB: 25, DiskGiB: 800}) {
		t.Fatalf("empty machine: %+v", room)
	}
	r := Check(host(), nil, Want{Name: "jane", CPUs: room.CPUs, MemoryGiB: room.MemoryGiB, DiskGrowthGiB: room.DiskGiB})
	if r.Impossible != "" || len(r.Over) != 0 {
		t.Fatalf("max did not pass: %+v", r)
	}
	if r := Check(host(), nil, Want{Name: "jane", CPUs: room.CPUs, MemoryGiB: room.MemoryGiB + 1, DiskGrowthGiB: room.DiskGiB + 1}); len(r.Over) == 0 && r.Impossible == "" {
		t.Fatalf("one more GiB still passed: %+v", r)
	}

	// Another running VM (6 CPUs, 10 GiB) takes its memory and overhead; CPUs stay all 16
	// (24 vCPUs less 6 is 18). A stopped one, and the agent itself, take nothing.
	others := []Sandbox{other, {Name: "jane", CPUs: 8, MemoryMiB: 8192, Running: true}, {Name: "idle", CPUs: 8, MemoryMiB: 8192}}
	if room := Max(host(), others, "jane"); room != (Room{CPUs: 16, MemoryGiB: 14, DiskGiB: 800}) {
		t.Fatalf("other VMs: %+v", room)
	}
	// Past the overcommit, CPUs are what is left of it.
	busy := []Sandbox{{Name: "a", CPUs: 16, MemoryMiB: 1024, Running: true}}
	if room := Max(host(), busy, "jane"); room.CPUs != 8 {
		t.Fatalf("overcommit: %+v", room)
	}
	// Disk growth already promised to existing VMs is not room.
	h := host()
	h.DiskGrowth = 300 * gib
	if room := Max(h, nil, "jane"); room.DiskGiB != 500 {
		t.Fatalf("disk growth: %+v", room)
	}
	// A full machine has no room, never a negative one.
	full := []Sandbox{{Name: "big", CPUs: 40, MemoryMiB: 40 * 1024, Running: true}}
	h.DiskFree = 100 * gib
	if room := Max(h, full, "jane"); room != (Room{}) {
		t.Fatalf("full machine: %+v", room)
	}
	// What could not be read is -1, never a guess.
	if room := Max(Host{}, nil, "jane"); room != (Room{CPUs: -1, MemoryGiB: -1, DiskGiB: -1}) {
		t.Fatalf("unread: %+v", room)
	}
}
