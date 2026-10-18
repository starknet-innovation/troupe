package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// writeReceipt replaces path atomically with v as JSON, private to the user and synced, so a
// receipt is either absent or complete.
func writeReceipt(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func writeFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

// readReceipt decodes a receipt; false when it is absent, empty or not a JSON object.
func readReceipt(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

func detachedProcess() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// A timer unit name troupe may stop and start.
var timerName = regexp.MustCompile(`^[A-Za-z0-9@._-]+\.timer$`)

// collectorRecord is the pause record every lifecycle tool on this host shares (the bash pair
// guards use the same file and format): it holds the collector timer's state before the first
// pause, so whichever tool restores it puts back what was there.
func (a *Agent) collectorRecord() string { return filepath.Join(a.Paths.Pairs, "collector.json") }

// pauseCollector stops the collector timer for the run and waits for any run of its service
// already in flight: killing it could leave an sbx client acting inside a VM.
func (r *recreation) pauseCollector(ctx context.Context) error {
	timer := r.o.CollectorTimer
	if timer == "" {
		return nil
	}
	if !timerName.MatchString(timer) {
		return Failure{"preflight", "invalid_collector_timer"}
	}
	if _, err := os.Stat(r.collectorRecord()); errors.Is(err, os.ErrNotExist) {
		out, _ := exec.Command("systemctl", "is-active", timer).Output()
		state := strings.TrimSpace(string(out))
		if state != "active" && state != "inactive" {
			return Failure{"preflight", "collector_timer_state_unknown"}
		}
		if err := os.MkdirAll(r.Paths.Pairs, 0o700); err != nil {
			return err
		}
		if err := writeReceipt(r.collectorRecord(), map[string]string{"previous_state": state}); err != nil {
			return err
		}
	}
	r.paused = true
	if err := r.systemctl("stop", timer); err != nil {
		return Failure{"preflight", "collector_timer_stop_failed"}
	}
	service := strings.TrimSuffix(timer, ".timer") + ".service"
	deadline := time.Now().Add(6 * time.Minute)
	for {
		out, _ := exec.Command("systemctl", "show", service, "-p", "MainPID", "--value").Output()
		if pid := strings.TrimSpace(string(out)); pid == "0" || pid == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return Failure{"preflight", "collector_did_not_drain"}
		}
		if err := sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// restoreCollector starts the collector timer again when it ran before the first pause, once
// no disposable copy of any agent on this host is mid-copy, and removes the shared record.
func (r *recreation) restoreCollector() error {
	var record struct {
		Previous string `json:"previous_state"`
	}
	if _, err := os.Stat(r.collectorRecord()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !readReceipt(r.collectorRecord(), &record) {
		return errors.New("unreadable collector record")
	}
	if !r.noCopies() {
		return errors.New("a disposable copy is mid-copy")
	}
	switch record.Previous {
	case "active":
		if r.o.CollectorTimer == "" || r.systemctl("start", r.o.CollectorTimer) != nil {
			return errors.New("collector timer not started")
		}
	case "inactive":
	default:
		return errors.New("unreadable collector record")
	}
	return os.Remove(r.collectorRecord())
}

// noCopies: every phase record under the shared pairs folder is complete and names a copy that
// is gone. Like the bash guard, an unreadable record or one without a copy fails closed.
func (r *recreation) noCopies() bool {
	records, _ := filepath.Glob(filepath.Join(r.Paths.Pairs, "*.json"))
	// One inventory for every record: each listing costs seconds, and nothing creates a copy
	// while this run holds the host workflow lock.
	var inventory map[string]sbx.Record
	listed := false
	for _, path := range records {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		if name == "collector" {
			continue
		}
		if guard.Suspended(r.Paths, name) {
			return false
		}
		// A map, not a struct: struct fields would match "Copy" or "COPY" too.
		var record map[string]any
		if !readReceipt(path, &record) {
			return false
		}
		copyName, ok := record["copy"].(string)
		if !ok || copyName == "" {
			return false
		}
		if !listed {
			var err error
			if inventory, err = r.Runner.Inventory(context.Background()); err != nil {
				return false
			}
			listed = true
		}
		if _, exists := inventory[copyName]; exists || !guard.VMGone(r.Paths, copyName) {
			return false
		}
	}
	return true
}
