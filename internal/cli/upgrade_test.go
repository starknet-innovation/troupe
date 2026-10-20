package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// lifecycleAgentForTest is an agent that only prints, into a buffer.
func lifecycleAgentForTest(t *testing.T) (*agent.Agent, *bytes.Buffer) {
	var out bytes.Buffer
	return &agent.Agent{Name: "jane", Printer: &progress.Printer{Agent: "Jane", Location: time.UTC, Out: &out, Err: &out, Now: time.Now}}, &out
}

func TestUpgradeStepFollowsTheVersionRule(t *testing.T) {
	for _, c := range []struct {
		latest, on string
		same       bool
		refused    string
	}{
		{"2.0.2", "2.0.2", true, ""},
		{"2.1.0", "2.0.2", false, ""},
		{"2.0.10", "2.0.9", false, ""},
		{"2.0.1", "2.0.2", false, "older than the agent's 2.0.2"},
		{"3.0.0", "2.0.2", false, "coder 3.0.0 is a new MAJOR version; recreate the agent instead (troupe delete jane, then troupe create coder)"},
	} {
		same, err := upgradeStep("coder", c.latest, c.on, "jane")
		if c.refused != "" {
			if err == nil || !strings.Contains(err.Error(), c.refused) {
				t.Errorf("%s from %s: %v", c.latest, c.on, err)
			}
			continue
		}
		if err != nil || same != c.same {
			t.Errorf("%s from %s: same %v, %v", c.latest, c.on, same, err)
		}
	}
}

func TestAnUpgradeKeepsTheSlackApp(t *testing.T) {
	old, next := t.TempDir(), t.TempDir()
	if err := sameSlackApp(old, next); err != nil {
		t.Fatalf("no Slack on either side: %v", err)
	}
	os.WriteFile(filepath.Join(old, "slack-app-manifest.json"), []byte(`{"a":1}`), 0o600)
	os.WriteFile(filepath.Join(next, "slack-app-manifest.json"), []byte(`{"a":1}`), 0o600)
	if err := sameSlackApp(old, next); err != nil {
		t.Fatalf("the same manifest: %v", err)
	}
	os.WriteFile(filepath.Join(next, "slack-app-manifest.json"), []byte(`{"a":2}`), 0o600)
	if err := sameSlackApp(old, next); err == nil || !strings.Contains(err.Error(), "Slack app manifest differs") {
		t.Fatalf("a changed manifest: %v", err)
	}
	os.Remove(filepath.Join(next, "slack-app-manifest.json"))
	if err := sameSlackApp(old, next); err == nil {
		t.Fatal("a manifest gone from the new version passed")
	}
}

// A key the new version needs that cannot be given in place (a native one, a Slack token) is
// refused before anything is asked or stored.
func TestAnUpgradeRefusesAKeyItCannotAdd(t *testing.T) {
	h, _ := home(t)
	folder, kit := filepath.Join(h, "agent"), filepath.Join(h, "kit")
	os.MkdirAll(folder, 0o700)
	os.MkdirAll(kit, 0o700)
	os.WriteFile(filepath.Join(folder, "sbxenv.yaml"), []byte("name: jane\nkits:\n  - \"x\"\n  - \".\"\n\nsecrets:\n  venice:\n    command: c\n    refresh: on-demand\n"), 0o600)
	for services, want := range map[string]string{
		`{"services":[{"name":"venice","prompt":"V","help":"The provider key, from its site.","required":true,"delivery":"native","domains":["api.venice.ai"]},{"name":"search","prompt":"S","help":"A search key, from its site.","required":true,"delivery":"native","domains":["api.search.example"]}]}`: "requires a search key",
		`{"services":[{"name":"slack-bot","prompt":"B","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-bot"}]}`:                                                                                                     "requires a slack-bot key",
	} {
		os.WriteFile(filepath.Join(kit, "services.json"), []byte(services), 0o600)
		a, _ := lifecycleAgentForTest(t)
		err := upgradeKeys(env{}, a, folder, kit, "jane", source{Services: []string{}}, sbx.Runner{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err %v", err)
		}
	}
}

// A move left partway is finished first: upgrade refuses before asking for sudo or changing
// the pin.
func TestUpgradeRefusesAnOpenTransactionFirst(t *testing.T) {
	h, bin := home(t)
	folder := publishedAgent(t, h)
	os.WriteFile(filepath.Join(folder, "source.json"), []byte(`{"reference":"ghcr.io/example/hermes:1.2.0","type":"hermes","version":"1.2.0","services":[]}`), 0o600)
	before, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml"))
	os.MkdirAll(filepath.Join(h, ".local/state/sandbox-replacements/jane/current"), 0o700)
	if code, _, errs := run("upgrade", "jane"); code != 1 || !strings.Contains(errs, "stopped partway; finish it first (troupe resize jane resumes a move)") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(bin, "sudo-calls")); err == nil {
		t.Fatal("asked for sudo before refusing")
	}
	if after, _ := os.ReadFile(filepath.Join(folder, "sbxenv.yaml")); string(after) != string(before) {
		t.Fatal("a refused upgrade changed the pin")
	}
	if code, out, _ := run("upgrade", "--help"); code != 0 || !strings.Contains(out, "troupe upgrade AGENT [flags]") || !strings.Contains(out, "  --force ") {
		t.Fatalf("help: exit %d: %s", code, out)
	}
}

// On the latest version already, an upgrade leaves the agent's skills alone; only an upgrade
// that stopped before its seed step (its pending mark left) goes on to the seeds.
func TestAnUpgradeOnTheLatestVersionLeavesTheAgent(t *testing.T) {
	home(t)
	pending := agent.LogFile("jane", "upgrade-seeds-pending")
	a, out := lifecycleAgentForTest(t)
	e := env{ctx: context.Background(), out: out, err: out}
	if code := alreadyLatest(e, a, "jane", pending); code != 0 || out.Len() != 0 {
		t.Fatalf("no pending upgrade: exit %d, %s", code, out)
	}
	// Negative control: with the mark, the seed step runs (and fails here: jane is unknown),
	// and the mark stays for the next run.
	if err := writePending(pending); err != nil {
		t.Fatal(err)
	}
	if code := alreadyLatest(e, a, "jane", pending); code == 0 {
		t.Fatal("a pending upgrade did not go on to its seeds")
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("a failed seed step removed the pending mark")
	}
}
