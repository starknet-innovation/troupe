package sbx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeSbx(t *testing.T, body string) Runner {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "sbx")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return Runner{Binary: binary, Log: filepath.Join(dir, "native.log"), Probe: func(context.Context) error { return nil }}
}

func TestOutputReturnsStdoutAndLogsEverything(t *testing.T) {
	r := fakeSbx(t, `echo "out $*"; echo "chatter" >&2`)
	out, err := r.Output(context.Background(), Call{Args: []string{"ls", "--json"}})
	if err != nil || string(out) != "out ls --json\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	log, _ := os.ReadFile(r.Log)
	for _, want := range []string{`$ sbx ["ls" "--json"]`, "out ls --json", "chatter"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if info, _ := os.Stat(r.Log); info.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", info.Mode().Perm())
	}
}

func TestFailedProbeSkipsSbx(t *testing.T) {
	r := fakeSbx(t, `touch "$0.ran"`)
	r.Probe = func(context.Context) error { return errors.New("daemon down") }
	_, err := r.Output(context.Background(), Call{Args: []string{"ls"}})
	if !errors.Is(err, ErrProbe) {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(r.Binary + ".ran"); statErr == nil {
		t.Fatal("sbx ran after a failed probe")
	}
}

func TestNonzeroExitIsAnError(t *testing.T) {
	r := fakeSbx(t, `exit 3`)
	if _, err := r.Output(context.Background(), Call{Args: []string{"exec", "x"}}); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v", err)
	}
}

// A hung sbx is asked to stop at the deadline and killed if it ignores that.
func TestTimeoutStopsAHungCall(t *testing.T) {
	r := fakeSbx(t, `trap '' TERM; sleep 30`)
	start := time.Now()
	_, err := r.Output(context.Background(), Call{Args: []string{"exec", "x"}, Timeout: 300 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("took %s", elapsed)
	}
}

// A detached call runs in its own process group, out of reach of the terminal's Ctrl+C.
func TestDetachedCallHasItsOwnProcessGroup(t *testing.T) {
	r := fakeSbx(t, `ps -o pgid= -p $$`)
	own := fmt.Sprint(syscall.Getpgrp())
	out, err := r.Output(context.Background(), Call{Args: []string{"x"}})
	if err != nil || strings.TrimSpace(string(out)) != own {
		t.Fatalf("attached call: group %q (ours %s) %v", out, own, err)
	}
	r.Detached = true
	out, err = r.Output(context.Background(), Call{Args: []string{"x"}})
	if err != nil || strings.TrimSpace(string(out)) == own {
		t.Fatalf("detached call shares our group %q: %v", out, err)
	}
}

// A VM that prints without end is stopped at the limit instead of filling the host's memory.
func TestEndlessOutputIsStoppedAtTheLimit(t *testing.T) {
	r := fakeSbx(t, `exec yes`)
	start := time.Now()
	out, err := r.Output(context.Background(), Call{Args: []string{"exec"}, Private: true, Limit: 1 << 20, Timeout: time.Minute})
	if !errors.Is(err, ErrTooLarge) || out != nil || time.Since(start) > 15*time.Second {
		t.Fatalf("err %v, %d bytes kept, after %v", err, len(out), time.Since(start))
	}
}

// Output from a VM reaches the log escaped, and the log stops growing at its cap.
func TestLogIsEscapedAndCapped(t *testing.T) {
	r := fakeSbx(t, `printf 'hello \033]0;owned\007 world\n' >&2`)
	if _, err := r.Output(context.Background(), Call{Args: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(r.Log)
	if bytes.ContainsAny(data, "\x1b\x07") || !bytes.Contains(data, []byte(`\x1b]0;owned\x07`)) {
		t.Fatalf("log %q", data)
	}
	// Every non-ASCII byte too: a raw 0x9b, U+009B in UTF-8, and text such as U+06DB whose
	// continuation byte 0x9b is CSI to an 8-bit terminal.
	file2, _ := os.CreateTemp(t.TempDir(), "log")
	LogWriter(file2).Write([]byte("a\x9b[2Jb\xc2\x9bc\xdb\x9bd"))
	file2.Close()
	raw, _ := os.ReadFile(file2.Name())
	for _, b := range raw {
		if b >= 0x7f || (b < 0x20 && b != '\n' && b != '\t') {
			t.Fatalf("log %q keeps byte %#x", raw, b)
		}
	}
	if string(raw) != `a\x9b[2Jb\xc2\x9bc\xdb\x9bd` {
		t.Fatalf("log %q", raw)
	}
	file, _ := os.OpenFile(r.Log, os.O_WRONLY|os.O_APPEND, 0o600)
	file.Truncate(MaxLog - 10)
	w := LogWriter(file)
	w.Write(bytes.Repeat([]byte("x"), 1000))
	file.Close()
	if info, _ := os.Stat(r.Log); info.Size() > MaxLog {
		t.Fatalf("log grew to %d", info.Size())
	}
	// One call's flood stops at its own budget, leaving room for the calls after it.
	flood, _ := os.CreateTemp(t.TempDir(), "log")
	LogWriter(flood).Write(bytes.Repeat([]byte("y"), 3*MaxCallLog))
	LogWriter(flood).Write([]byte("next call"))
	flood.Close()
	data, _ = os.ReadFile(flood.Name())
	if len(data) != MaxCallLog+len("next call") {
		t.Fatalf("flood wrote %d bytes", len(data))
	}
}

// sbx 0.48 lists sandboxes as a bare array; any other shape, the older wrapped one included,
// is unreadable rather than an empty machine.
func TestInventoryReadsOnlyABareArray(t *testing.T) {
	r := fakeSbx(t, `echo '[{"name":"ada","id":"1","agent":"coder","status":"running","cpus":8}]'`)
	records, err := r.Inventory(context.Background())
	if err != nil || len(records) != 1 || records["ada"].ID != "1" || records["ada"].Status != "running" {
		t.Fatalf("records %+v err %v", records, err)
	}
	for _, listing := range []string{`{"sandboxes":[{"name":"ada","id":"1","status":"running"}]}`,
		`{"sandboxes":[]}`, `null`, `not json`} {
		r := fakeSbx(t, "echo '"+listing+"'")
		if records, err := r.Inventory(context.Background()); !errors.Is(err, ErrInventory) {
			t.Errorf("%s: records %+v err %v", listing, records, err)
		}
	}
	r = fakeSbx(t, `echo '[{"name":"ada"},{"name":"ada"}]'`)
	if _, err := r.Inventory(context.Background()); !errors.Is(err, ErrInventory) {
		t.Errorf("a name held twice: err %v", err)
	}
	r = fakeSbx(t, `echo '[]'`)
	if records, err := r.Inventory(context.Background()); err != nil || len(records) != 0 {
		t.Errorf("an empty machine: records %+v err %v", records, err)
	}
}
