// Package registry discovers the Hermes agents in a kits root and composes their seeds.
//
// An agent is a folder whose sbxenv.yaml is named after the folder and composes exactly two
// kits: a base kit first (a relative ../ path, such as ../../kits/coder, to a Docker Sandbox
// v3 workload: a folder holding FOLDER.yaml that says kind: workload) and then itself (.), a
// v3 mixin. The
// folder is the only record of who an agent is: his .env, SOUL, avatar and secrets.
//
// Each kit copies its files/ into the image; v3 lets a mixin's file replace the workload's on a
// shared path, but here an overlap is always a mistake, so composition refuses it.
//
// A published agent is the same, but troupe writes its folder (DATA/agents/NAME) and its base
// kit is a published one pinned by version (REGISTRY/TYPE:VERSION; version tags never move).
// troupe keeps that kit's /usr/share/troupe/ files in DATA/kits/TYPE-VERSION, laid out like a
// local kit folder (its seeds
// under files/home/.hermes), so everything that reads a base kit reads it the same way.
package registry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// seedHome is where a kit keeps the files Docker Sandbox copies into the agent's home.
var seedHome = filepath.Join("files", "home", ".hermes")

// Registry reads agents from a local kits root and from troupe's own folder of published agents.
type Registry struct {
	Root string // the local kits root; "" when there is none (an end user's machine)
	Data string // troupe's data folder: published agents in agents/, their kits' files in kits/
	// Boundary are the Slack channels (public, private) this machine's published agents must be
	// unable to reach, from its settings when the command runs; "" for one that is unset.
	Boundary [2]string
}

