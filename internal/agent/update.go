package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/starknet-innovation/troupe/internal/guest"
)

// Change is one rewritten, added or deleted skill, script or SOUL.md.
type Change struct {
	Action string // rewrite, add or delete
	Name   string
	// Overwrites means the agent had edited it since troupe installed it: its copy is kept.
	Overwrites bool
}

// fileChange is one file an update writes (After set) or deletes (After empty).
type fileChange struct {
	Before      *string `json:"before"`
	After       *string `json:"after"`
	B64         string  `json:"b64,omitempty"`
	OriginalB64 *string `json:"original_b64"`
	Skill       string  `json:"skill,omitempty"`
}

// Kind is what an update covers: the agent's seeded skills, scripts or SOUL.md.
type Kind string

// Plan is what an update will do. Its JSON is the receipt's plan.json; its files keep the
// earlier operator's format (only the seeds hash replaced the commit), so receipts of either
// can be confirmed.
type Plan struct {
	ID            string                 `json:"id"`
	Agent         string                 `json:"agent"`
	SeedsSHA      string                 `json:"seeds_sha256"`
	Kind          Kind                   `json:"kind,omitempty"`
	Restart       bool                   `json:"restart"`
	Files         map[string]*fileChange `json:"files"`
	Removals      []string               `json:"removals"`
	AddedSkills   []string               `json:"added_skills"`
	RemovedSkills []string               `json:"removed_skills"`
	Changes       []Change               `json:"changes,omitempty"`
	Notes         []string               `json:"notes,omitempty"`
	// overwritten holds the agent's edited files this update replaces, kept in the receipt.
	overwritten map[string][]byte
}

// guestJSON runs one action of the update guest job with a request and decodes its answer.
func (a *Agent) guestJSON(ctx context.Context, timeout time.Duration, action string, request any) (map[string]any, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	out, runErr := guest.RunRequest(ctx, a.Runner, timeout, a.Name, body, "update", action)
	if ctx.Err() != nil {
		return nil, ErrInterrupted
	}
	var result map[string]any
	if json.Unmarshal(lastLine(out), &result) != nil {
		if runErr == nil {
			runErr = errors.New("unreadable answer")
		}
		return nil, fmt.Errorf("guest %s: %w", action, runErr)
	}
	return result, runErr
}

// receiptsDir is where every update and restart of this agent keeps its receipts.
func receiptsDir(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "live-apply", name)
}

// newReceipt creates a private, timestamped receipt folder, its own even when another was
// made the same second (an upgrade applies skills and then scripts).
func newReceipt(name string) (string, error) {
	base := filepath.Join(receiptsDir(name), time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", err
	}
	dir := base
	for n := 2; ; n++ {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) || n > 9 {
			return "", err
		}
		dir = fmt.Sprintf("%s-%d", base, n)
	}
}

func writePrivate(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	temporary := file + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, file)
}

func writeJSON(file string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(file, data)
}

// writeSummary records a receipt's outcome in the earlier operator's format.
func writeSummary(receipt, status, stage string, code int) {
	writePrivate(filepath.Join(receipt, "summary.txt"),
		[]byte(fmt.Sprintf("status=%s\nstage=%s\nexit_status=%d\n", status, stage, code)))
}

// readAnswer is update read's answer. It is decoded as a stream into small typed values with
// counts checked as it goes: decoded into generic maps, a crafted answer would inflate many
// times over in host memory before any check could run.
type readAnswer struct {
	Status  string      `json:"status"`
	Class   string      `json:"class"`
	Files   liveFiles   `json:"files"`
	Running runningList `json:"running"`
}

// liveFile is one live file the guest read: its bytes, or why it could not.
type liveFile struct {
	B64   *string `json:"b64"`
	Error *string `json:"error"`
}

// liveFiles is the guest's file listing, at most maxLiveFiles entries.
type liveFiles map[string]liveFile

func (f *liveFiles) UnmarshalJSON(data []byte) error {
	*f = liveFiles{}
	return eachEntry(data, maxLiveFiles, func(decoder *json.Decoder, key string) error {
		var file liveFile
		if err := decoder.Decode(&file); err != nil {
			return err
		}
		(*f)[key] = file
		return nil
	})
}

// runningList is the ids of work running in the gateway, at most maxRunning.
type runningList []string

func (r *runningList) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errors.New("running: not a list")
	}
	for decoder.More() {
		if len(*r) == maxRunning {
			return errors.New("running: too many entries")
		}
		var entry struct {
			ID json.RawMessage `json:"id"`
		}
		if err := decoder.Decode(&entry); err != nil {
			return err
		}
		// An id is a short string or number; anything else (a nested object would decode into
		// generic maps) is refused.
		if len(entry.ID) == 0 || string(entry.ID) == "null" {
			continue
		}
		if len(entry.ID) > 128 {
			return errors.New("running: id too long")
		}
		var id string
		var number json.Number
		if json.Unmarshal(entry.ID, &id) != nil {
			if json.Unmarshal(entry.ID, &number) != nil {
				return errors.New("running: id is not a string or number")
			}
			id = number.String()
		}
		*r = append(*r, id)
	}
	return nil
}

// eachEntry walks a JSON object's entries one at a time, refusing more than limit.
func eachEntry(data []byte, limit int, each func(*json.Decoder, string) error) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("not an object")
	}
	for count := 0; decoder.More(); count++ {
		if count == limit {
			return errors.New("too many entries")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, _ := token.(string)
		if err := each(decoder, key); err != nil {
			return err
		}
	}
	return nil
}

// maxRunning is far above the work a gateway runs at once.
const maxRunning = 1000

// guestRead runs update read and decodes its answer as a counted stream.
func (a *Agent) guestRead(ctx context.Context, request any) (*readAnswer, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	out, runErr := guest.RunRequest(ctx, a.Runner, 120*time.Second, a.Name, body, "update", "read")
	if ctx.Err() != nil {
		return nil, ErrInterrupted
	}
	var answer readAnswer
	if err := json.Unmarshal(lastLine(out), &answer); err != nil {
		if runErr == nil {
			runErr = errors.New("unreadable answer")
		}
		return nil, fmt.Errorf("guest read: %w", runErr)
	}
	return &answer, runErr
}

// maxLiveFiles is far above the files any agent's seeded skills, scripts and SOUL hold.
const maxLiveFiles = 5000

// verifyFiles checks every planned file's hash, owner and modes in the guest.
func (a *Agent) verifyFiles(ctx context.Context, plan *Plan) error {
	paths := make([]string, 0, len(plan.Files))
	for rel := range plan.Files {
		paths = append(paths, rel)
	}
	state, err := a.guestJSON(ctx, 120*time.Second, "state", map[string]any{"files": paths})
	if err != nil {
		return Failure{"verify", "guest_state_failed"}
	}
	got, _ := state["files"].(map[string]any)
	for rel, f := range plan.Files {
		entry, _ := got[rel].(map[string]any)
		if f.After == nil {
			if entry != nil {
				return Failure{"verify", "not_deleted " + rel}
			}
			continue
		}
		if entry == nil || entry["sha256"] != *f.After {
			return Failure{"verify", "hash_mismatch " + rel}
		}
		if entry["mode"] != "0o600" || fmt.Sprint(entry["uid"]) != "1000" || entry["parent_mode"] != "0o700" {
			return Failure{"verify", "permissions " + rel}
		}
	}
	return nil
}
