//go:build dev

package harvest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

var harvestHelp = help.Render(help.Command{
	Summary: "Collect an agent's own skill edits before a reset, update or upgrade replaces them.",
	Usage:   []string{"harvest AGENT"},
	Args:    []help.Arg{{Name: "AGENT", Desc: "Any agent troupe knows, of the kits root or a published kit."}},
	Notes: []string{
		"Read only: never changes the seeds or the agent. Runs the sandboxd probe before any sbx call.",
		"Prints one status line per file; writes the diffs under ~/.cache/hermes-skill-harvest/AGENT/.",
		"Blending the lessons into the kit's skills is yours. A refusal prints \"blocked CLASS\".",
	},
})

// failure is a refusal: harvest prints "blocked CLASS" and collects nothing more.
type failure string

func (f failure) Error() string { return string(f) }

var (
	agentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// The guest answer is the agent's: only clean, printable skill paths become file names here.
	skillPath = regexp.MustCompile(`^skills/[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+ -]+)*$`)
)

func harvestCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, harvestHelp)
		return 0
	}
	if len(args) != 1 {
		return usageError(e, harvestHelp, "harvest needs one AGENT")
	}
	lines, err := harvest(e, args[0])
	var f failure
	if errors.As(err, &f) {
		fmt.Fprintf(e.err, "blocked %s\n", f)
		return 1
	}
	if err != nil {
		return fail(e, err)
	}
	for _, line := range lines {
		fmt.Fprintln(e.out, line)
	}
	return 0
}

// nextStep tells the user what follows a harvest: troupe collects, and blending the lessons
// into the kit's skills is theirs (by hand or with an LLM); troupe then replaces the agent's skills.
func nextStep(reg registry.Registry, name, baseKit string) string {
	if folder, err := reg.Folder(name); err == nil && filepath.Dir(folder) == filepath.Join(reg.Data, "agents") {
		return "next: blend the lessons worth keeping into the kit's skills (by hand or with an LLM), publish a new version of the kit, then run troupe upgrade " + name
	}
	return "next: blend the lessons worth keeping into the skills under " + filepath.Join(baseKit, "files/home/.hermes") + " (by hand or with an LLM), then run troupe update skills " + name
}

