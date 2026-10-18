package cli

import (
	"runtime/debug"
	"testing"
)

// A release shows its tag's version; a checkout build its short commit, never Go's
// pseudo-version.
func TestVersionIsTheReleaseOrTheCommit(t *testing.T) {
	commit := []debug.BuildSetting{{Key: "vcs.revision", Value: "6ddc673619e1c3a1d2b4"}}
	modified := append(commit, debug.BuildSetting{Key: "vcs.modified", Value: "true"})
	for _, c := range []struct {
		module   string
		settings []debug.BuildSetting
		want     string
	}{
		{"v1.0.0", commit, "1.0.0"},
		{"v1.2.3", nil, "1.2.3"},
		{"v0.0.0-20261014202553-6ddc673619e1", commit, "6ddc673"},
		{"v0.0.0-20261014202553-6ddc673619e1+dirty", modified, "6ddc673 modified"},
		{"v1.0.1-0.20261014202553-6ddc673619e1", commit, "6ddc673"},
		{"v1.0.0-rc.1", commit, "6ddc673"},
		{"(devel)", commit, "6ddc673"},
		{"", nil, "unknown"},
	} {
		if got := versionOf(c.module, c.settings); got != c.want {
			t.Errorf("%q: %q, want %q", c.module, got, c.want)
		}
	}
}
