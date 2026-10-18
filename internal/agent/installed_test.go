package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// Bytes reads only regular files inside the recreate's seeds/ copy: no path out of it, no link.
func TestInstalledBytesStayInsideTheSeedsCopy(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "reseed", "2026-01-01T00:00:00Z")
	os.MkdirAll(filepath.Join(run, "seeds", "skills", "x"), 0o700)
	secret := filepath.Join(dir, "secret")
	os.WriteFile(secret, []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(run, "seeds", "skills", "x", "SKILL.md"), []byte("seed"), 0o600)
	os.Symlink(secret, filepath.Join(run, "seeds", "skills", "x", "linked.md"))
	in := &Installed{Reseed: Receipt{Name: "2026-01-01T00:00:00Z", Path: run}}
	if from, data := in.Bytes("skills/x/SKILL.md", digest([]byte("seed"))); from != in.Reseed.Name || string(data) != "seed" {
		t.Fatalf("seed: %q from %q", data, from)
	}
	for _, rel := range []string{"../../secret", "skills/x/linked.md", "/" + secret} {
		if _, data := in.Bytes(rel, digest([]byte("secret"))); data != nil {
			t.Errorf("%s read %q", rel, data)
		}
	}
	if _, data := in.Bytes("skills/x/SKILL.md", digest([]byte("other"))); data != nil {
		t.Error("bytes of another hash were returned")
	}
}
