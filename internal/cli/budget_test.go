package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// budgetHost fakes a 30 GiB machine whose daemon holds 1 GiB (27 GiB for agents), where sbx runs
// one sandbox of 10 GiB and lists a stopped one; it returns the runner and the printer's output.
func budgetHost(t *testing.T) (sbx.Runner, *progress.Printer, *bytes.Buffer) {
	t.Helper()
	return budgetHostIn(t, t.TempDir())
}

func budgetHostIn(t *testing.T, dir string) (sbx.Runner, *progress.Printer, *bytes.Buffer) {
	t.Helper()
	write := func(name, text string, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(text), mode)
	}
	write("meminfo", "MemTotal: 31457280 kB\n", 0o644)
	write("sandboxd/sandboxd.pid", "77\n", 0o644)
	write("proc/77/status", "VmRSS: 1048576 kB\n", 0o644)
	write("sbx", `#!/bin/sh
d=$(dirname "$0")
[ "$1" = ls ] && { cat "$d/ls.json" 2>/dev/null || echo '[{"name":"other","id":"o","status":"running","cpus":6,"memory_mib":10240},{"name":"grace","id":"g","status":"stopped","cpus":8,"memory_mib":12288}]'; }
exit 0
`, 0o755)
	originalUp := unitUp
	unitUp = func(name string) bool { _, err := os.Stat(filepath.Join(dir, "up-"+name)); return err == nil }
	t.Cleanup(func() { unitUp = originalUp })
	original := budgetPaths
	budgetPaths = func() budget.Paths {
		return budget.Paths{Meminfo: filepath.Join(dir, "meminfo"), Proc: filepath.Join(dir, "proc"), Sandboxd: filepath.Join(dir, "sandboxd")}
	}
	t.Cleanup(func() { budgetPaths = original })
	var out bytes.Buffer
	printer := &progress.Printer{Agent: "Jane", Location: time.UTC, Out: &out, Err: &out, Now: time.Now}
	return sbx.Runner{Binary: filepath.Join(dir, "sbx"), Log: filepath.Join(dir, "log"), Probe: func(context.Context) error { return nil }}, printer, &out
}

func TestTheBudgetPassesWhatFits(t *testing.T) {
	runner, printer, out := budgetHost(t)
	e := env{ctx: context.Background(), out: out, err: out}
	if err := checkBudget(e, runner, printer, budget.Want{Name: "jane", CPUs: 4, MemoryGiB: 8}, confirmBudget); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "Over budget") {
		t.Fatalf("output %s", out)
	}
}

func TestOverBudgetAsksAndForceConfirms(t *testing.T) {
	// 10+2 running, plus 14+2 asked: 28 GiB against 27.
	want := budget.Want{Name: "jane", CPUs: 4, MemoryGiB: 14}
	runner, printer, out := budgetHost(t)
	e := env{ctx: context.Background(), out: out, err: out}
	if err := checkBudget(e, runner, printer, want, forcedBudget); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "Over budget. Memory: 28 GiB committed, 27 GiB available, 1 GiB over") ||
		!strings.Contains(out.String(), "Continuing anyway (--force)") {
		t.Fatalf("output %s", out)
	}
	// Without --force and no terminal to answer on, the answer is no.
	if _, err := os.Open("/dev/tty"); err == nil {
		t.Skip("a terminal is attached; the prompt would wait for it")
	}
	runner, printer, out = budgetHost(t)
	if err := checkBudget(env{ctx: context.Background(), out: out, err: out}, runner, printer, want, confirmBudget); !errors.Is(err, budget.ErrDeclined) {
		t.Fatalf("err %v\n%s", err, out)
	}
}

func TestAnAgentLargerThanTheMachineIsRefusedEvenWithForce(t *testing.T) {
	runner, printer, out := budgetHost(t)
	e := env{ctx: context.Background(), out: out, err: out}
	err := checkBudget(e, runner, printer, budget.Want{Name: "jane", CPUs: 4, MemoryGiB: 40}, forcedBudget)
	if err == nil || !strings.Contains(err.Error(), "jane alone needs 42 GiB of memory") {
		t.Fatalf("err %v", err)
	}
}

// A check that changes nothing (recreate --check) reports over budget as a refusal, never asks.
func TestACheckReportsOverBudgetWithoutAsking(t *testing.T) {
	runner, printer, out := budgetHost(t)
	err := checkBudget(env{ctx: context.Background(), out: out, err: out}, runner, printer, budget.Want{Name: "jane", CPUs: 4, MemoryGiB: 14}, reportBudget)
	if !errors.Is(err, budget.ErrOver) {
		t.Fatalf("err %v", err)
	}
}

// A sandbox whose gateway unit is up counts as running even while its VM is momentarily stopped.
func TestAnAgentWhoseUnitIsUpCounts(t *testing.T) {
	dir := t.TempDir()
	runner, printer, out := budgetHostIn(t, dir)
	os.WriteFile(filepath.Join(dir, "up-grace"), nil, 0o644)
	// other 10+2 and grace 12+2 run: jane's 4+2 makes 30 GiB against 27.
	err := checkBudget(env{ctx: context.Background(), out: out, err: out}, runner, printer, budget.Want{Name: "jane", CPUs: 4, MemoryGiB: 4}, reportBudget)
	if !errors.Is(err, budget.ErrOver) {
		t.Fatalf("err %v\n%s", err, out)
	}
}

// A running sandbox whose size sbx does not report is refused: never counted as nothing.
func TestAnUnreadableSizeIsRefused(t *testing.T) {
	dir := t.TempDir()
	runner, printer, out := budgetHostIn(t, dir)
	os.WriteFile(filepath.Join(dir, "ls.json"), []byte(`[{"name":"other","id":"o","status":"running"}]`), 0o644)
	err := checkBudget(env{ctx: context.Background(), out: out, err: out}, runner, printer, budget.Want{Name: "jane", CPUs: 1, MemoryGiB: 1}, forcedBudget)
	if err == nil || !strings.Contains(err.Error(), "the size of the running sandbox other could not be read") {
		t.Fatalf("err %v", err)
	}
}
