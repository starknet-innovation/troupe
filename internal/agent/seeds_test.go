package agent

import "testing"

// Each kind covers only its own paths: files inside a skill folder, files directly in scripts,
// and SOUL.md.
func TestItemRootsPerKind(t *testing.T) {
	for _, c := range []struct {
		kind      Kind
		rel, want string
	}{
		{Skills, "skills/review/SKILL.md", "skills/review"},
		{Skills, "skills/review/references/style.md", "skills/review"},
		{Skills, "skills/README.md", ""},
		{Skills, "scripts/watch.py", ""},
		{Scripts, "scripts/watch.py", "scripts/watch.py"},
		{Scripts, "scripts/lib/helper.py", ""},
		{Soul, "SOUL.md", "SOUL.md"},
		{Soul, "skills/SOUL.md", ""},
		{Skills, "config.yaml", ""},
	} {
		if got := itemRoot(c.kind, c.rel); got != c.want {
			t.Errorf("itemRoot(%s, %q) = %q, want %q", c.kind, c.rel, got, c.want)
		}
	}
}
