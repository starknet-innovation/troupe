package guard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeDaemon is a sandboxd folder this test process serves: it holds the control socket and
// answers the sandbox list on it; a stub systemctl reports mainPID.
type fakeDaemon struct {
	paths   Paths
	control net.Listener
	status  int
	body    string
	delay   time.Duration
	mainPID int
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	// Unix socket paths are limited to 108 bytes, so keep the folder short.
	dir, err := os.MkdirTemp("", "sbxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := &fakeDaemon{paths: Paths{Pairs: filepath.Join(dir, "pairs"), Sandboxd: dir, DaemonUnit: "sbx-daemon.service"},
		status: http.StatusOK, body: "[]", mainPID: os.Getpid()}

	control, err := net.Listen("unix", filepath.Join(dir, "sandboxd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	d.control = control
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(d.delay)
		w.WriteHeader(d.status)
		fmt.Fprint(w, d.body)
	})}
	go server.Serve(control)
	t.Cleanup(func() { server.Close() })

	d.writePidfile(t, strconv.Itoa(os.Getpid())+"\n")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(bin, "systemctl")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat \""+filepath.Join(dir, "mainpid")+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d.setMainPID(t, os.Getpid())
	return d
}

func (d *fakeDaemon) writePidfile(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.paths.Sandboxd, "sandboxd.pid"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (d *fakeDaemon) setMainPID(t *testing.T, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.paths.Sandboxd, "mainpid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbePassesForAHealthyOwnedDaemon(t *testing.T) {
	d := newFakeDaemon(t)
	if err := Probe(context.Background(), d.paths); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
}

func TestProbeFails(t *testing.T) {
	cases := map[string]func(t *testing.T, d *fakeDaemon){
		"unit main PID differs from pidfile": func(t *testing.T, d *fakeDaemon) { d.setMainPID(t, os.Getpid()+1) },
		"unit not running":                   func(t *testing.T, d *fakeDaemon) { d.setMainPID(t, 0) },
		"pidfile missing": func(t *testing.T, d *fakeDaemon) {
			os.Remove(filepath.Join(d.paths.Sandboxd, "sandboxd.pid"))
		},
		"socket held by another process": func(t *testing.T, d *fakeDaemon) {
			other := exec.Command("sleep", "30")
			if err := other.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { other.Process.Kill(); other.Wait() })
			d.setMainPID(t, other.Process.Pid)
			d.writePidfile(t, strconv.Itoa(other.Process.Pid))
		},
		"socket also held by a second process": func(t *testing.T, d *fakeDaemon) {
			// The child gets the socket through SyscallConn and ForkExec, never exec.Cmd: its Fd
			// would switch the shared socket to blocking mode, and then closing the listener
			// could no longer end the server's pending accept. The child keeps no stdio.
			conn, err := d.control.(*net.UnixListener).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var dup uintptr
			var errno syscall.Errno
			if err := conn.Control(func(fd uintptr) {
				dup, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_DUPFD_CLOEXEC, 0)
			}); err != nil {
				t.Fatal(err)
			}
			if errno != 0 {
				t.Fatal(errno)
			}
			defer syscall.Close(int(dup))
			sleep, err := exec.LookPath("sleep")
			if err != nil {
				t.Fatal(err)
			}
			pid, err := syscall.ForkExec(sleep, []string{"sleep", "30"}, &syscall.ProcAttr{Files: []uintptr{^uintptr(0), ^uintptr(0), ^uintptr(0), dup}})
			if err != nil {
				t.Fatal(err)
			}
			second, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { second.Kill(); second.Wait() })
		},
		"list call answers an error": func(t *testing.T, d *fakeDaemon) { d.status = http.StatusInternalServerError },
		"list call answers no list":  func(t *testing.T, d *fakeDaemon) { d.body = `{"message":"Not Found"}` },
		"list call answers null":     func(t *testing.T, d *fakeDaemon) { d.body = "null" },
		"list call hangs": func(t *testing.T, d *fakeDaemon) {
			old := listTimeout
			listTimeout = 200 * time.Millisecond
			t.Cleanup(func() { listTimeout = old })
			d.delay = time.Second
		},
		"sandboxd folder empty": func(t *testing.T, d *fakeDaemon) {
			d.paths.Sandboxd = t.TempDir()
		},
	}
	for title, breakIt := range cases {
		t.Run(title, func(t *testing.T) {
			d := newFakeDaemon(t)
			breakIt(t, d)
			if err := Probe(context.Background(), d.paths); err == nil {
				t.Fatal("probe passed")
			}
		})
	}
}

