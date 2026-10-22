package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/doctor"
	"github.com/starknet-innovation/troupe/internal/secret"
)

func TestCreateArguments(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no --name":            {[]string{"create", "coder"}, "give the new agent a name with --name"},
		"sizes without --name": {[]string{"create", "coder", "--cpu", "2"}, "give the new agent a name with --name"},
		"no type":              {[]string{"create", "--name", "jane"}, "name one agent type"},
		"two types":            {[]string{"create", "coder", "hermes", "--name", "jane"}, "name one agent type"},
		"a name without an ID": {[]string{"create", "hermes", "--name", "!!!"}, "at least one Latin letter or digit"},
		"a name too long":      {[]string{"create", "hermes", "--name", strings.Repeat("a", 36)}, "longer than 35 characters"},
		"a malformed type":     {[]string{"create", "../hermes", "--name", "jane"}, "TYPE must be coder or hermes"},
		"an unknown type":      {[]string{"create", "other", "--name", "jane"}, "TYPE must be coder or hermes"},
		"a negative size":      {[]string{"create", "hermes", "--name", "jane", "--memory", "-1"}, "positive whole numbers"},
		"a Lab's name":         {[]string{"create", "hermes", "--name", "hermes-lab"}, "a Lab's"},
		"a reserved name":      {[]string{"create", "hermes", "--name", "collector"}, "troupe reserves"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, errs := run(c.args...); code != 2 || !strings.Contains(errs, c.want) {
				t.Fatalf("exit %d: %s", code, errs)
			}
		})
	}
}

// home isolates every folder create reads and gives it a docker that records its calls and fails.
func home(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	t.Setenv("TROUPE_KITS", "")
	t.Chdir(home)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"docker $*\" >> \""+bin+"/calls\"\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho \"sudo $*\" >> \""+bin+"/sudo-calls\"\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	// The machine is ready, unless a test says otherwise.
	saved := machineCheck
	machineCheck = func() []doctor.Line { return nil }
	t.Cleanup(func() { machineCheck = saved })
	// And it has a TPM, unless a test says otherwise.
	savedTier := keyTier
	keyTier = func() secret.Tier { return secret.TPM }
	t.Cleanup(func() { keyTier = savedTier })
	return home, bin
}

// Every cli test reads an empty data folder, never the machine's published agents.
func TestMain(m *testing.M) {
	data, err := os.MkdirTemp("", "troupe-cli-data-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_DATA_HOME", data)
	code := m.Run()
	os.RemoveAll(data)
	os.Exit(code)
}

// sudo is the first thing create asks for, before any docker call or other prompt.
func TestCreateAsksForSudoFirst(t *testing.T) {
	_, bin := home(t)
	os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	code, _, errs := run("create", "hermes", "--name", "jane")
	if code != 1 || !strings.Contains(errs, "sudo_required") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(bin, "calls")); err == nil {
		t.Fatal("docker ran before sudo")
	}
}

func TestAResumedCreateKeepsItsSizes(t *testing.T) {
	h, bin := home(t)
	folder := filepath.Join(h, ".local/share/troupe/agents/jane")
	os.MkdirAll(folder, 0o700)
	os.WriteFile(filepath.Join(folder, "source.json"), []byte(`{"reference":"ghcr.io/example/hermes:1.2.0","type":"hermes","version":"1.2.0"}`), 0o600)
	code, _, errs := run("create", "hermes", "--name", "jane", "--memory", "12")
	if code != 1 || !strings.Contains(errs, "keeps the sizes, SOUL and picture it began with") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if code, _, errs := run("create", "coder", "--name", "jane"); code != 1 || !strings.Contains(errs, "is a hermes agent, not coder") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(bin, "calls")); err == nil {
		t.Fatal("docker ran")
	}
}

