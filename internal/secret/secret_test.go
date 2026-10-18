package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/sbx"
)

// The secret every test plants; it must never appear in any output, log or argument list.
const planted = "sk-PLANTED-0123456789"

// fakeHost puts fake sudo, systemd-creds and sbx on PATH. systemd-creds "encrypts" by writing a
// valid-looking TPM ciphertext and keeping the plaintext in a side file the test reads;
// decrypt succeeds only for files carrying that ciphertext. sbx keeps scoped records.
type fakeHost struct {
	t    *testing.T
	dir  string
	root string
	out  bytes.Buffer
}

func ciphertext() string {
	raw := append(append([]byte{}, header...), bytes.Repeat([]byte{7}, 120)...)
	return base64.StdEncoding.EncodeToString(raw)
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	h := &fakeHost{t: t, dir: dir, root: filepath.Join(dir, "credentials")}
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	write := func(name, text string) { os.WriteFile(filepath.Join(bin, name), []byte(text), 0o755) }
	write("sudo", `#!/bin/sh
d=$(dirname "$0")/..
echo "sudo $*" >> "$d/calls"
[ "$1" = "-v" ] && exit 0
shift
exec "$@"
`)
	write("systemd-creds", `#!/bin/sh
d=$(dirname "$0")/..
echo "systemd-creds $*" >> "$d/calls"
for a in "$@"; do case "$a" in encrypt) mode=encrypt ;; decrypt) mode=decrypt ;; esac; done
if [ "$mode" = encrypt ]; then
  cat > "$d/plaintext"
  [ -f "$d/encrypt-bad" ] && { echo "bm90IGEgY2lwaGVydGV4dA=="; exit 0; }
  echo "`+ciphertext()+`"; exit 0
fi
file=$(for a in "$@"; do case "$a" in /*) echo "$a" ;; esac; done | head -1)
[ -f "$d/decrypt-bad" ] && exit 1
grep -q "`+ciphertext()[:20]+`" "$file" || exit 1
cat "$d/plaintext" 2>/dev/null
exit 0
`)
	write("sbx", `#!/bin/sh
d=$(dirname "$0")/..
echo "sbx $*" >> "$d/calls"
# Like sbx 0.48: secret set takes --force (troupe passes it); set-custom has none and rejects it.
case "$1 $2" in "secret set") case " $* " in *" --force "*) ;; *) exit 1 ;; esac ;; "secret set-custom") case " $* " in *" --force "*) exit 1 ;; esac ;; esac
case "$1 $2" in
  "ls --json") cat "$d/inventory.json" ;;
  "secret ls") cat "$d/records.json" ;;
  "secret set") echo '{"secrets":[{"scope":"ada","type":"service","name":"venice"}],"custom_secrets":[{"env":"SLACK_BOT_TOKEN","scope":"ada","placeholder":"sbx-cs-ada-slack-bot"}]}' > "$d/records.json" ;;
  "secret set-custom") echo '{"secrets":[{"scope":"ada","type":"service","name":"venice"}],"custom_secrets":[{"env":"SLACK_BOT_TOKEN","scope":"ada","placeholder":"sbx-cs-ada-slack-bot"}]}' > "$d/records.json" ;;
esac
`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "inventory.json"), []byte(`[{"name":"ada","status":"running"}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "records.json"), []byte(`{"secrets":[],"custom_secrets":[]}`), 0o644)
	return h
}

func (h *fakeHost) store() *Store {
	services, err := Services("ada", []string{"venice"}, h.base())
	if err != nil {
		h.t.Fatal(err)
	}
	return &Store{Agent: "ada", Services: services, Root: h.root,
		Runner:     sbx.Runner{Binary: filepath.Join(h.dir, "bin/sbx"), Log: filepath.Join(h.dir, "native.log"), Probe: func(context.Context) error { return nil }},
		ReadSecret: func(string) ([]byte, error) { return []byte(planted), nil }}
}

func (h *fakeHost) base() string {
	base := filepath.Join(h.dir, "base")
	os.MkdirAll(base, 0o755)
	os.WriteFile(filepath.Join(base, "secrets.json"), []byte(`{"placeholders": [
  {"service": "slack-bot", "env": "SLACK_BOT_TOKEN", "hosts": ["slack.com"], "placeholder": "sbx-cs-{agent}-slack-bot"}]}`), 0o644)
	return base
}

func (h *fakeHost) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.dir, name))
	return string(data)
}

func TestServicesComeFromTheKits(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	var names []string
	for _, svc := range s.Services {
		names = append(names, svc.Name)
	}
	if strings.Join(names, " ") != "venice slack-bot" || s.Services[1].Value != "sbx-cs-ada-slack-bot" {
		t.Fatalf("services %+v", s.Services)
	}
	if _, _, err := s.Set(context.Background(), "github"); err == nil || !strings.Contains(err.Error(), "venice, slack-bot") && !strings.Contains(err.Error(), "slack-bot, venice") {
		t.Fatalf("unknown service: %v", err)
	}
	bad := filepath.Join(h.dir, "bad")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "secrets.json"), []byte(`{"placeholders": [{"service": "x", "env": "X", "hosts": ["x.com"], "placeholder": "fixed"}]}`), 0o644)
	if _, err := Services("ada", nil, bad); err == nil {
		t.Fatal("a placeholder that does not name the agent was accepted")
	}
	if _, err := Services("ada", []string{"../venice"}, h.base()); err == nil {
		t.Fatal("a service name that leaves the agent's folder was accepted")
	}
}

func TestSetStoresWiresAndNeverShowsTheSecret(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	wired, isNew, err := s.Set(context.Background(), "venice")
	if err != nil || !wired || !isNew {
		t.Fatalf("wired %v new %v err %v", wired, isNew, err)
	}
	if h.read("plaintext") != planted {
		t.Fatal("the secret did not reach the encryption")
	}
	file := filepath.Join(h.root, "ada", "venice.cred")
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ciphertext %v %v", info, err)
	}
	for _, dir := range []string{h.root, filepath.Join(h.root, "ada")} {
		if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", dir, info.Mode().Perm())
		}
	}
	if !strings.Contains(h.read("calls"), "sbx secret set venice --sandbox ada --command systemd-creds --user --no-ask-password --name=ada-venice decrypt "+file+" -") {
		t.Fatalf("wiring:\n%s", h.read("calls"))
	}
	for _, place := range []string{h.read("calls"), h.read("native.log"), h.out.String()} {
		if strings.Contains(place, planted) {
			t.Fatal("the secret leaked into an argument list, log or output")
		}
	}
	// A rotation: the record is already there, so the key is not new to the sandbox.
	if _, isNew, err := s.Set(context.Background(), "venice"); err != nil || isNew {
		t.Fatalf("rotation: new %v err %v", isNew, err)
	}
}

func TestPlaceholderServiceIsWiredAsACustomSecret(t *testing.T) {
	h := newFakeHost(t)
	if _, _, err := h.store().Set(context.Background(), "slack-bot"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.read("calls"), "sbx secret set-custom --sandbox ada --env SLACK_BOT_TOKEN --placeholder sbx-cs-ada-slack-bot --host slack.com --command") {
		t.Fatalf("calls:\n%s", h.read("calls"))
	}
}

// A ciphertext that fails the format or the decryption check never replaces the stored one.
func TestBadCiphertextNeverReplacesTheStoredOne(t *testing.T) {
	for _, flag := range []string{"encrypt-bad", "decrypt-bad"} {
		t.Run(flag, func(t *testing.T) {
			h := newFakeHost(t)
			s := h.store()
			if _, _, err := s.Set(context.Background(), "venice"); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(h.root, "ada", "venice.cred")
			before, _ := os.ReadFile(file)
			os.WriteFile(filepath.Join(h.dir, flag), nil, 0o644)
			if _, _, err := s.Set(context.Background(), "venice"); err == nil || !strings.Contains(err.Error(), "unchanged") {
				t.Fatalf("err %v", err)
			}
			if after, _ := os.ReadFile(file); !bytes.Equal(before, after) {
				t.Fatal("the stored ciphertext changed")
			}
			if leftovers, _ := filepath.Glob(filepath.Join(h.root, "ada", ".enroll.*")); len(leftovers) != 0 {
				t.Fatalf("temporary files left: %v", leftovers)
			}
		})
	}
}

func TestSymlinkedFolderIsRefused(t *testing.T) {
	h := newFakeHost(t)
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, h.root)
	if _, _, err := h.store().Set(context.Background(), "venice"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatal("wrote through the symlink")
	}
}

func TestEmptyInputIsRefused(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	s.ReadSecret = func(string) ([]byte, error) { return []byte("  "), nil }
	if _, _, err := s.Set(context.Background(), "venice"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err %v", err)
	}
}

func TestMissingSandboxStoresWithoutWiring(t *testing.T) {
	h := newFakeHost(t)
	os.WriteFile(filepath.Join(h.dir, "inventory.json"), []byte(`[]`), 0o644)
	wired, _, err := h.store().Set(context.Background(), "venice")
	if err != nil || wired || strings.Contains(h.read("calls"), "secret set") {
		t.Fatalf("wired %v err %v calls:\n%s", wired, err, h.read("calls"))
	}
}

func TestCheckReportsEachProblem(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	if _, _, err := s.Set(context.Background(), "venice"); err != nil {
		t.Fatal(err)
	}
	results, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range results {
		got[r.Service] = r.Problem
	}
	// venice is fine; slack-bot was never stored.
	if got["venice"] != "" || got["slack-bot"] != "missing" {
		t.Fatalf("results %v", got)
	}
	os.Chmod(filepath.Join(h.root, "ada", "venice.cred"), 0o644)
	results, _ = s.Check(context.Background())
	if results[0].Problem != "unsafe_file" {
		t.Fatalf("a readable ciphertext passed: %+v", results[0])
	}
	os.Chmod(filepath.Join(h.root, "ada", "venice.cred"), 0o600)
	os.WriteFile(filepath.Join(h.dir, "records.json"), []byte(`{"secrets":[],"custom_secrets":[]}`), 0o644)
	results, _ = s.Check(context.Background())
	if results[0].Problem != "not_wired_to_the_sandbox" {
		t.Fatalf("an unwired service passed: %+v", results[0])
	}
}

// A record this tool never writes (a global one, an old placeholder) is refused, not counted.
func TestUnexpectedRecordIsRefused(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	os.WriteFile(filepath.Join(h.dir, "records.json"), []byte(`{"secrets":[{"scope":"(global)","type":"service","name":"venice"}],"custom_secrets":[{"env":"SLACK_BOT_TOKEN","scope":"ada","placeholder":"sbx-cs-old"}]}`), 0o644)
	for _, name := range []string{"venice", "slack-bot"} {
		if _, _, err := s.Set(context.Background(), name); err == nil || !strings.Contains(err.Error(), "unexpected_sandbox_record") {
			t.Fatalf("%s: err %v", name, err)
		}
	}
	if strings.Contains(h.read("calls"), "secret set") {
		t.Fatal("wired over an unexpected record")
	}
	results, _ := s.Check(context.Background())
	for _, r := range results {
		if r.Problem != "unexpected_sandbox_record" {
			t.Fatalf("check passed an unexpected record: %+v", r)
		}
	}
}

// With no sandbox, check says so instead of claiming the key is wired; with sandboxd down it
// still checks the host side and fails on the wiring it could not see.
func TestCheckWithoutTheSandbox(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	s.Set(context.Background(), "venice")
	os.WriteFile(filepath.Join(h.dir, "inventory.json"), []byte(`[]`), 0o644)
	results, err := s.Check(context.Background())
	if err != nil || results[0].Problem != "" || results[0].Wired || results[1].Problem != "missing" {
		t.Fatalf("results %+v err %v", results, err)
	}
	s.Runner.Probe = func(context.Context) error { return errors.New("down") }
	results, err = s.Check(context.Background())
	if err != nil || results[0].Problem != "sandboxd_unreachable_wiring_unchecked" || results[1].Problem != "missing" {
		t.Fatalf("results %+v err %v", results, err)
	}
}

func TestInterruptChangesNothing(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	s.ReadSecret = func(string) ([]byte, error) { return nil, ErrInterrupted }
	if _, _, err := s.Set(context.Background(), "venice"); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "ada", "venice.cred")); err == nil || strings.Contains(h.read("calls"), "encrypt") {
		t.Fatal("an interrupted set stored something")
	}
}

func TestAPublishedKitDeclaresItsServicesInServicesJSON(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, ServicesFile), []byte(`{"services":[
 {"name":"venice","prompt":"Venice API key","required":true,"delivery":"native","domains":["api.venice.ai"]},
 {"name":"slack-bot","prompt":"Slack bot token","required":false,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-bot"}]}`), 0o644)
	// A secrets.json beside it is never read: services.json is the published kit's declaration.
	os.WriteFile(filepath.Join(base, "secrets.json"), []byte(`not json`), 0o644)
	services, err := Services("jane", []string{"venice"}, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 || services[0].Name != "venice" || services[0].Placeholder ||
		!services[1].Placeholder || services[1].Value != "sbx-cs-jane-slack-bot" || services[1].Env != "SLACK_BOT_TOKEN" {
		t.Fatalf("services %+v", services)
	}
	declared, err := KitServices(base)
	if err != nil || len(declared) != 2 || !declared[0].Required || !declared[1].Slack {
		t.Fatalf("declared %+v, %v", declared, err)
	}
}

// Help is counted in characters, as the kits' own test counts it: 500 accented ones are allowed.
func TestHelpIsCountedInCharacters(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, ServicesFile), []byte(`{"services":[{"name":"venice","prompt":"Venice API key","help":"`+
		strings.Repeat("é", 500)+`","required":true,"delivery":"native","domains":["api.venice.ai"]}]}`), 0o644)
	if declared, err := KitServices(base); err != nil || declared[0].Help != strings.Repeat("é", 500) {
		t.Fatalf("declared %+v, %v", declared, err)
	}
}

func TestMalformedServicesAreRefused(t *testing.T) {
	native := `{"name":"venice","prompt":"Venice API key","required":true,"delivery":"native","domains":["api.venice.ai"]}`
	for name, entry := range map[string]string{
		"unknown delivery":         `{"name":"x","prompt":"p","delivery":"magic","domains":["api.venice.ai"]}`,
		"native without domains":   `{"name":"x","prompt":"p","delivery":"native"}`,
		"native with a bad domain": `{"name":"x","prompt":"p","delivery":"native","domains":["*.venice.ai"]}`,
		"placeholder for one name": `{"name":"x","prompt":"p","delivery":"placeholder","env":"X","hosts":["x.com"],"placeholder":"fixed"}`,
		"no prompt":                `{"name":"x","prompt":" ","delivery":"native","domains":["x.com"]}`,
		"a repeated name":          native,
		"an unknown field":         `{"name":"x","prompt":"p","delivery":"native","domains":["x.com"],"secret":"y"}`,
		"help with a control code": `{"name":"x","prompt":"p","help":"see \u001b[31mhere","delivery":"native","domains":["x.com"]}`,
		"help too long":            `{"name":"x","prompt":"p","help":"` + strings.Repeat("h", 501) + `","delivery":"native","domains":["x.com"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			os.WriteFile(filepath.Join(base, ServicesFile), []byte(`{"services":[`+native+`,`+entry+`]}`), 0o644)
			if _, err := KitServices(base); err == nil {
				t.Fatal("accepted")
			}
			if _, err := Services("jane", nil, base); err == nil {
				t.Fatal("Services accepted")
			}
		})
	}
}

// WireNew reads the records once before and once after, with one write per placeholder: no
// inventory and no listing per service (each sbx call costs seconds).
func TestWireNewListsOnceBeforeAndOnceAfter(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	if _, _, err := s.Set(context.Background(), "slack-bot"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(h.dir, "calls"))
	// The new sandbox holds its native record (from sbxenv.yaml) and nothing else yet.
	os.WriteFile(filepath.Join(h.dir, "records.json"), []byte(`{"secrets":[{"scope":"ada","type":"service","name":"venice"}],"custom_secrets":[]}`), 0o644)
	if _, _, err := s.Set(context.Background(), "venice"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(h.dir, "calls"))
	if err := s.WireNew(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sbxCalls []string
	for _, line := range strings.Split(strings.TrimSpace(h.read("calls")), "\n") {
		if strings.HasPrefix(line, "sbx ") {
			sbxCalls = append(sbxCalls, strings.Fields(line)[1]+" "+strings.Fields(line)[2])
		}
	}
	if strings.Join(sbxCalls, ",") != "secret ls,secret set-custom,secret ls" {
		t.Fatalf("sbx calls %v", sbxCalls)
	}
	// A foreign record stops it before any write.
	os.Remove(filepath.Join(h.dir, "calls"))
	os.WriteFile(filepath.Join(h.dir, "records.json"), []byte(`{"secrets":[],"custom_secrets":[{"env":"SLACK_BOT_TOKEN","scope":"ada","placeholder":"sbx-cs-old"}]}`), 0o644)
	if err := s.WireNew(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected_sandbox_record") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(h.read("calls"), "secret set") {
		t.Fatal("wrote over a foreign record")
	}
}

// A store for a sandbox that cannot exist yet stores the key and asks sandboxd nothing.
func TestAnUnbornStoreNeverAsksSandboxd(t *testing.T) {
	h := newFakeHost(t)
	s := h.store()
	s.Unborn = true
	wired, _, err := s.Set(context.Background(), "venice")
	if err != nil || wired || strings.Contains(h.read("calls"), "sbx ") {
		t.Fatalf("wired %v err %v calls:\n%s", wired, err, h.read("calls"))
	}
}
