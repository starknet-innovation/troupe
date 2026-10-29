package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The same sentences the bash and Python progress helpers print for these calls.
func TestSentenceShapes(t *testing.T) {
	var out, errs bytes.Buffer
	now := time.Date(2026, 1, 2, 19, 52, 16, 0, time.UTC)
	p := &Printer{Agent: "Ada", Out: &out, Err: &errs, Now: func() time.Time { return now }}
	p.Print("backup", Started, "Stopping the gateway and taking a quiesced backup")
	p.Print("backup", OK, "Native backup verified")
	p.Print("cutover", "", "Same-tag replacement, finalizing now")
	p.Print("channel check", Action, "In your DM send exactly: !status")
	p.Print("done", OK, "")
	want := []string{
		"Ada backup: started. Stopping the gateway and taking a quiesced backup.",
		"Ada backup: ok in 0s. Native backup verified.",
		"Ada cutover: Same-tag replacement, finalizing now.",
		"Ada channel check: ACTION. In your DM send exactly: !status",
		"Ada done: ok.",
	}
	if got := strings.Split(strings.TrimSpace(out.String()), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if errs.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", errs.String())
	}
}

func TestFailedGoesToStderrWithDuration(t *testing.T) {
	var out, errs bytes.Buffer
	clock := time.Date(2026, 1, 2, 19, 52, 16, 0, time.UTC)
	p := &Printer{Agent: "Ada", Out: &out, Err: &errs, Now: func() time.Time { return clock }}
	p.Print("backup", Started, "Backing up")
	clock = clock.Add(4*time.Minute + 44*time.Second)
	p.Print("backup", Failed, "archive_database_invalid. Log: ~/native.log")
	want := "Ada backup: FAILED in 4m 44s. archive_database_invalid. Log: ~/native.log.\n"
	if errs.String() != want {
		t.Fatalf("stderr %q, want %q", errs.String(), want)
	}
	if strings.Contains(out.String(), "FAILED") {
		t.Fatalf("FAILED reached stdout: %q", out.String())
	}
}

func TestDurations(t *testing.T) {
	for seconds, want := range map[int]string{5: "5s", 75: "1m 15s", 3725: "1h 2m"} {
		if got := Duration(time.Duration(seconds) * time.Second); got != want {
			t.Errorf("Duration(%ds) = %q, want %q", seconds, got, want)
		}
	}
}

// No line carries the time: a step's duration says how long it took, and the journal times
// a unit's lines.
func TestLinesCarryNoTime(t *testing.T) {
	p := &Printer{Agent: "Ada", Now: func() time.Time { return time.Date(2026, 1, 2, 23, 0, 0, 0, time.UTC) }}
	if got := p.Line("note", "", "hello"); got != "Ada note: hello." {
		t.Fatalf("got %q", got)
	}
}