// A name any agent has, had, or holds keys for on this machine is refused before anything is
// pulled or asked.
func TestCreateRefusesANameInUse(t *testing.T) {
	for name, plant := range map[string]string{
		"receipts remain": ".local/state/sandbox-replacements/jane/reseed/completed/x",
		"keys remain":     ".local/state/credentials/jane/venice.cred",
	} {
		t.Run(name, func(t *testing.T) {
			home, bin := home(t)
			os.MkdirAll(filepath.Dir(filepath.Join(home, plant)), 0o700)
			os.WriteFile(filepath.Join(home, plant), []byte("x"), 0o600)
			code, _, errs := run("create", "hermes", "--name", "jane")
			if code != 1 || !strings.Contains(errs, "existed on this machine before") {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if _, err := os.Stat(filepath.Join(bin, "calls")); err == nil {
				t.Fatal("docker ran")
			}
			if _, err := os.Stat(filepath.Join(home, plant)); err != nil {
				t.Fatal("a refused create removed what it found")
			}
		})
	}
}

func TestDeleteRefusesWhatTroupeDoesNotOwn(t *testing.T) {
	h, bin := home(t)
	// Another tool keeps its keys where troupe keeps an agent's; they are not troupe's record.
	os.MkdirAll(filepath.Join(h, ".local/state/credentials/keys-only"), 0o700)
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"a Lab":                    {[]string{"delete", "hermes-lab"}, 2, "is a Lab"},
		"a malformed name":         {[]string{"delete", "../x"}, 2, "malformed agent name"},
		"no record of it":          {[]string{"delete", "other-tool"}, 1, "troupe has no agent named other-tool"},
		"only another tool's keys": {[]string{"delete", "keys-only"}, 1, "troupe has no agent named keys-only"},
		"a reserved name":          {[]string{"delete", "collector"}, 1, "troupe has no agent named collector"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, errs := run(c.args...); code != c.code || !strings.Contains(errs, c.want) {
				t.Fatalf("exit %d: %s", code, errs)
			}
		})
	}
	_ = bin
	if _, err := os.Stat(filepath.Join(h, ".local/state/credentials/keys-only")); err != nil {
		t.Fatal("removed another tool's keys")
	}
}

// The confirmation is the name people read, quoted: a published agent's as created, else the
// ID capitalized. Only that exact name goes on to the deletion.
func TestDeleteAsksForTheNameAsShown(t *testing.T) {
	h, bin := home(t)
	// The accepted name goes on to the deletion, which must reach only fakes here: sbx and
	// systemctl record their call and fail, so nothing on this machine is touched.
	for _, tool := range []string{"sbx", "systemctl"} {
		os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho \""+tool+" $*\" >> \""+bin+"/calls\"\nexit 1\n"), 0o755)
	}
	folder := filepath.Join(h, ".local/share/troupe/agents/mr-doyle")
	os.MkdirAll(folder, 0o700)
	os.WriteFile(filepath.Join(folder, "source.json"), []byte(`{"reference":"ghcr.io/x/coder:2.0.1","type":"coder","display_name":"Mr. Doyle"}`), 0o600)
	defer func(saved func(context.Context, string) (string, error)) { readLine = saved }(readLine)
	for _, c := range []struct{ typed, refused string }{{"mr-doyle", "troupe: Mr. Doyle was not deleted"}, {"Mr. Doyle", ""}} {
		var asked string
		readLine = func(_ context.Context, prompt string) (string, error) { asked = prompt; return c.typed, nil }
		_, out, errs := run("delete", "mr-doyle")
		if want := `Deleting Mr. Doyle removes its VM, memory, keys and records for good. Type "Mr. Doyle" to delete it: `; asked != want {
			t.Fatalf("asked %q", asked)
		}
		if c.refused != "" && !strings.Contains(errs, c.refused) {
			t.Fatalf("typed %q: %s", c.typed, errs)
		}
		// The accepted name reached the deletion, which stopped at the faked machine's checks.
		if c.refused == "" && (strings.Contains(errs, "was not deleted") || !strings.Contains(errs+out, "sandboxd_unhealthy")) {
			t.Fatalf("the name as shown did not start the deletion: %s%s", out, errs)
		}
	}
}

func TestResetAndChatNeedAKnownAgent(t *testing.T) {
	home(t)
	for _, args := range [][]string{{"reset", "nobody"}, {"chat", "nobody"}} {
		if code, _, errs := run(args...); code == 0 || !strings.Contains(errs, "nobody") {
			t.Fatalf("%v: exit %d: %s", args, code, errs)
		}
	}
}

