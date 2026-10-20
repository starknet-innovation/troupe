// Package guard protects the shared sandbox host: the honest sandboxd probe, the durable
// phase records other repositories write while a disposable copy of an agent exists, and
// the host-wide maintenance lock. Coordination happens only through shared state on disk
// (the lock file and phase records), never through shared code, so other tools on the
// same host that use the same paths keep working beside troupe.
package guard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Paths are the shared host state the guard reads. Default fills them for the host user.
type Paths struct {
	// Pairs holds the maintenance lock and the phase records.
	Pairs string
	// Sandboxd is the daemon's state folder: pidfile and control socket.
	Sandboxd string
	// DaemonUnit is the systemd unit that owns sandboxd.
	DaemonUnit string
}

// Default returns the standard paths under the user's home, overridable for tests and other
// layouts through TROUPE_PAIRS_DIR and TROUPE_SANDBOXD_DIR.
func Default() Paths {
	home, _ := os.UserHomeDir()
	paths := Paths{
		Pairs:      filepath.Join(home, ".local/state/sandbox-pairs"),
		Sandboxd:   filepath.Join(home, ".local/state/sandboxes/sandboxes/sandboxd"),
		DaemonUnit: "sbx-daemon.service",
	}
	if dir := os.Getenv("TROUPE_PAIRS_DIR"); dir != "" {
		paths.Pairs = dir
	}
	if dir := os.Getenv("TROUPE_SANDBOXD_DIR"); dir != "" {
		paths.Sandboxd = dir
	}
	return paths
}

// Probe passes only when systemd owns the running sandboxd and it answers: the unit's main
// PID, the pidfile and the only process holding the control socket agree, and the sandbox
// list on that socket (what sbx ls reads) answers. Never run sbx without it: sbx would
// otherwise spawn an unmanaged daemon. A second holder (a leftover daemon, an inherited
// descriptor) fails it. sbx 0.49.1 no longer serves the Docker API on docker.sock.
func Probe(ctx context.Context, paths Paths) error {
	unit, err := mainPID(ctx, paths.DaemonUnit)
	if err != nil {
		return err
	}
	pidfile, err := os.ReadFile(filepath.Join(paths.Sandboxd, "sandboxd.pid"))
	if err != nil {
		return fmt.Errorf("sandboxd pidfile unreadable: %w", err)
	}
	if strings.TrimSpace(string(pidfile)) != strconv.Itoa(unit) {
		return fmt.Errorf("sandboxd pidfile does not match %s", paths.DaemonUnit)
	}
	holders, err := socketHolders(filepath.Join(paths.Sandboxd, "sandboxd.sock"))
	if err != nil {
		return err
	}
	if len(holders) != 1 || !holders[unit] {
		return fmt.Errorf("the sandboxd socket is held by %v, not only by %s (%d)", sortedPIDs(holders), paths.DaemonUnit, unit)
	}
	return listSandboxes(ctx, filepath.Join(paths.Sandboxd, "sandboxd.sock"))
}

// ProbeWait retries Probe until it passes or wait runs out (a unit start waiting for sandboxd).
func ProbeWait(ctx context.Context, paths Paths, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := Probe(ctx, paths)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func mainPID(ctx context.Context, unit string) (int, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "MainPID", "--value", unit).Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s is not running", unit)
	}
	return pid, nil
}

// socketHolders lists the processes with an open descriptor on any unix socket bound to path,
// read from /proc as fuser does; processes this user cannot inspect are skipped, as fuser
// skips them.
func socketHolders(path string) (map[int]bool, error) {
	inodes, err := socketInodes(path)
	if err != nil {
		return nil, err
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	holders := map[int]bool{}
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil {
			continue
		}
		fds := filepath.Join("/proc", proc.Name(), "fd")
		entries, err := os.ReadDir(fds)
		if err != nil {
			continue // exited, or another user's process
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(fds, entry.Name()))
			if err != nil {
				continue // closed while reading
			}
			if inode, ok := strings.CutPrefix(target, "socket:["); ok && inodes[strings.TrimSuffix(inode, "]")] {
				holders[pid] = true
				break
			}
		}
	}
	return holders, nil
}

func sortedPIDs(set map[int]bool) []int {
	pids := make([]int, 0, len(set))
	for pid := range set {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// socketInodes lists the inodes of the unix sockets bound to path, from /proc/net/unix.
func socketInodes(path string) (map[string]bool, error) {
	file, err := os.Open("/proc/net/unix")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	inodes := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// Num RefCount Protocol Flags Type St Inode Path
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 8 && fields[7] == path {
			inodes[fields[6]] = true
		}
	}
	return inodes, scanner.Err()
}

// listTimeout bounds the list call; a daemon that hangs is as unhealthy as one that is down.
var listTimeout = 10 * time.Second

func listSandboxes(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		// Like curl without -L: a redirect is an answer, not something to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/sandbox", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("sandboxd does not answer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return fmt.Errorf("sandboxd answered %s", response.Status)
	}
	// The whole answer must arrive within the deadline, as curl's did, and be the list.
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("sandboxd answer cut off: %w", err)
	}
	var sandboxes []json.RawMessage
	if err := json.Unmarshal(body, &sandboxes); err != nil || sandboxes == nil {
		return errors.New("sandboxd answered something other than the sandbox list")
	}
	return nil
}

// Suspended reports whether name has an unresolved phase record: a record whose phase is not
// "complete", or one that cannot be read, suspends the name until it is resolved.
func Suspended(paths Paths, name string) bool {
	data, err := os.ReadFile(filepath.Join(paths.Pairs, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	// A map, not a struct: Go matches struct fields case-insensitively, jq does not.
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		return true
	}
	return record["phase"] != "complete"
}

// Lock is the held host-wide maintenance lock. Keep it referenced until done: Release frees
// it, and so does process exit, but Go also closes an unreferenced file, which frees it early.
type Lock struct {
	file *os.File
}

// ErrBusy means another maintenance workflow on this host holds the lock.
var ErrBusy = errors.New("another maintenance workflow is active")

// Acquire takes the host-wide maintenance lock without waiting: flock(2) on the same file every
// tool on this host locks, so they exclude each other.
func Acquire(paths Paths) (*Lock, error) {
	return AcquireFile(filepath.Join(paths.Pairs, "maintenance.lock"))
}

// AcquireFile takes an exclusive flock(2) on path without waiting, creating it (and its folder,
// private to the user) when missing. ErrBusy means another process holds it.
func AcquireFile(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

// Release frees the lock.
func (l *Lock) Release() error {
	return l.file.Close()
}

// VMGone reports whether no VM process for name is left: every containerd task whose config
// names that hostname has a shim that has exited. This reads containerd's own state on the
// host and never execs into the VM, so it cannot boot a stopped one.
func VMGone(paths Paths, name string) bool {
	tasks := filepath.Join(paths.Sandboxd, "containerd", "state", "io.containerd.runtime.v2.task", "docker")
	entries, err := os.ReadDir(tasks)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		dir := filepath.Join(tasks, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false
		}
		var config struct {
			Hostname *string `json:"hostname"`
		}
		if json.Unmarshal(data, &config) != nil || config.Hostname == nil {
			return false
		}
		if *config.Hostname != name {
			continue
		}
		shim, err := os.ReadFile(filepath.Join(dir, "shim.pid"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(shim)))
		if err != nil || pid <= 0 {
			return false
		}
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
			return false
		}
	}
	return true
}
