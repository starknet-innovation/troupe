// Package sbx runs the Docker Sandbox CLI. Every call probes sandboxd first, because sbx
// silently spawns an unmanaged daemon when systemd's is down, and every call has a deadline.
package sbx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
)

// Runner calls sbx under one set of guard paths, appending native output to Log.
type Runner struct {
	Paths guard.Paths
	// Log receives every call's stderr, and its stdout unless the caller captures it.
	Log string
	// Binary is the sbx executable; tests point it at a fake.
	Binary string
	// Probe replaces the sandboxd probe in tests; nil means guard.Probe on Paths.
	Probe func(context.Context) error
	// Detached runs sbx in its own process group, so a Ctrl+C at the terminal cannot reach
	// it: cleanup calls (clearing a drain) must finish even when the operator presses it again.
	Detached bool
}

// Call is one sbx invocation.
type Call struct {
	Args    []string
	Stdin   io.Reader
	Timeout time.Duration
	// Private keeps stdout out of the log (only its size is noted): guest answers can carry
	// the agent's files and session prompts.
	Private bool
	// Env adds environment variables to the call's (sbx env create reads its disk sizes there).
	Env []string
	// Stdout, when set, receives stdout as it streams (an archive), unbuffered and unlogged.
	Stdout io.Writer
	// Limit caps stdout in bytes; past it the call is stopped and fails with ErrTooLarge.
	// Zero means MaxAnswer for a buffered call; a streamed call must set its own.
	Limit int64
}

// MaxAnswer caps a buffered call's stdout: guest jobs and sbx listings answer in one short JSON
// line. Decoding inflates JSON many times over in memory, so the cap stays small; the one job
// that returns files (update read) sets its own.
const MaxAnswer = 1 << 20

// ErrTooLarge means a call's stdout passed its limit, so the call was stopped: the VM on the
// other end is the agent's and must not be able to fill the host's memory or disk.
var ErrTooLarge = errors.New("sbx output larger than allowed")

// ErrTimeout means sbx did not finish before its deadline.
var ErrTimeout = errors.New("sbx timed out")

// ErrProbe means sandboxd failed its probe, so sbx was not called.
var ErrProbe = errors.New("sandboxd probe failed")

// Output runs the call after a passing probe and returns its stdout.
func (r Runner) Output(ctx context.Context, call Call) ([]byte, error) {
	log, err := r.openLog()
	if err != nil {
		return nil, err
	}
	defer log.Close()
	fmt.Fprintf(log, "$ sbx %q\n", call.Args)
	logw := LogWriter(log)
	probe := r.Probe
	if probe == nil {
		probe = func(ctx context.Context) error { return guard.Probe(ctx, r.Paths) }
	}
	if err := probe(ctx); err != nil {
		fmt.Fprintf(log, "not run: sandboxd probe: %v\n", err)
		return nil, fmt.Errorf("%w: %v", ErrProbe, err)
	}

	timeout := call.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limit := call.Limit
	if limit == 0 {
		limit = MaxAnswer
	}
	var tooLarge bool
	binary := r.Binary
	if binary == "" {
		binary = "sbx"
	}
	cmd := exec.CommandContext(ctx, binary, call.Args...)
	// On the deadline ask politely, then kill after ten seconds, as timeout -k 10 did.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = call.Stdin
	if len(call.Env) > 0 {
		// The call's own DOCKER_SANDBOXES_* settings are the only ones: a value exported in
		// the operator's shell must not add a disk the declaration does not have.
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "DOCKER_SANDBOXES_") {
				cmd.Env = append(cmd.Env, v)
			}
		}
		cmd.Env = append(cmd.Env, call.Env...)
	}
	if r.Detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	var stdout bytes.Buffer
	var sink io.Writer = io.MultiWriter(&stdout, logw)
	if call.Private {
		sink = &stdout
	}
	if call.Stdout != nil {
		sink = call.Stdout
	}
	cmd.Stdout = &limited{w: sink, left: limit, over: func() { tooLarge = true; cancel() }}
	cmd.Stderr = logw
	err = cmd.Run()
	if tooLarge {
		fmt.Fprintf(log, "stopped: stdout passed %d bytes\n", limit)
		return nil, ErrTooLarge
	}
	if call.Private {
		fmt.Fprintf(log, "stdout: %d bytes, not logged\n", stdout.Len())
	}
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(log, "timed out after %s\n", timeout)
		return stdout.Bytes(), ErrTimeout
	}
	if err != nil {
		fmt.Fprintf(log, "exit: %v\n", err)
		return stdout.Bytes(), fmt.Errorf("sbx %s: %w", call.Args[0], err)
	}
	return stdout.Bytes(), nil
}

// limited passes at most left bytes to w, then calls over once and refuses the rest.
type limited struct {
	w    io.Writer
	left int64
	over func()
	done bool
}

func (l *limited) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		if !l.done {
			l.done = true
			l.over()
		}
		return 0, ErrTooLarge
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// MaxLog caps one log file, and MaxCallLog what one call may add to it: past them, output from
// the VM is dropped rather than filling the disk, and one flood cannot crowd out later calls.
const (
	MaxLog     = 256 << 20
	MaxCallLog = 1 << 20
)

// LogWriter writes VM output into a private log with control characters escaped (so reading
// the log cannot drive the terminal), up to MaxCallLog and never past MaxLog.
func LogWriter(file *os.File) io.Writer {
	size := int64(0)
	if info, err := file.Stat(); err == nil {
		size = info.Size()
	}
	left := MaxLog - size
	if left > MaxCallLog {
		left = MaxCallLog
	}
	return &logWriter{file: file, left: left}
}

type logWriter struct {
	mu   sync.Mutex // stdout and stderr may share one writer from two goroutines
	file *os.File
	left int64
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left <= 0 {
		return len(p), nil
	}
	// Only printable ASCII, newlines and tabs pass: every other byte, including every byte of
	// UTF-8 text (whose continuation bytes are C1 controls to an 8-bit terminal), is escaped.
	var out bytes.Buffer
	for _, b := range p {
		if (b >= 0x20 && b < 0x7f) || b == '\n' || b == '\t' {
			out.WriteByte(b)
		} else {
			fmt.Fprintf(&out, "\\x%02x", b)
		}
	}
	data := out.Bytes()
	if int64(len(data)) > l.left {
		data = data[:l.left]
	}
	l.left -= int64(len(data))
	l.file.Write(data)
	return len(p), nil
}

func (r Runner) openLog() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(r.Log), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(r.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}
