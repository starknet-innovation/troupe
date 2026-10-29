package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/starknet-innovation/troupe/internal/progress"
)

// Fingerprint names exactly what a plan writes and deletes, from what; --yes must match the
// fingerprint of the last dry run.
func (p *Plan) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %v\n", p.Agent, p.SeedsSHA, p.Restart)
	paths := make([]string, 0, len(p.Files))
	for rel := range p.Files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		f := p.Files[rel]
		fmt.Fprintf(&b, "%s %s %s\n", rel, deref(f.Before), deref(f.After))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:12]
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func hashOf(data []byte) *string {
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	return &h
}

func b64(data []byte) *string {
	s := base64.StdEncoding.EncodeToString(data)
	return &s
}

// PlanUpdate compares the seeds of kind with the running agent and lists every change. What
// troupe last installed (its own receipts) tells the agent's edits from older seed versions.
func (a *Agent) PlanUpdate(ctx context.Context, seeds Seeds, kind Kind) (*Plan, error) {
	installed, err := ReadInstalled(filepath.Join(a.stateRoot(), a.Name, "reseed", "completed"), receiptsDir(a.Name))
	var unknown InstalledError
	if errors.As(err, &unknown) {
		return nil, Failure{"reseed", string(unknown)}
	}
	if err != nil {
		return nil, err
	}
	items, err := seeds.Items(kind, installed.SHA256)
	if err != nil {
		return nil, err
	}
	request := map[string]any{"folders": []string{}, "files": []string{}}
	for _, it := range items {
		if kind == Skills {
			request["folders"] = append(request["folders"].([]string), it.root)
		} else {
			request["files"] = append(request["files"].([]string), it.root)
		}
	}
	// At most maxLiveFiles files, read while the host-wide lock is held, so a VM must not be
	// able to keep it for hours with a huge listing.
	answer, err := a.guestRead(ctx, request)
	if err != nil {
		if errors.Is(err, ErrInterrupted) {
			return nil, err
		}
		return nil, Failure{"reseed", "live_files_unreadable"}
	}
	live := map[string][]byte{}
	for rel, entry := range answer.Files {
		// The answer comes from inside the agent's VM: trust no path in it.
		if !insideItems(rel, items) {
			return nil, Failure{"reseed", "live_path_not_allowed " + strconv.Quote(rel)}
		}
		if entry.Error != nil {
			return nil, Failure{"reseed", fmt.Sprintf("live_%s %s", Plain(*entry.Error), strconv.QuoteToASCII(rel))}
		}
		if entry.B64 == nil {
			return nil, Failure{"reseed", "live_files_unreadable"}
		}
		data, err := base64.StdEncoding.DecodeString(*entry.B64)
		if err != nil {
			return nil, Failure{"reseed", "live_files_unreadable"}
		}
		live[rel] = data
	}

	plan := &Plan{Agent: a.Name, SeedsSHA: itemsSHA(items), Kind: kind, Files: map[string]*fileChange{},
		Removals: []string{}, AddedSkills: []string{}, RemovedSkills: []string{}, overwritten: map[string][]byte{}}
	for _, it := range items {
		current := map[string][]byte{}
		for rel, data := range live {
			if rel == it.root || strings.HasPrefix(rel, it.root+"/") {
				current[rel] = data
			}
		}
		replaced := map[string][]byte{} // live files this item overwrites or deletes
		for rel, data := range it.seed {
			old, present := current[rel]
			if present && bytes.Equal(old, data) {
				continue
			}
			change := &fileChange{After: hashOf(data), B64: base64.StdEncoding.EncodeToString(data)}
			if present {
				change.Before, change.OriginalB64 = hashOf(old), b64(old)
				replaced[rel] = old
			}
			plan.Files[rel] = change
		}
		for rel, old := range current {
			if _, seeded := it.seed[rel]; !seeded {
				plan.Files[rel] = &fileChange{Before: hashOf(old), OriginalB64: b64(old)}
				replaced[rel] = old
			}
		}
		changed := false
		for rel := range plan.Files {
			if rel == it.root || strings.HasPrefix(rel, it.root+"/") {
				changed = true
			}
		}
		if !changed {
			continue
		}
		change := Change{Name: it.name, Action: "rewrite"}
		for rel, data := range replaced {
			// Not what troupe last wrote there (or never written by troupe): the agent's edit.
			if installed.SHA256[rel] != digest(data) {
				change.Overwrites = true
				plan.overwritten[rel] = data
			}
		}
		switch {
		case len(current) == 0:
			change.Action = "add"
		case len(it.seed) == 0:
			change.Action = "delete"
		}
		plan.Changes = append(plan.Changes, change)
		if kind == Skills {
			skillFile := it.root + "/SKILL.md"
			switch {
			case change.Action == "add":
				plan.AddedSkills = append(plan.AddedSkills, it.name)
				plan.Restart = true
			case change.Action == "delete":
				plan.RemovedSkills = append(plan.RemovedSkills, it.name)
				plan.Restart = true
			case frontmatter(current[skillFile]) != frontmatter(it.seed[skillFile]):
				// Hermes caches skill names and descriptions per process.
				plan.Restart = true
			}
		}
		if kind == Soul {
			plan.Notes = append(plan.Notes, "Conversations pick up the new SOUL.md after /new")
		}
	}
	plan.ID = plan.Fingerprint()
	return plan, nil
}