// publishedAgent writes troupe's data folder for a published agent jane (hermes, 4 CPUs, 6 GiB,
// 50 GiB root) on a kept kit.
func publishedAgent(t *testing.T, h string) string {
	t.Helper()
	ref := "ghcr.io/example/hermes:1.2.0"
	data := filepath.Join(h, ".local/share/troupe")
	kit := filepath.Join(data, "kits", "hermes-1.2.0")
	folder := filepath.Join(data, "agents/jane")
	for name, text := range map[string]string{
		filepath.Join(kit, "kit.json"):                       `{"reference":"` + ref + `","type":"hermes","version":"1.2.0","hermes":"0.22.2"}`,
		filepath.Join(kit, "files/home/.hermes/config.yaml"): "x\n",
		filepath.Join(folder, "sbxenv.yaml"):                 "schemaVersion: \"1\"\nname: jane\nagent: hermes\n\nkits:\n  - \"" + ref + "\"\n  - \".\"\n\nsandboxOptions:\n  skills: \"off\"\n  cpus: 4\n  memory: 6g\n",
		filepath.Join(folder, "jane.yaml"):                   "kind: mixin\n",
		filepath.Join(folder, "jane.dockerfile"):             "FROM scratch\n",
		filepath.Join(folder, ".dockerignore"):               "sbxenv.yaml\n",
		filepath.Join(folder, "storage.env"):                 "DOCKER_SANDBOXES_ROOT_SIZE=50g\nDOCKER_SANDBOXES_DOCKER_SIZE=10g\n",
		filepath.Join(folder, "files/home/.hermes/.env"):     "# x\n",
	} {
		os.MkdirAll(filepath.Dir(name), 0o700)
		os.WriteFile(name, []byte(text), 0o600)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(h, ".local/share"))
	return folder
}

func TestResizeRefusals(t *testing.T) {
	h, _ := home(t)
	folder := publishedAgent(t, h)
	before, _ := os.ReadFile(filepath.Join(folder, "storage.env"))
	if code, _, errs := run("resize", "jane", "--storage", "40"); code != 1 || !strings.Contains(errs, "the root disk can only grow: jane has 50 GiB") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, open := range []string{"current", "reseed/current"} {
		dir := filepath.Join(h, ".local/state/sandbox-replacements/jane", open)
		os.MkdirAll(dir, 0o700)
		if code, _, errs := run("resize", "jane", "--memory", "8"); code != 1 || !strings.Contains(errs, "stopped partway; finish it first") {
			t.Fatalf("%s: exit %d: %s", open, code, errs)
		}
		os.RemoveAll(dir)
	}
	if code, _, errs := run("resize", "jane", "--memory", "9223372036854775807"); code != 2 || !strings.Contains(errs, "at most") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if after, _ := os.ReadFile(filepath.Join(folder, "storage.env")); string(after) != string(before) {
		t.Fatal("a refused resize changed the sizes")
	}
}

// reset asks before it erases anything; --yes answers for a script, and --check never asks.
// --force alone still asks: it only confirms the machine's budget.
func TestResetAsksUnlessForcedOrChecking(t *testing.T) {
	h, _ := home(t)
	publishedAgent(t, h)
	t.Setenv("TROUPE_SANDBOXD_DIR", t.TempDir())
	asked := 0
	defer func(saved func(context.Context, string) (string, error)) { readLine = saved }(readLine)
	readLine = func(context.Context, string) (string, error) { asked++; return "n", nil }
	if code, _, errs := run("reset", "jane"); code != 1 || asked != 1 || !strings.Contains(errs, "jane was not reset") {
		t.Fatalf("exit %d, asked %d: %s", code, asked, errs)
	}
	for _, flag := range []string{"--yes", "--check"} {
		code, _, errs := run("reset", flag, "jane")
		if code == 0 || asked != 1 || strings.Contains(errs, "was not reset") || !strings.Contains(errs, "sandboxd") {
			t.Fatalf("%s: exit %d, asked %d: %s", flag, code, asked, errs)
		}
	}
	if code, _, errs := run("reset", "--force", "jane"); code != 1 || asked != 2 || !strings.Contains(errs, "jane was not reset") {
		t.Fatalf("--force: exit %d, asked %d: %s", code, asked, errs)
	}
}

// create checks the machine first: any problem the doctor finds stops it before sudo or a prompt.
func TestCreateRefusesAMachineThatIsNotReady(t *testing.T) {
	for _, level := range []doctor.Level{doctor.Fail, doctor.Warn} {
		_, bin := home(t)
		machineCheck = func() []doctor.Line {
			return []doctor.Line{{Name: "cpu", Level: doctor.OK, Message: "x86_64"}, {Name: "kvm", Level: level, Message: "/dev/kvm is not usable"}}
		}
		code, out, errs := run("create", "hermes", "--name", "jane")
		if code != 1 || !strings.Contains(out+errs, "/dev/kvm is not usable") || !strings.Contains(errs, "not ready for agents") {
			t.Fatalf("level %d: exit %d: %s%s", level, code, out, errs)
		}
		if _, err := os.Stat(filepath.Join(bin, "sudo-calls")); err == nil {
			t.Fatalf("level %d: asked for sudo", level)
		}
	}
}

