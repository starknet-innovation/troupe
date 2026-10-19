package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/published"
	"github.com/starknet-innovation/troupe/internal/registry"
)

var (
	png  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 64)...)
	jpeg = append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{2}, 64)...)
)

// avatarServer serves paths as given, and trusts its own certificate for the test.
func avatarServer(t *testing.T, routes map[string][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-http":
			// A plain http server that does serve a PNG: only the redirect rule can refuse it.
			plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(png) }))
			t.Cleanup(plain.Close)
			http.Redirect(w, r, plain.URL+"/a.png", http.StatusFound)
		case "/to-https":
			http.Redirect(w, r, "/a.png", http.StatusFound)
		default:
			body, ok := routes[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	original := avatarClient
	client := server.Client()
	client.Timeout, client.CheckRedirect = 5*time.Second, original.CheckRedirect
	avatarClient = client
	t.Cleanup(func() { avatarClient = original })
	return server
}

func TestAnAvatarIsAPNGOrJPEGFromAFileOrHTTPS(t *testing.T) {
	server := avatarServer(t, map[string][]byte{"/a.png": png, "/a.jpg": jpeg})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "me.png"), png, 0o644)
	for from, want := range map[string]string{
		filepath.Join(dir, "me.png"): "png",
		server.URL + "/a.png":        "png",
		server.URL + "/a.jpg":        "jpg",
		server.URL + "/to-https":     "png",
	} {
		data, ext, err := readAvatar(context.Background(), from)
		if err != nil || ext != want || len(data) == 0 {
			t.Errorf("%s: %s %v", from, ext, err)
		}
	}
}

func TestAnAvatarThatIsNotAnImageOrTooLargeOrUnsafeIsRefused(t *testing.T) {
	big := append(append([]byte{}, png...), bytes.Repeat([]byte{0}, avatarLimit)...)
	server := avatarServer(t, map[string][]byte{"/text": []byte("<html>hello</html>"), "/big.png": big})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644)
	for name, from := range map[string]string{
		"a text file":          filepath.Join(dir, "notes.txt"),
		"a missing file":       filepath.Join(dir, "absent.png"),
		"a web page":           server.URL + "/text",
		"a picture over 5 MiB": server.URL + "/big.png",
		"plain http":           strings.Replace(server.URL, "https://", "http://", 1) + "/a.png",
		"a redirect to http":   server.URL + "/to-http",
		"a missing page":       server.URL + "/absent.png",
	} {
		if _, _, err := readAvatar(context.Background(), from); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// kitWithSlack is a published coder-like kit's folder: its Slack manifest and its env template.
func kitWithSlack(t *testing.T) string {
	t.Helper()
	kit := t.TempDir()
	os.WriteFile(filepath.Join(kit, "slack-app-manifest.json"), []byte(`{"display_information":{"name":"AGENT"},"features":{"bot_user":{"display_name":"AGENT"}},"oauth_config":{"scopes":{"bot":["chat:write"]}}}`), 0o644)
	os.WriteFile(filepath.Join(kit, "env"), []byte("# kit settings\nVITE_CONFIG_NATIVE_IGNORE_WARNING=true\nSLACK_ALLOW_BOTS=none"), 0o644)
	return kit
}

// The steps leave nothing in the operator's folder: the link carries the whole manifest.
func TestTheSlackStepsAreAOneClickLinkAndWriteNothing(t *testing.T) {
	kit := kitWithSlack(t)
	dir := t.TempDir()
	t.Chdir(dir)
	var out bytes.Buffer
	printer := &progress.Printer{Agent: "Mr. Doyle", Location: time.UTC, Out: &out, Err: &out, Now: time.Now}
	if err := slackSteps(printer, kit, "Mr. Doyle", true); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v in the operator's folder", entries)
	}
	text := out.String()
	i := strings.Index(text, "https://api.slack.com/apps?new_app=1&manifest_json=")
	if i < 0 {
		t.Fatalf("no link:\n%s", text)
	}
	link, _ := url.Parse(strings.Fields(text[i:])[0])
	var decoded map[string]any
	if err := json.Unmarshal([]byte(link.Query().Get("manifest_json")), &decoded); err != nil {
		t.Fatalf("the link's manifest: %v", err)
	}
	if info, _ := decoded["display_information"].(map[string]any); info["name"] != "Mr. Doyle" {
		t.Fatalf("the link's manifest names %v", decoded["display_information"])
	}
	for _, want := range []string{"xapp-", "xoxb-", "connections:write", "upload the picture given with --avatar"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the steps lack %q:\n%s", want, text)
		}
	}
	// In the order the tokens are asked for: the app-level token, then the bot token.
	if strings.Index(text, "xapp-") > strings.Index(text, "xoxb-") {
		t.Fatalf("the bot token comes before the app-level token:\n%s", text)
	}
}