// insideItems accepts only a clean relative path inside one of the items being updated.
func insideItems(rel string, items []item) bool {
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == ".." ||
		strings.ContainsAny(rel, "\x00\\") || !utf8.ValidString(rel) ||
		strings.IndexFunc(rel, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return false
	}
	for _, it := range items {
		if rel == it.root || strings.HasPrefix(rel, it.root+"/") {
			return true
		}
	}
	return false
}

// frontmatter is a SKILL.md's leading --- block, which holds its name and description.
func frontmatter(data []byte) string {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return ""
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return ""
	}
	return text[4 : 4+end]
}

// PrintPlan writes one progress line per change.
func (a *Agent) PrintPlan(p *Plan) {
	for _, c := range p.Changes {
		what := "skill " + c.Name
		switch p.Kind {
		case Scripts:
			what = "script " + c.Name
		case Soul:
			what = c.Name
		}
		line := c.Action + " " + what
		if c.Action == "delete" {
			line += " (removed from the seeds)"
		}
		if c.Overwrites {
			line += fmt.Sprintf(" (overwrites %s's edits; the agent's copy is saved)", a.Printer.Agent)
		}
		a.Printer.Print("reseed", "", line)
	}
	if p.Restart {
		a.Printer.Print("reseed", "", "A graceful restart follows (the skill list or a skill's description changes)")
	}
	for _, note := range p.Notes {
		a.Printer.Print("reseed", "", note)
	}
}

// dryRunRecord is where the last dry run of kind left its fingerprint for --yes.
func dryRunRecord(name string, kind Kind) string {
	return filepath.Join(receiptsDir(name), "dry-run-"+string(kind)+".json")
}

// UpdateOptions are how an update was asked for.
type UpdateOptions struct {
	DryRun, Yes bool
	// Confirm asks the operator; nil means there is no terminal to ask on.
	Confirm func(question string) bool
}

// Update makes the agent's seeded files of kind match the seeds.
func (a *Agent) Update(ctx context.Context, seeds Seeds, kind Kind, opts UpdateOptions) error {
	release, err := a.liveChecks(ctx)
	if err != nil {
		return err
	}
	defer release()
	plan, err := a.PlanUpdate(ctx, seeds, kind)
	if err != nil {
		return err
	}
	if len(plan.Changes) == 0 {
		what := fmt.Sprintf("%s's %s already match", a.Printer.Agent, kind)
		if kind == Soul {
			what = fmt.Sprintf("%s's SOUL.md already matches", a.Printer.Agent)
		}
		a.Printer.Print("reseed", progress.OK, what+" the seeds")
		return nil
	}
	a.PrintPlan(plan)
	record := dryRunRecord(a.Name, kind)
	if opts.DryRun {
		if err := writeJSON(record, map[string]string{"fingerprint": plan.Fingerprint()}); err != nil {
			return err
		}
		a.Printer.Print("reseed", progress.OK, "Dry run, nothing changed")
		a.Printer.Print("reseed", progress.Action, fmt.Sprintf("To apply exactly this, run: troupe reseed %s %s --yes", kind, a.Name))
		return nil
	}
	if opts.Yes {
		var last map[string]string
		data, err := os.ReadFile(record)
		if err != nil || json.Unmarshal(data, &last) != nil {
			return Failure{"reseed", "no_dry_run; run with --dry-run first, then --yes"}
		}
		if last["fingerprint"] != plan.Fingerprint() {
			return Failure{"reseed", "changed_since_dry_run; nothing changed, run --dry-run again"}
		}
	} else {
		if opts.Confirm == nil {
			return Failure{"reseed", "no_terminal_to_confirm; run with --dry-run, then --yes"}
		}
		noun := "changes"
		if len(plan.Changes) == 1 {
			noun = "change"
		}
		if !opts.Confirm(fmt.Sprintf("Apply these %d %s to %s? [y/N] ", len(plan.Changes), noun, a.Printer.Agent)) {
			a.Printer.Print("reseed", progress.Skipped, "Nothing changed")
			return nil
		}
	}
	os.Remove(record)
	return a.applyPlan(ctx, plan)
}

// forceInstallFailure makes the guest fail its install after the first write, to test the
// rollback; only the dev build can turn it on (update_hook_dev.go).
var forceInstallFailure = func() bool { return false }