// doctor exits 1 only on a failure; a note never stops create.
func TestDoctorExitsOnFailuresOnlyAndNotesNeverBlockCreate(t *testing.T) {
	_, bin := home(t)
	for level, want := range map[doctor.Level]int{doctor.Fail: 1, doctor.Warn: 0, doctor.Note: 0, doctor.OK: 0} {
		machineCheck = func() []doctor.Line { return []doctor.Line{{Name: "x", Level: level, Message: "m"}} }
		if code, _, _ := run("doctor"); code != want {
			t.Errorf("level %d: exit %d, want %d", level, code, want)
		}
	}
	machineCheck = func() []doctor.Line {
		return []doctor.Line{{Name: "systemd", Level: doctor.Note, Message: "systemd 255 is older than 259"}}
	}
	run("create", "hermes", "--name", "jane")
	if _, err := os.Stat(filepath.Join(bin, "sudo-calls")); err != nil {
		t.Fatal("a note stopped create before sudo")
	}
}

// Keys kept without a TPM need the user's yes, asked before sudo or any setup: a no, or no
// terminal to ask on, stops create with nothing done. With a TPM, nothing is asked.
func TestCreateAsksBeforeKeepingKeysWithoutATPM(t *testing.T) {
	defer func(saved func(context.Context, string) (string, error)) { readLine = saved }(readLine)
	for name, c := range map[string]struct {
		tier    secret.Tier
		answer  string
		err     error
		asked   bool
		goesOn  bool
		message string
	}{
		"a TPM":               {secret.TPM, "", nil, false, true, "Sealed by this machine's TPM"},
		"host key, declined":  {secret.Host, "n", nil, true, false, "host key only"},
		"host key, no answer": {secret.Host, "", nil, true, false, "host key only"},
		"plain file, no tty":  {secret.File, "", errors.New("private_terminal_required"), true, false, "plain files"},
		"host key, yes":       {secret.Host, "y", nil, true, true, "host key only"},
		"plain file, yes":     {secret.File, "yes", nil, true, true, "plain files"},
	} {
		t.Run(name, func(t *testing.T) {
			h, bin := home(t)
			keyTier = func() secret.Tier { return c.tier }
			asked := false
			readLine = func(_ context.Context, prompt string) (string, error) {
				asked = prompt == "Keep this agent's keys this way? [y/N] "
				return c.answer, c.err
			}
			code, out, errs := run("create", "hermes", "--name", "jane")
			if asked != c.asked || !strings.Contains(out, c.message) {
				t.Fatalf("asked %v: %s%s", asked, out, errs)
			}
			_, sudoErr := os.Stat(filepath.Join(bin, "sudo-calls"))
			if c.goesOn != (sudoErr == nil) {
				t.Fatalf("went on to sudo: %v: %s", sudoErr == nil, errs)
			}
			if !c.goesOn {
				if code != 1 || !strings.Contains(errs, "need your yes") {
					t.Fatalf("exit %d: %s", code, errs)
				}
				if _, err := os.Stat(filepath.Join(h, ".local/state/credentials/jane")); err == nil {
					t.Fatal("a declined create claimed the keys folder")
				}
			}
		})
	}
}

// A resumed creation keeps the tier its keys folder records: it says that one, and asks only
// when it is below the TPM, whatever this machine offers now.
func TestAResumedCreateKeepsItsKeysTier(t *testing.T) {
	defer func(saved func(context.Context, string) (string, error)) { readLine = saved }(readLine)
	for name, c := range map[string]struct {
		recorded, machine secret.Tier
		asked             bool
		message           string
	}{
		"plain files, now a TPM": {secret.File, secret.TPM, true, "plain files"},
		"TPM, now host key only": {secret.TPM, secret.Host, false, "Sealed by this machine's TPM"},
		"no record, now no TPM":  {"", secret.File, false, "Sealed by this machine's TPM"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := home(t)
			keyTier = func() secret.Tier { return c.machine }
			keys := filepath.Join(h, ".local/state/credentials/jane")
			os.MkdirAll(keys, 0o700)
			if c.recorded != "" {
				secret.SetTier(filepath.Dir(keys), "jane", c.recorded)
			}
			asked := false
			readLine = func(context.Context, string) (string, error) { asked = true; return "n", nil }
			_, out, errs := run("create", "hermes", "--name", "jane")
			if asked != c.asked || !strings.Contains(out, c.message) {
				t.Fatalf("asked %v: %s%s", asked, out, errs)
			}
		})
	}
}
