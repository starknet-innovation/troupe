package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Receipt is one finished run's folder, named by its time.
type Receipt struct {
	When       time.Time
	Name, Path string
}

// ReceiptTime reads a folder name: recreate and migrate folders are named
// 2026-10-09T23:47:00Z, update folders 20261010T000818Z, and a second update in the same
// second 20261010T000818Z-2 (up to -9, which still sorts by name after the first).
func ReceiptTime(name string) (time.Time, bool) {
	if n := len(name); n > 2 && name[n-2] == '-' && name[n-1] >= '2' && name[n-1] <= '9' {
		name = name[:n-2]
	}
	for _, layout := range []string{"2006-01-02T15:04:05Z", "20060102T150405Z"} {
		if when, err := time.Parse(layout, name); err == nil {
			return when, true
		}
	}
	return time.Time{}, false
}

// Receipts are the folder's receipts holding the required files, oldest first; a name that
// is no time is skipped.
func Receipts(folder string, required ...string) []Receipt {
	entries, _ := os.ReadDir(folder)
	var found []Receipt
	for _, entry := range entries {
		when, ok := ReceiptTime(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(folder, entry.Name())
		complete := true
		for _, name := range required {
			if info, err := os.Stat(filepath.Join(path, name)); err != nil || !info.Mode().IsRegular() {
				complete = false
			}
		}
		if complete {
			found = append(found, Receipt{when, entry.Name(), path})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].When.Equal(found[j].When) {
			return found[i].When.Before(found[j].When)
		}
		return found[i].Name < found[j].Name
	})
	return found
}

// InstalledError is why what troupe installed is unknown; its text is the class.
type InstalledError string

func (e InstalledError) Error() string { return string(e) }

// Overwrite is an agent's edited copy an applied update replaced, kept in its receipt, with
// the hash the seeds had written there before that update.
type Overwrite struct {
	Stamp, Rel, Path, Replaced string
}

// Installed is what troupe last wrote to each file of an agent's ~/.hermes: the last
// recreate's seed manifest, then every update applied after it, oldest first (an update's
// after hash, or no file when it deleted one). It is read from troupe's own receipts on this
// host, so it never depends on how the kits are stored.
type Installed struct {
	Reseed Receipt
	// SHA256 is the hash troupe last wrote, by path under ~/.hermes.
	SHA256 map[string]string
	// Updates are the applied updates' receipt names, oldest first.
	Updates []string
	// Overwritten are the agent's edited copies those updates replaced, so an overwritten
	// lesson is still reviewed before a recreate erases the receipt's last trace.
	Overwritten []Overwrite
	applied     []Receipt
}

// ReadInstalled reads an agent's last completed recreate under reseeds (STATE/AGENT/reseed/completed)
// and the updates applied since under updates (live-apply/AGENT).
func ReadInstalled(reseeds, updates string) (*Installed, error) {
	runs := Receipts(reseeds, "seed-manifest.json", "cutover.json")
	if len(runs) == 0 {
		return nil, InstalledError("no_completed_reseed")
	}
	reseed := runs[len(runs)-1]
	var manifest struct {
		SHA256 map[string]string `json:"sha256"`
	}
	data, err := os.ReadFile(filepath.Join(reseed.Path, "seed-manifest.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		return nil, InstalledError("reseed_receipt_unreadable")
	}
	in := &Installed{Reseed: reseed, SHA256: map[string]string{}, Updates: []string{}, Overwritten: []Overwrite{}}
	for rel, sha := range manifest.SHA256 {
		in.SHA256[rel] = sha
	}
	for _, run := range Receipts(updates, "plan.json", "install.json") {
		if !run.When.After(reseed.When) {
			continue
		}
		// An install that never answered (interrupted, timed out) may not have written anything:
		// leaving the base where it was reports those files instead of hiding them.
		var install map[string]any
		data, err := os.ReadFile(filepath.Join(run.Path, "install.json"))
		if err != nil || json.Unmarshal(data, &install) != nil || install["status"] != "passed" {
			continue
		}
		var plan map[string]any
		data, err = os.ReadFile(filepath.Join(run.Path, "plan.json"))
		if err != nil || json.Unmarshal(data, &plan) != nil {
			return nil, InstalledError("update_receipt_unreadable")
		}
		files, ok := plan["files"].(map[string]any)
		if !ok {
			return nil, InstalledError("update_receipt_unreadable")
		}
		changes := map[string]any{}
		for rel, f := range files {
			item, ok := f.(map[string]any)
			after, present := item["after"]
			if !ok || !present {
				return nil, InstalledError("update_receipt_unreadable")
			}
			changes[rel] = after
		}
		kept := filepath.Join(run.Path, "overwritten")
		var copies []Overwrite
		filepath.WalkDir(kept, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(kept, path)
			rel = filepath.ToSlash(rel)
			copies = append(copies, Overwrite{run.Name, rel, path, in.SHA256[rel]})
			return nil
		})
		sort.Slice(copies, func(i, j int) bool { return copies[i].Path < copies[j].Path })
		in.Overwritten = append(in.Overwritten, copies...)
		for rel, after := range changes {
			switch after := after.(type) {
			case nil:
				delete(in.SHA256, rel)
			case string:
				in.SHA256[rel] = after
			}
		}
		in.Updates = append(in.Updates, run.Name)
		in.applied = append(in.applied, run)
	}
	return in, nil
}

// Bytes is the content troupe wrote to rel with this hash and the receipt that holds it: an
// applied update's plan (newest first), else the recreate's seeds/ copy. Receipts written
// before troupe kept those bytes hold none.
func (in *Installed) Bytes(rel, sha string) (string, []byte) {
	if sha == "" {
		return "", nil
	}
	for i := len(in.applied) - 1; i >= 0; i-- {
		var plan struct {
			Files map[string]struct {
				After *string `json:"after"`
				B64   string  `json:"b64"`
			} `json:"files"`
		}
		data, err := os.ReadFile(filepath.Join(in.applied[i].Path, "plan.json"))
		if err != nil || json.Unmarshal(data, &plan) != nil {
			continue
		}
		f, ok := plan.Files[rel]
		if !ok || f.After == nil || *f.After != sha || f.B64 == "" {
			continue
		}
		if content, err := base64.StdEncoding.DecodeString(f.B64); err == nil && digest(content) == sha {
			return in.applied[i].Name, content
		}
	}
	seeds := filepath.Join(in.Reseed.Path, "seeds")
	path := filepath.Join(seeds, filepath.FromSlash(rel))
	if inside, err := filepath.Rel(seeds, path); err != nil || !filepath.IsLocal(inside) {
		return "", nil
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	if content, err := os.ReadFile(path); err == nil && digest(content) == sha {
		return in.Reseed.Name, content
	}
	return "", nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
