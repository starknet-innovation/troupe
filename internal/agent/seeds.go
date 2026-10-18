package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/starknet-innovation/troupe/internal/registry"
)

const (
	Skills  Kind = "skills"
	Scripts Kind = "scripts"
	Soul    Kind = "soul"
)

// item is one unit an update rewrites, adds or deletes: a skill folder, a script or SOUL.md.
type item struct {
	name string            // skill name, script file name, or SOUL.md
	root string            // path under ~/.hermes: skills/NAME, scripts/NAME or SOUL.md
	seed map[string][]byte // seed files by path under ~/.hermes; empty when removed from the seeds
}

// Seeds reads the kits an agent composes (base first), composed exactly as a recreate
// installs them: every file of the kits' files/home/.hermes, never from version control.
type Seeds struct {
	Kits []string
}

// composed is every seed file's bytes by path under ~/.hermes.
func (s Seeds) composed() (map[string][]byte, error) {
	seeds, err := registry.ComposeKits(s.Kits)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, seed := range seeds {
		data, err := os.ReadFile(seed.Source)
		if err != nil {
			return nil, err
		}
		files[seed.Path] = data
	}
	return files, nil
}

// itemRoot is the item of kind a path under ~/.hermes belongs to (skills/NAME for a file inside
// a skill folder, scripts/NAME for a file directly in scripts, SOUL.md), or "" when kind does
// not cover it.
func itemRoot(kind Kind, rel string) string {
	switch kind {
	case Soul:
		if rel == "SOUL.md" {
			return rel
		}
	case Skills:
		if rest, ok := strings.CutPrefix(rel, "skills/"); ok {
			if name, _, nested := strings.Cut(rest, "/"); nested && name != "" {
				return "skills/" + name
			}
		}
	case Scripts:
		if rest, ok := strings.CutPrefix(rel, "scripts/"); ok && rest != "" && !strings.Contains(rest, "/") {
			return rel
		}
	}
	return ""
}

// Items lists what an update of kind covers: everything the kits hold now, plus what troupe
// installed (installed, by path under ~/.hermes) that the kits no longer hold (those are
// deleted). Anything else in the agent's folders was never seeded, so it is the agent's own and
// is never listed.
func (s Seeds) Items(kind Kind, installed map[string]string) ([]item, error) {
	files, err := s.composed()
	if err != nil {
		return nil, err
	}
	if _, ok := files["SOUL.md"]; kind == Soul && !ok {
		return nil, fmt.Errorf("no kit has a SOUL.md")
	}
	items := map[string]*item{}
	add := func(rel string) *item {
		root := itemRoot(kind, rel)
		if root == "" {
			return nil
		}
		if items[root] == nil {
			name := strings.TrimPrefix(strings.TrimPrefix(root, "skills/"), "scripts/")
			items[root] = &item{name: name, root: root, seed: map[string][]byte{}}
		}
		return items[root]
	}
	for rel, data := range files {
		if it := add(rel); it != nil {
			it.seed[rel] = data
		}
	}
	for rel := range installed {
		add(rel)
	}
	list := make([]item, 0, len(items))
	for _, it := range items {
		list = append(list, *it)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	return list, nil
}

// itemsSHA names the seeds an update of these items installs: one hash over every path and
// its content's hash.
func itemsSHA(items []item) string {
	var lines []string
	for _, it := range items {
		for rel, data := range it.seed {
			lines = append(lines, rel+" "+digest(data))
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