// pinned is a published kit reference pinned by version, as a published agent's sbxenv names it:
// its last path component is the type, then the MAJOR.MINOR.PATCH tag.
var pinned = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)*/([a-z0-9]([a-z0-9._-]*[a-z0-9])?):((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))$`)

// PublishedFile is the record troupe writes in a published kit's folder.
const PublishedFile = "kit.json"

// Published describes a published kit troupe has read: the pinned reference, the agent type
// (the repository's last component, as sbx names it), the kit version and the Hermes version.
type Published struct {
	Reference string `json:"reference"`
	Type      string `json:"type"`
	Version   string `json:"version"`
	Hermes    string `json:"hermes"`
}

// KitFolder is where troupe keeps a pinned published kit's files.
func KitFolder(data, reference string) string {
	m := pinned.FindStringSubmatch(reference)
	if m == nil {
		return ""
	}
	return filepath.Join(data, "kits", m[5]+"-"+m[7])
}

// ReadPublished reads a published kit folder's record; an error means folder is not one.
func ReadPublished(folder string) (Published, error) {
	var p Published
	data, err := os.ReadFile(filepath.Join(folder, PublishedFile))
	if err != nil {
		return p, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, fmt.Errorf("%s: %w", filepath.Join(folder, PublishedFile), err)
	}
	if KitFolder(filepath.Dir(filepath.Dir(folder)), p.Reference) != folder || p.Type == "" {
		return p, fmt.Errorf("%s: does not describe this folder", filepath.Join(folder, PublishedFile))
	}
	return p, nil
}

// IsPublished reports whether a base kit folder is a published kit troupe read.
func IsPublished(folder string) bool {
	_, err := ReadPublished(folder)
	return err == nil
}

// UnknownAgentError names an agent the kits root does not hold.
type UnknownAgentError struct {
	Agent, Root string
}

func (e UnknownAgentError) Error() string {
	return fmt.Sprintf("%s is not an agent in %s", e.Agent, e.Root)
}

// environment reads name: and the kits: list from a folder's sbxenv.yaml, without a YAML library.
func environment(folder string) (name string, kits []string, err error) {
	lines, err := readLines(filepath.Join(folder, "sbxenv.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "name:") {
			name = unquote(strings.SplitN(line, ":", 2)[1])
			break
		}
	}
	if start := index(lines, "kits:"); start >= 0 {
		for _, line := range lines[start+1:] {
			if !strings.HasPrefix(line, "  - ") {
				break
			}
			kits = append(kits, unquote(strings.TrimSpace(line)[2:]))
		}
	}
	return name, kits, nil
}

// composesBase reports whether kits are a base kit (a relative ../ path, such as
// ../../kits/coder, to a v3 workload) followed by the folder itself, as an agent's or a Lab's are.
func composesBase(folder string, kits []string) bool {
	return len(kits) == 2 && kits[1] == "." && strings.HasPrefix(kits[0], "../") && isBase(resolve(folder, kits[0]))
}

// composesPublished reports whether kits are a pinned published kit troupe has read, followed by
// the folder itself, as a published agent's are.
func (r Registry) composesPublished(kits []string) bool {
	if len(kits) != 2 || kits[1] != "." || r.Data == "" {
		return false
	}
	// The folder is named TYPE-VERSION; its record says which registry's kit it holds.
	p, err := ReadPublished(KitFolder(r.Data, kits[0]))
	return err == nil && p.Reference == kits[0]
}

// isBase reports whether folder is a v3 workload kit: sbx names a local workload after its
// folder, so its descriptor is FOLDER.yaml, and it says kind: workload.
func isBase(folder string) bool {
	lines, err := readLines(Descriptor(folder))
	if err != nil {
		return false
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "kind: workload" {
			return true
		}
	}
	return false
}

// Descriptor is a v3 kit folder's descriptor, named after the folder.
func Descriptor(folder string) string {
	return filepath.Join(folder, filepath.Base(folder)+".yaml")
}

// Names lists every agent, sorted.
func (r Registry) Names() ([]string, error) {
	folders, err := r.agents()
	if err != nil {
		return nil, err
	}
	found := make([]string, 0, len(folders))
	for name := range folders {
		found = append(found, name)
	}
	sort.Strings(found)
	return found, nil
}

// agents finds every agent folder, up to three levels below the root (agents/ada, or one level
// deeper), mapped by name. An agent's sbxenv.yaml is named after its folder and composes a
// base kit (../../kits/coder) followed by itself.
func (r Registry) agents() (map[string]string, error) {
	found := map[string]string{}
	var walk func(folder string, depth int) error
	walk = func(folder string, depth int) error {
		entries, err := os.ReadDir(folder)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			child := filepath.Join(folder, entry.Name())
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if info, err := os.Stat(child); err != nil || !info.IsDir() {
				continue
			}
			name, kits, err := environment(child)
			if err != nil {
				return err
			}
			// NAME-lab is always a Lab, never an agent, even in a folder of the same name.
			if name == entry.Name() && composesBase(child, kits) && !strings.HasSuffix(name, "-lab") {
				if other, seen := found[name]; seen {
					return fmt.Errorf("two agents named %s: %s and %s", name, other, child)
				}
				found[name] = child
				continue
			}
			if depth < 3 {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if r.Root != "" {
		if err := walk(r.Root, 1); err != nil {
			return nil, err
		}
	}
	// Published agents are the folders troupe wrote, one level below DATA/agents.
	if r.Data == "" {
		return found, nil
	}
	entries, err := os.ReadDir(filepath.Join(r.Data, "agents"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		child := filepath.Join(r.Data, "agents", entry.Name())
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		name, kits, err := environment(child)
		if err != nil {
			return nil, err
		}
		if name != entry.Name() || !r.composesPublished(kits) {
			continue
		}
		if other, seen := found[name]; seen {
			return nil, fmt.Errorf("two agents named %s: %s and %s", name, other, child)
		}
		found[name] = child
	}
	return found, nil
}

// Folder returns the agent's own folder: in the kits root, or troupe's for a published agent.
func (r Registry) Folder(agent string) (string, error) { return r.kit(agent) }

func (r Registry) kit(agent string) (string, error) {
	folders, err := r.agents()
	if err != nil {
		return "", err
	}
	if folder, ok := folders[agent]; ok {
		return folder, nil
	}
	where := r.Root
	if where == "" {
		where = filepath.Join(r.Data, "agents")
	}
	return "", UnknownAgentError{Agent: agent, Root: where}
}

// Kits returns the agent's kit folders, base first.
func (r Registry) Kits(agent string) ([]string, error) {
	folder, err := r.kit(agent)
	if err != nil {
		return nil, err
	}
	_, entries, err := environment(folder)
	if err != nil {
		return nil, err
	}
	kits := make([]string, len(entries))
	for i, entry := range entries {
		if pinned.MatchString(entry) {
			kits[i] = KitFolder(r.Data, entry)
			continue
		}
		kits[i] = resolve(folder, entry)
	}
	return kits, nil
}

// Base returns the agent's base kit folder.
func (r Registry) Base(agent string) (string, error) {
	kits, err := r.Kits(agent)
	if err != nil {
		return "", err
	}
	return kits[0], nil
}

// Env returns the KEY=VALUE settings of the agent's seed .env (identity and Slack settings, no secrets).
func (r Registry) Env(agent string) (map[string]string, error) {
	folder, err := r.kit(agent)
	if err != nil {
		return nil, err
	}
	lines, err := readLines(filepath.Join(folder, seedHome, ".env"))
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range lines {
		key, value, found := strings.Cut(line, "=")
		if found && !strings.HasPrefix(strings.TrimLeft(key, " \t"), "#") {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values, nil
}

// Secrets lists the native secrets the agent's sbxenv.yaml declares.
func (r Registry) Secrets(agent string) ([]string, error) {
	folder, err := r.kit(agent)
	if err != nil {
		return nil, err
	}
	return SecretsIn(folder)
}

// SecretsIn lists the service names under secrets: in a folder's sbxenv.yaml (an agent's kit or a Lab).
func SecretsIn(folder string) ([]string, error) {
	lines, err := readLines(filepath.Join(folder, "sbxenv.yaml"))
	if err != nil {
		return nil, err
	}
	native := []string{}
	if start := index(lines, "secrets:"); start >= 0 {
		for _, line := range lines[start+1:] {
			if line != "" && !strings.HasPrefix(line, " ") {
				break
			}
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimRight(line, " \t"), ":") {
				trimmed := strings.TrimSpace(line)
				native = append(native, trimmed[:len(trimmed)-1])
			}
		}
	}
	return native, nil
}

// Seed is one file of an agent's composed home: its path under ~/.hermes and its source.
type Seed struct {
	Path, Source string
}

// Compose lists every seed file, sorted by path, refusing a path two kits provide.
func (r Registry) Compose(agent string) ([]Seed, error) {
	kits, err := r.Kits(agent)
	if err != nil {
		return nil, err
	}
	return ComposeKits(kits)
}

// ComposeKits lists every seed file these kits provide, base first, as Compose does.
func ComposeKits(kits []string) ([]Seed, error) {
	sources := map[string]string{}
	for _, kit := range kits {
		root := filepath.Join(kit, seedHome)
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		// WalkDir does not enter a linked folder, so start from the real one.
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			// A link to a folder counts as a folder, as it does for the agent's VM.
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if _, taken := sources[rel]; taken {
				return fmt.Errorf("seed path provided by two kits: %s", rel)
			}
			sources[rel] = path
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	seeds := make([]Seed, 0, len(sources))
	for rel, source := range sources {
		seeds = append(seeds, Seed{Path: rel, Source: source})
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].Path < seeds[j].Path })
	return seeds, nil
}

// Materialize writes the agent's composed seed home into dir, keeping each file's mode and time.
func (r Registry) Materialize(agent, dir string) error {
	seeds, err := r.Compose(agent)
	if err != nil {
		return err
	}
	for _, seed := range seeds {
		if err := copyFile(seed.Source, filepath.Join(dir, filepath.FromSlash(seed.Path))); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
		return err
	}
	// WriteFile keeps an existing file's mode; set it explicitly, as a copy would.
	if err := os.Chmod(target, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(target, info.ModTime(), info.ModTime())
}

// resolve is folder/entry with symlinks resolved first, so ../BASE is taken from where an
// agent folder really is, as Python's Path.resolve does.
func resolve(folder, entry string) string {
	if real, err := filepath.EvalSymlinks(folder); err == nil {
		folder = real
	}
	path := filepath.Join(folder, entry)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	return lines, scanner.Err()
}

func index(lines []string, want string) int {
	for i, line := range lines {
		if line == want {
			return i
		}
	}
	return -1
}

func unquote(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"`)
}