// applyPlan installs a plan, verifies the files and, when needed, restarts gracefully.
func (a *Agent) applyPlan(ctx context.Context, plan *Plan) error {
	receipt, err := newReceipt(a.Name)
	if err != nil {
		return err
	}
	a.Runner.Log = filepath.Join(receipt, "native.log")
	if err := writeJSON(filepath.Join(receipt, "plan.json"), plan); err != nil {
		return err
	}
	kept := filepath.Join(receipt, "overwritten")
	for rel, data := range plan.overwritten {
		target := filepath.Join(kept, filepath.FromSlash(rel))
		// Belt and braces: never write outside the receipt, whatever the path says.
		if inside, err := filepath.Rel(kept, target); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return Failure{"reseed", "live_path_not_allowed " + strconv.Quote(rel)}
		}
		if err := writePrivate(target, data); err != nil {
			return err
		}
	}
	if plan.Restart {
		// Refuse before installing when the gateway is not running or already restarting.
		if _, err := a.gatewayPID(ctx); err != nil {
			writeSummary(receipt, "failed", "preflight", 1)
			return err
		}
	}
	files := map[string]any{}
	for rel, f := range plan.Files {
		files[rel] = map[string]any{"before": f.Before, "after": f.After, "b64": f.B64, "original_b64": f.OriginalB64}
	}
	answer, err := a.guestJSON(ctx, 180*time.Second, "install",
		map[string]any{"files": files, "fail_after_first_write": forceInstallFailure()})
	writeJSON(filepath.Join(receipt, "install.json"), answer)
	if err != nil || answer["status"] != "passed" {
		writeSummary(receipt, "failed", "install", 1)
		if answer != nil && answer["class"] != nil {
			if restored, _ := answer["restored"].(bool); !restored && answer["restored"] != nil {
				return Failure{"install", fmt.Sprintf("install_failed %s; the rollback was incomplete, run the dry run again to see what changed", Plain(answer["class"]))}
			}
			// The guest answered: it refused or rolled back, so live files are unchanged.
			return Failure{"install", fmt.Sprintf("install_failed %s; live files are unchanged", Plain(answer["class"]))}
		}
		// No answer (interrupted, timed out): the guest may still have finished.
		if errors.Is(err, ErrInterrupted) {
			return err
		}
		return Failure{"install", "install_result_unknown; run the dry run again to see what changed"}
	}
	pruneReceipts(a.Name, receipt)
	if plan.Restart {
		// Restart right after the install, as the skill index must reload; the confirmation
		// verifies the files, so a failed check can never leave the restart unsent.
		a.Printer.Print("install", progress.OK, fmt.Sprintf("%d file(s) written. Receipt: %s", len(plan.Files), receipt))
		return a.restartAndConfirm(ctx, receipt, plan)
	}
	if err := a.verifyFiles(ctx, plan); err != nil {
		writeSummary(receipt, "failed", "verify", 1)
		return err
	}
	a.Printer.Print("install", progress.OK, fmt.Sprintf("%d file(s) written and verified. Receipt: %s", len(plan.Files), receipt))
	writeSummary(receipt, "passed", "done", 0)
	return nil
}

// pruneReceipts keeps the live files' former contents only in the newest receipt; what troupe
// wrote (the seeds' bytes, the base later edits are measured against) and the agent's
// overwritten copies are kept in every receipt.
func pruneReceipts(name, current string) {
	plans, _ := filepath.Glob(filepath.Join(receiptsDir(name), "*", "plan.json"))
	for _, file := range plans {
		if filepath.Dir(file) == current {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var value map[string]any
		if json.Unmarshal(data, &value) != nil {
			continue
		}
		files, _ := value["files"].(map[string]any)
		pruned := false
		for _, v := range files {
			if entry, ok := v.(map[string]any); ok {
				for _, key := range []string{"original_b64"} {
					if s, ok := entry[key].(string); ok && s != "" {
						delete(entry, key)
						pruned = true
					}
				}
			}
		}
		if pruned {
			writeJSON(file, value)
		}
	}
}

// TerminalConfirm asks on a terminal and accepts only y or yes.
func TerminalConfirm(in io.Reader, out io.Writer) func(string) bool {
	return func(question string) bool {
		fmt.Fprint(out, question)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes"
	}
}

// ApplySeeds makes the agent's seeded skills and scripts match the new kit version without
// asking: the last step of an upgrade, after the move restored the agent's own home. The seeds
// win, as with troupe reseed: the agent's lessons are expected in the new version already
// (harvested into the kit before it was published), and every file of the agent's own it
// replaces is kept in that change's receipt. SOUL.md is the agent's own folder's, not the
// kit's, so an upgrade leaves it as it is.
func (a *Agent) ApplySeeds(ctx context.Context, seeds Seeds) error {
	release, err := a.liveChecks(ctx)
	if err != nil {
		return err
	}
	defer release()
	changed := 0
	for _, kind := range []Kind{Skills, Scripts} {
		plan, err := a.PlanUpdate(ctx, seeds, kind)
		if err != nil {
			return err
		}
		if len(plan.Changes) == 0 {
			continue
		}
		a.PrintPlan(plan)
		if err := a.applyPlan(ctx, plan); err != nil {
			return err
		}
		changed += len(plan.Changes)
	}
	if changed == 0 {
		a.Printer.Print("reseed", progress.OK, a.Printer.Agent+"'s skills and scripts already match the new kit")
	}
	return nil
}
