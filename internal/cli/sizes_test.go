package cli

import (
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/published"
)

func flags(t *testing.T, cpu, memory, storage, docker string) sizeFlags {
	t.Helper()
	var f sizeFlags
	for _, v := range []struct {
		s     *size
		value string
	}{{&f.cpu, cpu}, {&f.memory, memory}, {&f.storage, storage}, {&f.docker, docker}} {
		if v.value != "" {
			if err := v.s.Set(v.value); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func TestSizeTakesANumberOrMax(t *testing.T) {
	var s size
	for _, bad := range []string{"0", "-1", "lots", "MAX", "1.5", ""} {
		if s.Set(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if s.Set("max") != nil || !s.max || s.Set("12") != nil || s.max || s.n != 12 {
		t.Fatalf("%+v", s)
	}
}

// coder's defaults, and a small host: 4 CPUs, 11 GiB and 170 GiB of disk to give.
var (
	coderSizes = published.Defaults{CPUs: 8, MemoryGiB: 12, RootGiB: 100, DockerGiB: 20}
	hostRoom   = budget.Room{CPUs: 4, MemoryGiB: 11, DiskGiB: 170}
)

func TestMaxSizesOnCreate(t *testing.T) {
	for name, c := range map[string]struct {
		f    [4]string
		want published.Defaults
	}{
		"nothing given":          {[4]string{}, coderSizes},
		"numbers only":           {[4]string{"2", "4", "60", "30"}, published.Defaults{CPUs: 2, MemoryGiB: 4, RootGiB: 60, DockerGiB: 30}},
		"everything max":         {[4]string{"max", "max", "max", "max"}, published.Defaults{CPUs: 4, MemoryGiB: 11, RootGiB: 141, DockerGiB: 29}},
		"root max, Docker given": {[4]string{"", "", "max", "60"}, published.Defaults{CPUs: 8, MemoryGiB: 12, RootGiB: 110, DockerGiB: 60}},
		"Docker max, root given": {[4]string{"", "", "100", "max"}, published.Defaults{CPUs: 8, MemoryGiB: 12, RootGiB: 100, DockerGiB: 70}},
		"root max, Docker kept":  {[4]string{"", "max", "max", ""}, published.Defaults{CPUs: 8, MemoryGiB: 11, RootGiB: 150, DockerGiB: 20}},
		"below the kit's root":   {[4]string{"", "", "max", "100"}, published.Defaults{CPUs: 8, MemoryGiB: 12, RootGiB: 70, DockerGiB: 100}},
	} {
		t.Run(name, func(t *testing.T) {
			f := flags(t, c.f[0], c.f[1], c.f[2], c.f[3])
			got, err := f.resolve(coderSizes, hostRoom, false)
			if err != nil || got != c.want {
				t.Fatalf("got %+v err %v", got, err)
			}
			// What max chose always fits the room it was given.
			if f.storage.max || f.docker.max {
				if got.RootGiB+got.DockerGiB > hostRoom.DiskGiB {
					t.Fatalf("disks %d+%d past %d", got.RootGiB, got.DockerGiB, hostRoom.DiskGiB)
				}
			}
		})
	}
}

// On resize the disk room comes on top of the current disks, and the agent's own sizes are the base.
func TestMaxSizesOnResize(t *testing.T) {
	current := published.Defaults{CPUs: 2, MemoryGiB: 4, RootGiB: 100, DockerGiB: 20}
	got, err := flags(t, "max", "max", "max", "max").resolve(current, budget.Room{CPUs: 4, MemoryGiB: 11, DiskGiB: 60}, true)
	if err != nil || got != (published.Defaults{CPUs: 4, MemoryGiB: 11, RootGiB: 150, DockerGiB: 30}) {
		t.Fatalf("got %+v err %v", got, err)
	}
	got, err = flags(t, "", "", "max", "").resolve(current, budget.Room{DiskGiB: 60}, true)
	if err != nil || got.RootGiB != 160 || got.DockerGiB != 20 {
		t.Fatalf("root max: %+v err %v", got, err)
	}
	// max never shrinks an agent: on a machine already over budget it is refused instead.
	for _, f := range []sizeFlags{flags(t, "max", "", "", ""), flags(t, "", "max", "", "")} {
		if _, err := f.resolve(current, budget.Room{CPUs: 1, MemoryGiB: 2}, true); err == nil || !strings.Contains(err.Error(), "give a number to shrink it") {
			t.Fatalf("shrank: %v", err)
		}
	}
}

// Too little room, or a figure this machine could not read, is refused with what is left, never
// shrunk to fit.
func TestMaxRefusesTooLittleRoom(t *testing.T) {
	for name, c := range map[string]struct {
		f    [4]string
		room budget.Room
		want string
	}{
		"no memory left":    {[4]string{"", "max", "", ""}, budget.Room{MemoryGiB: 0}, "has 0 GiB left"},
		"no CPUs left":      {[4]string{"max", "", "", ""}, budget.Room{CPUs: 0}, "has 0 left"},
		"memory unread":     {[4]string{"", "max", "", ""}, budget.Room{MemoryGiB: -1}, "could not be read"},
		"disk unread":       {[4]string{"", "", "max", ""}, budget.Room{DiskGiB: -1}, "could not be read"},
		"root cramped":      {[4]string{"", "", "max", ""}, budget.Room{DiskGiB: 25}, "needs at least 10 GiB"},
		"Docker squeezed":   {[4]string{"", "", "165", "max"}, budget.Room{DiskGiB: 170}, "needs at least 10 GiB"},
		"root takes it all": {[4]string{"", "", "500", "max"}, budget.Room{DiskGiB: 145}, "--storage 500 already takes all the 145 GiB"},
		"default Docker":    {[4]string{"", "", "max", ""}, budget.Room{DiskGiB: 15}, "the 20 GiB Docker disk already takes all the 15 GiB"},
		"no disk left":      {[4]string{"", "", "max", "max"}, budget.Room{DiskGiB: 0}, "no disk left to add"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := flags(t, c.f[0], c.f[1], c.f[2], c.f[3]).resolve(coderSizes, c.room, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestDescribeMarksMax(t *testing.T) {
	f := flags(t, "max", "", "max", "60")
	line := f.describe(published.Defaults{CPUs: 4, MemoryGiB: 12, RootGiB: 110, DockerGiB: 60})
	want := "4 CPUs (max), 12 GiB memory, 110 GiB root disk (max), 60 GiB Docker disk, after this machine's reserves and the VM's overhead"
	if line != want {
		t.Fatalf("%q", line)
	}
}
