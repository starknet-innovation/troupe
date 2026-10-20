//go:build dev

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/budget"
)

// budget check answers by its exit code, with troupe's own lines, and never asks. (Disk is
// left at 0: the fixture measures the real filesystem holding the test's folder.)
func TestBudgetCheckFitsOrRefusesWithoutAsking(t *testing.T) {
	runner, printer, out := budgetHost(t)
	e := env{ctx: context.Background(), out: out, err: out}
	if code := budgetCheck(e, runner, printer, budget.Want{Name: "coder-lab", CPUs: 4, MemoryGiB: 6}); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	out.Reset()
	if code := budgetCheck(e, runner, printer, budget.Want{Name: "coder-lab", CPUs: 4, MemoryGiB: 14}); code != 3 ||
		!strings.Contains(out.String(), "Over budget") || strings.Contains(out.String(), "Continue anyway") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// Larger than the whole machine can never run: 1, not over budget.
	out.Reset()
	if code := budgetCheck(e, runner, printer, budget.Want{Name: "coder-lab", CPUs: 4, MemoryGiB: 64}); code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestBudgetCheckNeedsEverySize(t *testing.T) {
	for _, args := range [][]string{
		{"budget"}, {"budget", "other"}, {"budget", "check", "--name", "x", "--cpu", "4", "--memory", "6"},
		{"budget", "check", "--name", "Bad Name", "--cpu", "4", "--memory", "6", "--disk", "60"},
		{"budget", "check", "--name", "x", "--cpu", "0", "--memory", "6", "--disk", "60"},
	} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