func TestAnAgentsSettingsComeFromTheKitTemplateAndItsSlackAnswers(t *testing.T) {
	kit := kitWithSlack(t)
	k := keys{slackManager: "U0ONE1234"}
	env, slackEnv, err := agentSettings(kit, k, config.Machine{BoundaryPublic: "C0PUB12345"})
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "# kit settings\nVITE_CONFIG_NATIVE_IGNORE_WARNING=true\nSLACK_ALLOW_BOTS=none\nSLACK_MANAGER=U0ONE1234\nSLACK_ALLOWED_USERS=U0ONE1234\nSLACK_HOME_CHANNEL=U0ONE1234\n" {
		t.Fatalf("env %q", env)
	}
	if string(slackEnv) != "SLACK_WORKSPACE=\n" {
		t.Fatalf("slack.env %q", slackEnv)
	}
	// The kit's settings come last, as answered.
	k.settings = []string{"TEAM_NAME=blue", "REGION="}
	env, _, err = agentSettings(kit, k, config.Machine{})
	if err != nil || !strings.HasSuffix(string(env), "SLACK_HOME_CHANNEL=U0ONE1234\nTEAM_NAME=blue\nREGION=\n") {
		t.Fatalf("env %q %v", env, err)
	}
	// A kit without Slack or a template: no Slack settings, the comment-only .env.
	env, slackEnv, err = agentSettings(t.TempDir(), keys{}, config.Machine{})
	if err != nil || len(env) != 0 || len(slackEnv) != 0 {
		t.Fatalf("%q %q %v", env, slackEnv, err)
	}
}

// A setting may not set a line the kit's template already sets, or a placeholder's variable.
func TestAKitSettingCannotShadowTheKitsOwnLines(t *testing.T) {
	kit := kitWithSlack(t)
	write := func(env string) {
		os.WriteFile(filepath.Join(kit, "settings.json"), []byte(`{"settings":[{"env":"`+env+`","prompt":"P","pattern":"x"}]}`), 0o644)
	}
	write("TEAM_NAME")
	if settings, err := kitSettings(kit); err != nil || len(settings) != 1 {
		t.Fatalf("%v %v", settings, err)
	}
	write("VITE_CONFIG_NATIVE_IGNORE_WARNING")
	if _, err := kitSettings(kit); err == nil {
		t.Fatal("a template line accepted")
	}
	os.WriteFile(filepath.Join(kit, "services.json"), []byte(`{"services":[
 {"name":"starkscan","prompt":"Starkscan","delivery":"placeholder","env":"STARKSCAN_API_KEY","hosts":["api.starkscan.co"],"placeholder":"sbx-starkscan-{agent}"}]}`), 0o644)
	write("STARKSCAN_API_KEY")
	if _, err := kitSettings(kit); err == nil {
		t.Fatal("a placeholder's variable accepted")
	}
	if settings, err := kitSettings(t.TempDir()); err != nil || settings != nil {
		t.Fatalf("a kit without settings: %v %v", settings, err)
	}
}

// Each setting is asked in order; Enter leaves it empty, a wrong answer is asked again, and a
// third wrong one stops.
func TestKitSettingsAreAskedAndMatchTheirPattern(t *testing.T) {
	defer func(saved func(context.Context, string) (string, error)) { readLine = saved }(readLine)
	var out bytes.Buffer
	printer := &progress.Printer{Agent: "Jane", Location: time.UTC, Out: &out, Err: &out, Now: time.Now}
	e := env{ctx: context.Background(), out: &out, err: &out}
	settings := []published.Setting{
		{Env: "TEAM_NAME", Prompt: "Team name", Help: "Change it later in chat.", Pattern: "[a-z]+(,[a-z]+)*"},
		{Env: "REGION", Prompt: "Region", Pattern: "eu|us"},
	}
	var prompts []string
	answers := []string{"Blue Team", "blue,red", ""}
	readLine = func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
	lines, err := askSettings(e, printer, settings)
	if err != nil || strings.Join(lines, "\n") != "TEAM_NAME=blue,red\nREGION=" {
		t.Fatalf("%q %v", lines, err)
	}
	if prompts[0] != "Team name (Enter to skip): " || !strings.Contains(out.String(), "Change it later in chat.") ||
		!strings.Contains(out.String(), "try again") {
		t.Fatalf("%q\n%s", prompts, out.String())
	}
	// The pattern matches the whole answer, never a part of it.
	readLine = func(context.Context, string) (string, error) { return "eu-west", nil }
	if _, err := askSettings(e, printer, settings[1:]); err == nil {
		t.Fatal("three wrong answers accepted")
	}
	// Even a pattern that admits anything never lets a dotenv comment, quote or expansion through.
	for _, answer := range []string{"a #b", `"a"`, "'a'", "$HOME", `a\b`, "`a`", "a\x1bb"} {
		readLine = func(context.Context, string) (string, error) { return answer, nil }
		if _, err := askSettings(e, printer, []published.Setting{{Env: "ANY", Prompt: "Any", Pattern: ".*"}}); err == nil {
			t.Fatalf("%q accepted", answer)
		}
	}
}

func TestAddServiceRecordsAKitServiceOnce(t *testing.T) {
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "source.json"), []byte(`{"reference":"ghcr.io/x/coder:1.3.1","type":"coder","services":["slack-bot"]}`), 0o600)
	for i := 0; i < 2; i++ {
		if err := addService(folder, "starkscan"); err != nil {
			t.Fatal(err)
		}
	}
	s, err := readSource(folder)
	if err != nil || strings.Join(s.Services, ",") != "slack-bot,starkscan" || s.Type != "coder" {
		t.Fatalf("%+v %v", s, err)
	}
	_ = registry.Registry{}
}

