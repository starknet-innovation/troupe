package sbx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// sourcesSbx is an sbx whose kit.allowedSources lives in a file: get prints it, set replaces it
// and is recorded.
func sourcesSbx(t *testing.T, value string) (Runner, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "sources")
	os.WriteFile(file, []byte(value+"\n"), 0o600)
	script := `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
  "settings get") cat "$d/sources" ;;
  "settings set") printf '%s\n' "$4" > "$d/sources"; echo set >> "$d/calls" ;;
  *) exit 1 ;;
esac
`
	os.WriteFile(filepath.Join(dir, "sbx"), []byte(script), 0o755)
	return Runner{Binary: filepath.Join(dir, "sbx"), Log: filepath.Join(dir, "log"),
		Probe: func(context.Context) error { return nil }}, dir
}

// A fresh sbx allows only docker.io/: troupe adds its own registry, keeping what was there, and
// never touches a list that already admits it.
func TestAllowSourceAddsTroupesRegistryOnce(t *testing.T) {
	runner, dir := sourcesSbx(t, `["docker.io/"]`)
	added, err := runner.AllowKitSource(context.Background(), "ghcr.io/example/agents")
	if err != nil || !added {
		t.Fatalf("added %v err %v", added, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sources")); string(got) != `["docker.io/","ghcr.io/example/agents/"]`+"\n" {
		t.Fatalf("sources %s", got)
	}
	if added, err := runner.AllowKitSource(context.Background(), "ghcr.io/example/agents"); err != nil || added {
		t.Fatalf("second: added %v err %v", added, err)
	}
	for _, covered := range []string{`["docker.io/", "ghcr.io/"]`, `["ghcr.io/example/"]`} {
		runner, dir := sourcesSbx(t, covered)
		if added, err := runner.AllowKitSource(context.Background(), "ghcr.io/example/agents"); err != nil || added {
			t.Fatalf("%s: added %v err %v", covered, added, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
			t.Fatalf("%s: set", covered)
		}
	}
	// Another owner on the same host is not this registry.
	runner, dir = sourcesSbx(t, `["ghcr.io/other/"]`)
	if added, _ := runner.AllowKitSource(context.Background(), "ghcr.io/example/agents"); !added {
		t.Fatal("ghcr.io/other/ counted as troupe's registry")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sources")); string(got) != `["ghcr.io/other/","ghcr.io/example/agents/"]`+"\n" {
		t.Fatalf("sources %s", got)
	}
	// Not a list, or one whose meaning sbx does not state, is never rewritten.
	for _, unread := range []string{`not json`, `null`, `[]`} {
		runner, dir := sourcesSbx(t, unread)
		if _, err := runner.AllowKitSource(context.Background(), "ghcr.io/example/agents"); err == nil {
			t.Fatalf("%s passed", unread)
		}
		if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
			t.Fatalf("%s: set", unread)
		}
	}
}