// Signed out, sandboxd refuses the list with a 401: the probe says what to run.
func TestProbeNamesASignedOutSbx(t *testing.T) {
	d := newFakeDaemon(t)
	d.status = http.StatusUnauthorized
	if err := Probe(context.Background(), d.paths); err == nil || !strings.Contains(err.Error(), "sbx login") {
		t.Fatalf("err %v", err)
	}
}

func TestProbeWaitRetriesUntilReady(t *testing.T) {
	d := newFakeDaemon(t)
	d.setMainPID(t, 0)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		os.WriteFile(filepath.Join(d.paths.Sandboxd, "mainpid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	}()
	if err := ProbeWait(context.Background(), d.paths, 10*time.Second); err != nil {
		t.Fatalf("probe never passed: %v", err)
	}
	d.setMainPID(t, 0)
	if err := ProbeWait(context.Background(), d.paths, 1200*time.Millisecond); err == nil {
		t.Fatal("probe passed for a stopped unit")
	}
}

func TestSuspended(t *testing.T) {
	paths := Paths{Pairs: t.TempDir()}
	record := func(text string) {
		if err := os.WriteFile(filepath.Join(paths.Pairs, "ada.json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if Suspended(paths, "ada") {
		t.Fatal("no record suspends")
	}
	record(`{"phase":"complete","copy":"ada-rehearsal"}`)
	if Suspended(paths, "ada") {
		t.Fatal("a complete record suspends")
	}
	// Keys match exactly, as jq reads them: other spellings of phase do not count.
	for _, text := range []string{`{"phase":"restored"}`, `{"copy":"x"}`, `not json`, `{"phase":1}`, `null`, `[]`,
		`{"Phase":"complete"}`, `{"phase":"copying","PHASE":"complete"}`} {
		record(text)
		if !Suspended(paths, "ada") {
			t.Fatalf("record %s does not suspend", text)
		}
	}
	if Suspended(paths, "grace") {
		t.Fatal("another agent's record suspends")
	}
}

// The lock is the same flock(2) the bash guard and util-linux flock take, in both directions.
func TestLockExcludesShellFlock(t *testing.T) {
	paths := Paths{Pairs: t.TempDir()}
	file := filepath.Join(paths.Pairs, "maintenance.lock")

	// -o: only flock itself holds the lock, so reaping it releases the lock; the group kill
	// also stops its command.
	holder := exec.Command("flock", "-o", file, "sh", "-c", "echo locked; sleep 30")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "locked" {
		t.Fatalf("holder said %q", line)
	}
	if _, err := Acquire(paths); !errors.Is(err, ErrBusy) {
		t.Fatalf("acquire while the shell holds the lock: %v", err)
	}
	syscall.Kill(-holder.Process.Pid, syscall.SIGKILL)
	holder.Wait()

	lock, err := Acquire(paths)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := exec.Command("flock", "-n", file, "true").Run(); err == nil {
		t.Fatal("shell flock succeeded while troupe holds the lock")
	}
	lock.Release()
	if err := exec.Command("flock", "-n", file, "true").Run(); err != nil {
		t.Fatalf("shell flock after release: %v", err)
	}
}

// VMGone reads containerd's task folder. A host where no VM has run yet has an empty task root,
// which is gone; a missing root, another namespace, a live shim or an unreadable task is not.
func TestVMGone(t *testing.T) {
	paths := Paths{Sandboxd: t.TempDir()}
	root := filepath.Join(paths.Sandboxd, "containerd", "state", "io.containerd.runtime.v2.task")
	if VMGone(paths, "ada") {
		t.Fatal("gone without a task root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if !VMGone(paths, "ada") {
		t.Fatal("not gone on a host where no VM has run")
	}
	// A task root holding anything but docker is a layout this does not know.
	elsewhere := filepath.Join(root, "renamed", "abc")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if VMGone(paths, "ada") {
		t.Fatal("gone with tasks under another namespace")
	}
	os.RemoveAll(filepath.Join(root, "renamed"))
	task := filepath.Join(root, "docker", "abc")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(file, text string) {
		if err := os.WriteFile(filepath.Join(task, file), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", `{"hostname":"ada"}`)
	write("shim.pid", strconv.Itoa(os.Getpid()))
	if VMGone(paths, "ada") {
		t.Fatal("gone while its shim runs")
	}
	if !VMGone(paths, "grace") {
		t.Fatal("another VM's shim keeps grace")
	}
	write("shim.pid", "0")
	if VMGone(paths, "ada") {
		t.Fatal("gone with an unreadable shim pid")
	}
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	write("shim.pid", strconv.Itoa(exited.Process.Pid))
	if !VMGone(paths, "ada") {
		t.Fatal("not gone after its shim exited")
	}
	write("config.json", `not json`)
	if VMGone(paths, "ada") {
		t.Fatal("gone with an unreadable task config")
	}
}