func cleanSkillPath(rel string) bool {
	if !skillPath.MatchString(rel) || strings.HasSuffix(rel, "/") {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// newID is the VM a recreate or migrate receipt's cutover made.
func newID(run string) string {
	var cutover struct {
		NewID string `json:"new_id"`
	}
	data, err := os.ReadFile(filepath.Join(run, "cutover.json"))
	if err == nil {
		json.Unmarshal(data, &cutover)
	}
	return cutover.NewID
}

// liveFile is one file as the guest read it.
type liveFile struct {
	sha, b64 string
}

type ledgerEntry struct {
	ts           string
	paths, after []string
}

// answer is the guest's read: seeded files (nil when absent), the agent's ledger entries and
// the files he created.
type answer struct {
	files, created map[string]*liveFile
	ledger         []ledgerEntry
}

// Counted lists in the guest's answer: far above a real agent's skills and ledger.
const (
	maxCreated     = 10000
	maxLedger      = 100000
	maxLedgerPaths = 1000
)

// parseAnswer takes the guest's last line. A line that is no JSON is guest_read_failed; any
// shape other than the one harvest reads is refused whole as guest_read_invalid. It decodes
// typed values with counted lists, one entry at a time, so an answer within the size cap but
// built to inflate in memory is refused before it is held. Seeded files must be ones requested.
func parseAnswer(out []byte, requested []string) (*answer, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := []byte(lines[len(lines)-1])
	if !json.Valid(last) {
		return nil, failure("guest_read_failed")
	}
	invalid := failure("guest_read_invalid")
	var raw struct {
		Files, Created, Ledger json.RawMessage
	}
	if json.Unmarshal(last, &raw) != nil || raw.Files == nil || raw.Created == nil || raw.Ledger == nil {
		return nil, invalid
	}
	asked := map[string]bool{}
	for _, rel := range requested {
		asked[rel] = true
	}
	files := func(data json.RawMessage, limit int, allowed func(string) bool) (map[string]*liveFile, error) {
		found := map[string]*liveFile{}
		err := eachKey(data, limit, func(decoder *json.Decoder, rel string) error {
			var entry *struct {
				SHA256 *string `json:"sha256"`
				B64    *string `json:"b64"`
			}
			if !allowed(rel) || decoder.Decode(&entry) != nil {
				return invalid
			}
			if entry == nil {
				found[rel] = nil
				return nil
			}
			if entry.SHA256 == nil || entry.B64 == nil {
				return invalid
			}
			found[rel] = &liveFile{*entry.SHA256, *entry.B64}
			return nil
		})
		return found, err
	}
	a := &answer{}
	var err error
	if a.files, err = files(raw.Files, len(requested), func(rel string) bool { return asked[rel] }); err != nil {
		return nil, invalid
	}
	if a.created, err = files(raw.Created, maxCreated, func(string) bool { return true }); err != nil {
		return nil, invalid
	}
	err = eachElement(raw.Ledger, maxLedger, func(decoder *json.Decoder) error {
		var entry struct {
			TS    *string         `json:"ts"`
			Paths json.RawMessage `json:"paths"`
			After json.RawMessage `json:"after"`
		}
		if decoder.Decode(&entry) != nil || entry.TS == nil {
			return invalid
		}
		paths, err := stringList(entry.Paths, maxLedgerPaths)
		if err != nil {
			return invalid
		}
		shas, err := stringList(entry.After, maxLedgerPaths)
		if err != nil || len(shas) != len(paths) {
			return invalid
		}
		a.ledger = append(a.ledger, ledgerEntry{*entry.TS, paths, shas})
		return nil
	})
	if err != nil {
		return nil, invalid
	}
	return a, nil
}

// eachKey walks a JSON object's entries one at a time, refusing more than limit.
func eachKey(data []byte, limit int, each func(*json.Decoder, string) error) error {
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

// eachElement walks a JSON array's elements one at a time, refusing more than limit.
func eachElement(data []byte, limit int, each func(*json.Decoder) error) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errors.New("not an array")
	}
	for count := 0; decoder.More(); count++ {
		if count == limit {
			return errors.New("too many elements")
		}
		if err := each(decoder); err != nil {
			return err
		}
	}
	return nil
}

// stringList reads a JSON array of strings, refusing more than limit.
func stringList(data []byte, limit int) ([]string, error) {
	found := []string{}
	err := eachElement(data, limit, func(decoder *json.Decoder) error {
		var s string
		if err := decoder.Decode(&s); err != nil {
			return err
		}
		found = append(found, s)
		return nil
	})
	return found, err
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// privateWrite writes a private file; every write stays inside this collection's folder,
// whatever a name looked like.
func privateWrite(path string, data []byte, out string) error {
	if rel, err := filepath.Rel(out, filepath.Clean(path)); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return failure("path_outside_scratch")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// readSeed is the current seed's bytes, or none.
func readSeed(path string) []byte {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return []byte{}
	}
	data, _ := os.ReadFile(path)
	return data
}

type fileItem struct {
	Status               string   `json:"status"`
	BaseReceipt          any      `json:"base_receipt"`
	MatchesCurrentSeed   bool     `json:"matches_current_seed"`
	SeedChangedSinceBase bool     `json:"seed_changed_since_base"`
	AgentEdits           []string `json:"agent_edits"`
}

type overwrittenItem struct {
	Path               string `json:"path"`
	Receipt            string `json:"receipt"`
	BaseReceipt        any    `json:"base_receipt"`
	MatchesCurrentSeed bool   `json:"matches_current_seed"`
}

type harvestSummary struct {
	Agent       string            `json:"agent"`
	Reseed      string            `json:"reseed"`
	Migrate     any               `json:"migrate"`
	Updates     []string          `json:"updates"`
	Files       map[string]any    `json:"files"`
	Overwritten []overwrittenItem `json:"overwritten"`
}

// receiptOrNull is the receipt holding a base as the summary names it: null when unknown.
func receiptOrNull(name string) any {
	if name == "" {
		return nil
	}
	return name
}

// harvest collects the agent's skill edits into private scratch and returns the status lines.
func harvest(e env, name string) ([]string, error) {
	if !agentName.MatchString(name) {
		return nil, failure("malformed_agent_name")
	}
	// Skills are seeded by the agent's base kit; only skills are harvested.
	reg, err := e.registry()
	if err != nil {
		return nil, failure("unregistered_or_unresolvable_agent")
	}
	baseKit, err := reg.Base(name)
	if err != nil {
		return nil, failure("unregistered_or_unresolvable_agent")
	}
	seedRoot := filepath.Join(baseKit, "files/home/.hermes")
	installed, err := agent.ReadInstalled(filepath.Join(e.tools.state, name, "reseed/completed"), filepath.Join(e.tools.updates, name))
	var unknown agent.InstalledError
	if errors.As(err, &unknown) {
		return nil, failure(string(unknown))
	}
	if err != nil {
		return nil, err
	}
	reseed, seeded := installed.Reseed, installed.SHA256
	// A migrate keeps the agent's state in a new VM: the latest one after the reseed is the VM to read.
	vm := reseed
	for _, migrate := range agent.Receipts(filepath.Join(e.tools.state, name, "completed"), "cutover.json") {
		if migrate.When.After(reseed.When) {
			vm = migrate
		}
	}
	paths := []string{}
	for rel := range seeded {
		if strings.HasPrefix(rel, "skills/") {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)

	// The sandboxd probe and the pair guard (troupe guard probe and check) before any sbx call.
	probe := e.tools.runner.Probe
	if probe == nil {
		probe = func(ctx context.Context) error { return guard.Probe(ctx, e.tools.runner.Paths) }
	}
	if probe(e.ctx) != nil || guard.Suspended(e.tools.runner.Paths, name) {
		return nil, failure("sandbox_preflight_failed")
	}
	records, err := e.tools.runner.Inventory(e.ctx)
	if errors.Is(err, sbx.ErrProbe) {
		return nil, failure("sandbox_preflight_failed")
	}
	if current := records[name].ID; err != nil || current == "" || current != newID(vm.Path) {
		return nil, failure("running_vm_is_not_the_last_reseed_or_migrate")
	}
	// sbx exec would boot a stopped VM: an agent is read only while it runs.
	if records[name].Status != "running" {
		return nil, failure("agent_not_running")
	}
	request, err := json.Marshal(map[string]any{"paths": paths})
	if err != nil {
		return nil, err
	}
	out, err := guest.RunRequest(e.ctx, e.tools.runner, 90*time.Second, name, request, "harvest")
	if err != nil {
		return nil, failure("guest_read_failed")
	}
	live, err := parseAnswer(out, paths)
	if err != nil {
		return nil, err
	}
	// Seeded files must decode; a created file that does not is only skipped below.
	liveBytes := map[string][]byte{}
	for rel, f := range live.files {
		if f == nil {
			continue
		}
		if liveBytes[rel], err = base64.StdEncoding.Strict().DecodeString(f.b64); err != nil {
			return nil, failure("guest_read_invalid")
		}
	}

	scratch := filepath.Join(e.tools.scratch, name, e.tools.now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return nil, err
	}
	for _, parent := range []string{e.tools.scratch, filepath.Dir(scratch)} {
		if err := os.Chmod(parent, 0o700); err != nil {
			return nil, err
		}
	}
	agentShas := map[string]map[string]bool{}
	for _, entry := range live.ledger {
		for i, rel := range entry.paths {
			if i < len(entry.after) {
				if agentShas[rel] == nil {
					agentShas[rel] = map[string]bool{}
				}
				agentShas[rel][entry.after[i]] = true
			}
		}
	}
	summary := harvestSummary{Agent: name, Reseed: reseed.Name, Updates: installed.Updates, Files: map[string]any{},
		Overwritten: []overwrittenItem{}}
	if vm.Path != reseed.Path {
		summary.Migrate = vm.Name
	}
	var lines []string
	for _, rel := range paths {
		file := live.files[rel]
		if file != nil && file.sha == seeded[rel] {
			continue
		}
		seedNow := readSeed(filepath.Join(seedRoot, rel))
		from, base := installed.Bytes(rel, seeded[rel])
		status := "changed_without_agent_record"
		if file == nil {
			status = "deleted_live"
		} else if agentShas[rel][file.sha] {
			status = "changed_by_agent"
		}
		current := liveBytes[rel]
		if current == nil {
			current = []byte{}
		}
		item := fileItem{Status: status, BaseReceipt: receiptOrNull(from), MatchesCurrentSeed: bytes.Equal(current, seedNow),
			SeedChangedSinceBase: base != nil && !bytes.Equal(seedNow, base), AgentEdits: []string{}}
		for _, entry := range live.ledger {
			for _, p := range entry.paths {
				if p == rel && entry.ts != "" {
					item.AgentEdits = append(item.AgentEdits, entry.ts)
					break
				}
			}
		}
		sort.Strings(item.AgentEdits)
		folder := filepath.Join(scratch, "files", rel)
		names, contents := []string{"live", "seed"}, [][]byte{current, seedNow}
		if base != nil {
			names = append(names, "base", "agent.diff", "seed.diff")
			contents = append(contents, base, unified(base, current, "base", "live"), unified(base, seedNow, "base", "seed"))
		}
		for i, n := range names {
			if err := privateWrite(filepath.Join(folder, n), contents[i], scratch); err != nil {
				return nil, err
			}
		}
		summary.Files[rel] = item
		shown := "unknown"
		if from != "" {
			shown = from
		}
		line := status + " " + rel + " base=" + shown
		if item.SeedChangedSinceBase {
			line += " seed_changed"
		}
		if item.MatchesCurrentSeed {
			line += " already_in_seed"
		}
		lines = append(lines, fmt.Sprintf("%s agent_edits=%d", line, len(item.AgentEdits)))
	}
	for _, o := range installed.Overwritten {
		// Only skills are harvested; the receipt's names become paths here only when clean.
		if !cleanSkillPath(o.Rel) {
			continue
		}
		folder := filepath.Join(scratch, "files", o.Rel)
		data, err := os.ReadFile(o.Path)
		if err == nil {
			err = privateWrite(filepath.Join(folder, "overwritten-"+o.Stamp), data, scratch)
		}
		from := ""
		if err == nil && o.Replaced != "" {
			var base []byte
			if from, base = installed.Bytes(o.Rel, o.Replaced); base != nil {
				err = privateWrite(filepath.Join(folder, "overwritten-"+o.Stamp+".diff"), unified(base, data, "base", "overwritten"), scratch)
			}
		}
		var f failure
		if errors.As(err, &f) {
			return nil, err
		}
		if err != nil {
			lines = append(lines, "skipped one overwritten copy that could not be written")
			continue
		}
		inSeed := bytes.Equal(data, readSeed(filepath.Join(seedRoot, o.Rel)))
		summary.Overwritten = append(summary.Overwritten, overwrittenItem{o.Rel, o.Stamp, receiptOrNull(from), inSeed})
		line := "overwritten_by_update " + o.Rel + " receipt=" + o.Stamp
		if inSeed {
			line += " already_in_seed"
		}
		lines = append(lines, line)
	}
	created := make([]string, 0, len(live.created))
	for rel := range live.created {
		created = append(created, rel)
	}
	sort.Strings(created)
	for _, rel := range created {
		file := live.created[rel]
		if file == nil {
			continue
		}
		if !cleanSkillPath(rel) {
			lines = append(lines, "skipped one created file with an unsafe name")
			continue
		}
		// A seeded path's live copy is the one read with it and diffed against its base.
		if _, isSeeded := seeded[rel]; isSeeded {
			lines = append(lines, "skipped one created file under a seeded path")
			continue
		}
		data, err := base64.StdEncoding.Strict().DecodeString(file.b64)
		if err == nil {
			err = privateWrite(filepath.Join(scratch, "files", rel, "live"), data, scratch)
		}
		if err != nil {
			// Undecodable, or a name that collides with another one (skills/a beside skills/a/live).
			lines = append(lines, "skipped one created file that could not be written")
			continue
		}
		summary.Files[rel] = map[string]string{"status": "new_file"}
		lines = append(lines, "new_file "+rel)
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := privateWrite(filepath.Join(scratch, "summary.json"), encoded, scratch); err != nil {
		return nil, err
	}
	if count := len(summary.Files) + len(summary.Overwritten); count > 0 {
		lines = append(lines, fmt.Sprintf("scratch %s files=%d", scratch, count))
		lines = append(lines, nextStep(reg, name, baseKit))
	} else {
		lines = append(lines, "no agent skill edits to review")
	}
	return lines, nil
}

// unified is a unified diff of a to b with three lines of context, as difflib's unified_diff
// writes it; empty when they are equal.
func unified(a, b []byte, nameA, nameB string) []byte {
	x, y := splitLines(a), splitLines(b)
	type edit struct {
		op   byte // ' ' kept, '-' only in a, '+' only in b
		line string
	}
	// Common head and tail first; the middle is compared line by line (longest common
	// subsequence), or, when too large to compare cheaply, replaced whole: still a correct diff.
	head := 0
	for head < len(x) && head < len(y) && x[head] == y[head] {
		head++
	}
	tail := 0
	for tail < len(x)-head && tail < len(y)-head && x[len(x)-1-tail] == y[len(y)-1-tail] {
		tail++
	}
	var edits []edit
	for _, line := range x[:head] {
		edits = append(edits, edit{' ', line})
	}
	mx, my := x[head:len(x)-tail], y[head:len(y)-tail]
	n, m := len(mx), len(my)
	if n*m <= 1<<22 {
		common := make([]int32, (n+1)*(m+1))
		at := func(i, j int) int32 { return common[i*(m+1)+j] }
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if mx[i] == my[j] {
					common[i*(m+1)+j] = at(i+1, j+1) + 1
				} else {
					common[i*(m+1)+j] = max(at(i+1, j), at(i, j+1))
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && mx[i] == my[j]:
				edits = append(edits, edit{' ', mx[i]})
				i, j = i+1, j+1
			case j == m || (i < n && at(i+1, j) >= at(i, j+1)):
				edits = append(edits, edit{'-', mx[i]})
				i++
			default:
				edits = append(edits, edit{'+', my[j]})
				j++
			}
		}
	} else {
		for _, line := range mx {
			edits = append(edits, edit{'-', line})
		}
		for _, line := range my {
			edits = append(edits, edit{'+', line})
		}
	}
	for _, line := range x[len(x)-tail:] {
		edits = append(edits, edit{' ', line})
	}

	// Hunks: changes closer than twice the context share one.
	const around = 3
	var out bytes.Buffer
	ranges := func(start, count int) string {
		if count == 1 {
			return fmt.Sprint(start + 1)
		}
		if count == 0 {
			return fmt.Sprintf("%d,0", start)
		}
		return fmt.Sprintf("%d,%d", start+1, count)
	}
	// before[k] counts a's and b's lines ahead of edit k.
	beforeA, beforeB := make([]int, len(edits)+1), make([]int, len(edits)+1)
	for k, ed := range edits {
		beforeA[k+1], beforeB[k+1] = beforeA[k], beforeB[k]
		if ed.op != '+' {
			beforeA[k+1]++
		}
		if ed.op != '-' {
			beforeB[k+1]++
		}
	}
	for k := 0; k < len(edits); {
		if edits[k].op == ' ' {
			k++
			continue
		}
		start, last := max(0, k-around), k
		for next := k + 1; next < len(edits); next++ {
			if edits[next].op != ' ' {
				if next-last-1 > 2*around {
					break
				}
				last = next
			}
		}
		end := min(len(edits), last+around+1)
		if out.Len() == 0 {
			fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", ranges(beforeA[start], beforeA[end]-beforeA[start]),
			ranges(beforeB[start], beforeB[end]-beforeB[start]))
		for _, ed := range edits[start:end] {
			out.WriteByte(ed.op)
			out.WriteString(ed.line)
			if !strings.HasSuffix(ed.line, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
		k = end
	}
	return out.Bytes()
}

// splitLines splits after each newline, keeping it.
func splitLines(data []byte) []string {
	var lines []string
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n') + 1
		if i == 0 {
			i = len(data)
		}
		lines = append(lines, string(data[:i]))
		data = data[i:]
	}
	return lines
}