// A published agent's store holds the kit's natives it declares and only the placeholder
// services it was given keys for; another service joins once secret set records it.
func TestAPublishedAgentWiresOnlyTheServicesItWasGiven(t *testing.T) {
	h, _ := home(t)
	ref := "ghcr.io/example/coder:1.3.1"
	data := filepath.Join(h, ".local/share/troupe")
	kit := filepath.Join(data, "kits", "coder-1.3.1")
	folder := filepath.Join(data, "agents/jane")
	for name, text := range map[string]string{
		filepath.Join(kit, "kit.json"): `{"reference":"` + ref + `","type":"coder","version":"1.3.1","commit":"` + strings.Repeat("b", 40) + `"}`,
		filepath.Join(kit, "services.json"): `{"services":[
 {"name":"venice","prompt":"Venice","required":true,"delivery":"native","domains":["api.venice.ai"]},
 {"name":"slack-bot","prompt":"bot","required":true,"slack":true,"delivery":"placeholder","env":"SLACK_BOT_TOKEN","hosts":["slack.com"],"placeholder":"sbx-cs-{agent}-slack-bot"},
 {"name":"starkscan","prompt":"Starkscan","required":false,"delivery":"placeholder","env":"STARKSCAN_API_KEY","hosts":["api.starkscan.co"],"placeholder":"sbx-starkscan-{agent}"}]}`,
		filepath.Join(folder, "sbxenv.yaml"): "schemaVersion: \"1\"\nname: jane\nagent: coder\n\nkits:\n  - \"" + ref + "\"\n  - \".\"\n\nsecrets:\n  venice:\n    command: x\n",
		filepath.Join(folder, "source.json"): `{"reference":"` + ref + `","type":"coder","services":["slack-bot"]}`,
	} {
		os.MkdirAll(filepath.Dir(name), 0o700)
		os.WriteFile(name, []byte(text), 0o600)
	}
	e := env{ctx: context.Background(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	names := func() string {
		store, err := agentStore(e, "jane", &agent.Agent{})
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, s := range store.Services {
			found = append(found, s.Name)
		}
		return strings.Join(found, ",")
	}
	if got := names(); got != "venice,slack-bot" {
		t.Fatalf("services %s", got)
	}
	// secret set: the kit's service joins the store at once, the record only after the key.
	store, _ := agentStore(e, "jane", &agent.Agent{})
	folder2, err := kitService(e, "jane", "starkscan", store)
	if err != nil || folder2 != folder || store.Services[len(store.Services)-1].Name != "starkscan" {
		t.Fatalf("kitService %q %v", folder2, err)
	}
	if got := names(); got != "venice,slack-bot" {
		t.Fatalf("recorded before the key was stored: %s", got)
	}
	if err := addService(folder2, "starkscan"); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "venice,slack-bot,starkscan" {
		t.Fatalf("after recording: %s", got)
	}
	// A name the kit does not declare is never added.
	if folder3, err := kitService(e, "jane", "other", store); err != nil || folder3 != "" {
		t.Fatalf("other %q %v", folder3, err)
	}
}

// The Slack link is the whole address, plain, even on a terminal: a terminal without clickable
// labels would show a label alone, with no address to open.
func TestTheSlackLinkIsTheWholeAddress(t *testing.T) {
	kit := kitWithSlack(t)
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	printer := &progress.Printer{Agent: "Jane", Location: time.UTC, Out: &out, Err: &out, Now: time.Now}
	if err := slackSteps(printer, kit, "Jane", false); err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(out.String(), "\n", 2)[0]
	if !strings.Contains(first, "Open https://api.slack.com/apps?new_app=1&manifest_json=") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("first step %q", first)
	}
	// No picture, no icon step.
	if strings.Contains(out.String(), "App icon") {
		t.Fatalf("an icon step without a picture:\n%s", out.String())
	}
}

// The manager is asked as a member ID; a wrong answer says where to find one, and a third stops.
func TestTheManagerIsAMemberID(t *testing.T) {
	defer func(saved func(context.Context, string) (string, error)) { readLine = saved }(readLine)
	var out bytes.Buffer
	printer := &progress.Printer{Agent: "Jane", Location: time.UTC, Out: &out, Err: &out, Now: time.Now}
	e := env{ctx: context.Background(), out: &out, err: &out}
	answers := []string{"@jane", "C0123456789", "W0123456789"}
	readLine = func(context.Context, string) (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	if manager, err := askSlackManager(e, printer); err != nil || manager != "W0123456789" {
		t.Fatalf("%q %v", manager, err)
	}
	if !strings.Contains(out.String(), "Copy member ID") {
		t.Fatalf("no hint:\n%s", out.String())
	}
	readLine = func(context.Context, string) (string, error) { return "Jane", nil }
	if _, err := askSlackManager(e, printer); err == nil {
		t.Fatal("three wrong answers accepted")
	}
}
