package cli

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/published"
)

// size is one size flag's value: a whole number, or max, all this machine can give.
type size struct {
	n   int
	max bool
}

func (s *size) String() string {
	switch {
	case s.max:
		return "max"
	case s.n > 0:
		return strconv.Itoa(s.n)
	}
	return ""
}

func (s *size) Set(v string) error {
	if v == "max" {
		*s = size{max: true}
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return errors.New("a whole number or max")
	}
	*s = size{n: n}
	return nil
}

func (s size) given() bool { return s.max || s.n > 0 }

// minMaxDiskGiB is the smallest disk a max may leave: below it the disk is too cramped to use.
const minMaxDiskGiB = 10

// sizeFlags are create's and resize's size flags.
type sizeFlags struct{ cpu, memory, storage, docker size }

func (f *sizeFlags) register(set *flag.FlagSet) {
	set.Var(&f.cpu, "cpu", "")
	set.Var(&f.memory, "memory", "")
	set.Var(&f.storage, "storage", "")
	set.Var(&f.docker, "docker", "")
}

func (f sizeFlags) given() bool {
	return f.cpu.given() || f.memory.given() || f.storage.given() || f.docker.given()
}

func (f sizeFlags) anyMax() bool { return f.cpu.max || f.memory.max || f.storage.max || f.docker.max }

// inRange refuses an absurd number before it reaches any sum.
func (f sizeFlags) inRange() error {
	if err := sizesInRange(f.cpu.n, f.memory.n, f.storage.n); err != nil {
		return err
	}
	if f.docker.n > 1<<20 {
		return errors.New("the Docker disk must be at most 1048576 GiB")
	}
	return nil
}

// resolve turns the flags into sizes. base is the kit's defaults (create) or the agent's current
// sizes (resize); a flag left out keeps base's. room is budget.Max for the agent, read only when a
// flag says max. Disks: room is what both may add together (to nothing on create, to the current
// disks on resize); when both are max it is split in base's ratio of root to Docker. Refused: a
// max disk below 10 GiB (a root disk, on resize, below its current size), a max below 1 CPU or 1 GiB of memory (on resize, below the agent's
// current CPUs or memory), no disk left, and a figure this machine could not read.
func (f sizeFlags) resolve(base published.Defaults, room budget.Room, resize bool) (published.Defaults, error) {
	sizes := base
	pick := func(s size, fallback int) int {
		if s.n > 0 {
			return s.n
		}
		return fallback
	}
	sizes.CPUs = pick(f.cpu, base.CPUs)
	sizes.MemoryGiB = pick(f.memory, base.MemoryGiB)
	sizes.RootGiB = pick(f.storage, base.RootGiB)
	sizes.DockerGiB = pick(f.docker, base.DockerGiB)
	if f.cpu.max {
		if room.CPUs < 1 {
			return sizes, roomError("CPUs", room.CPUs, "")
		}
		if resize && room.CPUs < base.CPUs {
			return sizes, fmt.Errorf("--cpu max is %d here, fewer than the agent's %d: give a number to shrink it", room.CPUs, base.CPUs)
		}
		sizes.CPUs = room.CPUs
	}
	if f.memory.max {
		if room.MemoryGiB < 1 {
			return sizes, roomError("memory", room.MemoryGiB, " GiB")
		}
		if resize && room.MemoryGiB < base.MemoryGiB {
			return sizes, fmt.Errorf("--memory max is %d GiB here, less than the agent's %d GiB: give a number to shrink it", room.MemoryGiB, base.MemoryGiB)
		}
		sizes.MemoryGiB = room.MemoryGiB
	}
	if !f.storage.max && !f.docker.max {
		return sizes, nil
	}
	if room.DiskGiB < 0 {
		return sizes, roomError("disk", -1, "")
	}
	// The disk room on top of what the disks start from: nothing on create, the current disks
	// on resize (whose own growth the room already counts).
	var root0, docker0 int
	if resize {
		root0, docker0 = base.RootGiB, base.DockerGiB
	}
	left := room.DiskGiB
	if left == 0 {
		return sizes, errors.New("max disk: this machine has no disk left to add, after the 10% it keeps free and what existing disks may still grow")
	}
	switch {
	case f.storage.max && f.docker.max:
		share := left
		if total := base.RootGiB + base.DockerGiB; total > 0 {
			share = left * base.RootGiB / total
		}
		sizes.RootGiB, sizes.DockerGiB = root0+share, docker0+left-share
	case f.storage.max:
		sizes.RootGiB = root0 + left - (sizes.DockerGiB - docker0)
	default:
		sizes.DockerGiB = docker0 + left - (sizes.RootGiB - root0)
	}
	// The other disk, given as a number, may already take all the room.
	if f.storage.max != f.docker.max {
		other, given, added := f.docker, sizes.DockerGiB, sizes.DockerGiB-docker0
		label := fmt.Sprintf("--docker %d", given)
		if f.docker.max {
			other, given, added = f.storage, sizes.RootGiB, sizes.RootGiB-root0
			label = fmt.Sprintf("--storage %d", given)
		}
		if !other.given() {
			disk := "Docker"
			if f.docker.max {
				disk = "root"
			}
			label = fmt.Sprintf("the %d GiB %s disk", given, disk)
		}
		if added >= left {
			return sizes, fmt.Errorf("%s already takes all the %d GiB of disk this machine has to add", label, left)
		}
	}
	// max is what is left: any sensible leftover is taken as asked, only a cramped one refused.
	if least := max(minMaxDiskGiB, root0); f.storage.max && sizes.RootGiB < least {
		return sizes, fmt.Errorf("--storage max leaves a %d GiB root disk; it needs at least %d GiB (this machine has %d GiB of disk to add)",
			sizes.RootGiB, least, room.DiskGiB)
	}
	if f.docker.max && sizes.DockerGiB < minMaxDiskGiB {
		return sizes, fmt.Errorf("--docker max leaves a %d GiB Docker disk; it needs at least %d GiB (this machine has %d GiB of disk to add)",
			sizes.DockerGiB, minMaxDiskGiB, room.DiskGiB)
	}
	return sizes, nil
}

func roomError(resource string, got int, unit string) error {
	if got < 0 {
		return fmt.Errorf("max %s: this machine's %s could not be read", resource, resource)
	}
	return fmt.Errorf("max %s: this machine has %d%s left for an agent, after its reserves and the running sandboxes", resource, got, unit)
}

// describe is the sizes as the progress line shows them, marking each one max chose.
func (f sizeFlags) describe(s published.Defaults) string {
	mark := func(sz size) string {
		if sz.max {
			return " (max)"
		}
		return ""
	}
	parts := []string{
		fmt.Sprintf("%d CPUs%s", s.CPUs, mark(f.cpu)),
		fmt.Sprintf("%d GiB memory%s", s.MemoryGiB, mark(f.memory)),
		fmt.Sprintf("%d GiB root disk%s", s.RootGiB, mark(f.storage)),
	}
	if s.DockerGiB > 0 {
		parts = append(parts, fmt.Sprintf("%d GiB Docker disk%s", s.DockerGiB, mark(f.docker)))
	}
	line := strings.Join(parts, ", ")
	if f.anyMax() {
		line += ", after this machine's reserves and the VM's overhead"
	}
	return line
}
