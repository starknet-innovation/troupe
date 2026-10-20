package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/starknet-innovation/troupe/internal/agent"
	"github.com/starknet-innovation/troupe/internal/budget"
	"github.com/starknet-innovation/troupe/internal/config"
	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/published"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// createFlags are create's options for a published agent.
type createFlags struct {
	name, soul, avatar          string
	cpus, memoryGiB, storageGiB int
	force                       bool
}

// createPublished creates a new agent from the latest published TYPE kit: it pins that version,
// keeps the kit's files, asks for the agent's keys, writes the agent's folder and runs the same
// create transaction a local agent's does. Run again after a failure, it resumes the transaction.
func createPublished(e env, typ string, f createFlags) int {
	display, name, err := agentNames(f.name)
	if err != nil {
		return usageError(e, createHelp, "create: %v", err)
	}
	if _, ok := published.TypeNamed(typ); !ok {
		return usageError(e, createHelp, "create: TYPE must be %s", typeList())
	}
	if strings.HasSuffix(name, "-lab") {
		return usageError(e, createHelp, "create: a name ending in -lab is a Lab's, never an agent's")
	}
	if reservedName(name) {
		return usageError(e, createHelp, "create: %s is a name troupe reserves", name)
	}
	machine, err := config.LoadMachine()
	if err != nil {
		return fail(e, err)
	}
	printer := progress.New(display, machine.Location())
	printer.Out, printer.Err = e.out, e.err
	// The machine first: a problem found later would stop the creation partway.
	if err := machineReady(printer); err != nil {
		return fail(e, err)
	}
	// sudo first, before any work or other prompt, and kept fresh until the end: storing keys
	// and the create itself need it, and a password prompt must never come midway.
	stopSudo, err := askSudo(e.ctx)
	if err != nil {
		return fail(e, err)
	}
	defer stopSudo()
	if display != name {
		printer.Print("create", progress.Info, display+"'s ID is "+name+": every other command takes it")
	}
	report := func(step string, err error) int {
		printer.Print(step, progress.Failed, err.Error())
		return 1
	}
	reg, _, err := e.registry()
	if err != nil {
		return fail(e, err)
	}
	folder := filepath.Join(reg.Data, "agents", name)
	// A folder troupe wrote and a create that has not finished: resume it.
	if source, err := readSource(folder); err == nil {
		if source.Type != typ {
			return report("create", fmt.Errorf("%s is a %s agent, not %s", name, source.Type, typ))
		}
		if agent.Created(name) {
			return report("create", fmt.Errorf("%s already exists", name))
		}
		if f.soul != "" || f.avatar != "" || f.cpus != 0 || f.memoryGiB != 0 || f.storageGiB != 0 {
			return report("create", errors.New("a resumed creation keeps the sizes, SOUL and picture it began with; run it again without them"))
		}
		printer.Print("create", progress.Info, "Resuming the creation of "+name)
		return runRecreate(e, name, true, askBudget(f.force))
	}
	if names, err := reg.Names(); err != nil {
		return fail(e, err)
	} else if contains(names, name) {
		return report("create", fmt.Errorf("an agent named %s already exists", name))
	}
	keys := filepath.Join(secret.DefaultRoot(), name)
	if agent.Existed(name) || exists(keys) || exists(folder) {
		return report("create", fmt.Errorf("an agent named %s existed on this machine before; its records or keys remain", name))
	}

	logPath := agent.LogFile(name, "create.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fail(e, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(e, err)
	}
	defer logFile.Close()
	docker := published.Docker{Log: sbx.LogWriter(logFile)}
	runner := sbx.Runner{Paths: guard.Default(), Log: logPath}

	// No sandbox of this name may exist, troupe's or not: keys are wired by sandbox name.
	inventory, err := runner.Inventory(e.ctx)
	if err != nil {
		return report("create", err)
	}
	if _, taken := inventory[name]; taken {
		return report("create", fmt.Errorf("a sandbox named %s already exists", name))
	}
	// Claim the name: the agent's keys folder, created exclusively, so a second create of the
	// same name stops here and this one removes only what it made.
	if err := os.MkdirAll(secret.DefaultRoot(), 0o700); err != nil {
		return fail(e, err)
	}
	if err := os.Mkdir(keys, 0o700); err != nil {
		return report("create", fmt.Errorf("an agent named %s is being created or existed before", name))
	}
	created := false
	defer func() {
		if !created {
			os.RemoveAll(keys)
		}
	}()

	// The kit: latest, pinned, its files kept.
	repository := machine.Registry + "/" + typ
	host, _, _ := strings.Cut(machine.Registry, "/")
	printer.Print("kit", progress.Started, "Finding the latest "+repository+" kit")
	img, err := resolve(e, docker, runner, repository, host)
	if err != nil {
		return report("kit", err)
	}
	kit, err := docker.Extract(e.ctx, reg.Data, img)
	if err != nil {
		return report("kit", err)
	}
	printer.Print("kit", progress.OK, fmt.Sprintf("%s %s, pinned", typ, img.Version))

	sizes, err := published.ReadDefaults(kit)
	if err != nil {
		return report("kit", err)
	}
	settings, err := kitSettings(kit)
	if err != nil {
		return report("kit", err)
	}
	hasSlack := exists(filepath.Join(kit, "slack-app-manifest.json"))
	if hasSlack && !exists(filepath.Join(kit, "env")) {
		return report("kit", fmt.Errorf("%s %s is too old for troupe create: it has Slack but no env template", typ, img.Version))
	}
	// troupe's help describes each type's kit as it was; a newer kit is followed, and said so.
	if asks, err := kitAsks(kit, settings); err != nil {
		return report("kit", err)
	} else if t, _ := published.TypeNamed(typ); t.Differs(sizes, asks) {
		printer.Print("kit", progress.Info, fmt.Sprintf("%s %s's sizes or questions differ from this troupe's help; troupe follows the kit", typ, img.Version))
	}
	if f.cpus > 0 {
		sizes.CPUs = f.cpus
	}
	if f.memoryGiB > 0 {
		sizes.MemoryGiB = f.memoryGiB
	}
	if f.storageGiB > 0 {
		sizes.RootGiB = f.storageGiB
	}
	// The budget before any key is asked: an agent this machine cannot hold stops here.
	if err := checkBudget(e, runner, printer, budget.Want{Name: name, CPUs: sizes.CPUs, MemoryGiB: sizes.MemoryGiB,
		DiskGrowthGiB: sizes.RootGiB + sizes.DockerGiB, NewVM: true}, askBudget(f.force)); err != nil {
		if errors.Is(err, agent.ErrInterrupted) {
			return report("budget", errors.New("interrupted; nothing was kept"))
		}
		return report("budget", err)
	}
	// The profile picture, before any key: a bad one is refused while nothing is stored.
	var avatar []byte
	ext := ""
	if f.avatar != "" {
		if avatar, ext, err = readAvatar(e.ctx, f.avatar); err != nil {
			return report("avatar", err)
		}
	}
	soul, err := readSoul(f.soul, kit, display)
	if err != nil {
		return report("create", err)
	}
	if len(avatar) > 0 {
		soul = append(soul, []byte(fmt.Sprintf("\n\nYour profile picture is `/home/agent/.hermes/assets/profile-picture.%s`. "+
			"Look at it when your appearance matters.\n", ext))...)
	}

	// The kit's settings before the keys: a refused answer costs no key or Slack app.
	answers, err := askSettings(e, printer, settings)
	if err != nil {
		if errors.Is(err, secret.ErrInterrupted) {
			return report("settings", errors.New("interrupted; nothing was kept"))
		}
		return report("settings", err)
	}
	// The keys, before anything names them, so a failure here leaves nothing behind. Nothing is
	// written beside the operator: the Slack link carries the manifest, and the icon step names
	// the picture given with --avatar (a file, or an address to download it from).
	given, err := askKeys(e, printer, kit, name, display, len(avatar) > 0, runner)
	if err != nil {
		if errors.Is(err, secret.ErrInterrupted) {
			return report("keys", errors.New("interrupted; nothing was kept"))
		}
		return report("keys", err)
	}
	given.settings = answers
	agentEnv, slackEnv, err := agentSettings(kit, given, machine)
	if err != nil {
		return report("create", err)
	}
	if _, err := published.Write(reg.Data, published.Agent{Name: name, DisplayName: display, Image: img, Sizes: sizes, Soul: soul,
		Natives: given.natives, Services: given.placeholders, Env: agentEnv, SlackEnv: slackEnv,
		Avatar: avatar, AvatarExt: ext, Created: time.Now()}); err != nil {
		return report("create", err)
	}
	created = true
	return runRecreate(e, name, true, skipBudget) // checked above, before the keys
}

// resolve pins the latest kit. When the registry refuses Docker, or sbx has no credential for it,
// it asks once for a token that reads the kits and gives it to both.
func resolve(e env, docker published.Docker, runner sbx.Runner, repository, host string) (published.Image, error) {
	stored, err := published.HasRegistrySecret(e.ctx, runner, host)
	if err != nil {
		return published.Image{}, err
	}
	img, err := docker.Resolve(e.ctx, repository)
	if err == nil && stored {
		return img, nil
	}
	if err != nil && !errors.Is(err, published.ErrAccess) {
		return published.Image{}, err
	}
	refused := err != nil
	fmt.Fprintf(e.err, "%s needs a token that can read its kits (on GitHub: a classic token with read:packages).\n", host)
	user, err := readLine(e.ctx, "Registry username: ")
	if err != nil {
		return published.Image{}, err
	}
	token, err := secret.TerminalReader(e.ctx)("Registry token (hidden): ")
	defer clear(token)
	if err != nil {
		return published.Image{}, err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`).MatchString(user) || len(strings.TrimSpace(string(token))) == 0 {
		return published.Image{}, errors.New("a username and a token are both needed")
	}
	// Docker gets the token only when it was refused, so a working login (one that can also
	// publish) is never replaced; sbx only when it has none.
	if err := published.StoreToken(e.ctx, docker, runner, host, user, []byte(strings.TrimSpace(string(token))), refused, !stored); err != nil {
		return published.Image{}, err
	}
	img, err = docker.Resolve(e.ctx, repository)
	if errors.Is(err, published.ErrAccess) {
		return published.Image{}, fmt.Errorf("%s refused the token, or has no kit %s", host, repository)
	}
	return img, err
}

// keys is what create collected: the native services the agent's sbxenv declares, the placeholder
// services it was given keys for, its Slack manager when its kit has Slack, and the answers to
// its kit's settings as .env lines.
type keys struct {
	natives      []published.Native
	placeholders []string
	slackManager string
	settings     []string
}

var slackMember = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// askKeys asks for each service the kit declares, in its order, storing each key with the TPM;
// Enter skips an optional one. At the kit's first Slack service it gives the Slack app's setup
// first, and after the last one it asks for the agent's Slack manager.
func askKeys(e env, printer *progress.Printer, kit, name, display string, avatar bool, runner sbx.Runner) (keys, error) {
	var k keys
	declared, err := secret.KitServices(kit)
	if err != nil {
		return k, err
	}
	// The manager comes right after the last Slack key, whether that key was given or skipped.
	lastSlack := -1
	for i, s := range declared {
		if s.Slack {
			lastSlack = i
		}
	}
	shown := false
	for i, s := range declared {
		if s.Slack && !shown {
			if err := slackSteps(printer, kit, display, avatar); err != nil {
				return k, err
			}
			shown = true
		}
		if s.Help != "" {
			printer.Print("keys", progress.Info, s.Help)
		}
		prompt := s.Prompt + " (hidden): "
		if !s.Required {
			prompt = s.Prompt + " (hidden, Enter to skip): "
		}
		value, err := readSecret(e.ctx)(prompt)
		if err != nil {
			return k, err
		}
		if len(strings.TrimSpace(string(value))) == 0 {
			if s.Required {
				return k, fmt.Errorf("%s is required", s.Name)
			}
		} else if err := k.store(e, s, value, name, runner); err != nil {
			return k, err
		}
		if i == lastSlack {
			if k.slackManager, err = askSlackManager(e, printer); err != nil {
				return k, err
			}
		}
	}
	return k, nil
}

// store keeps one service's key with the TPM, to be wired when the agent is created.
func (k *keys) store(e env, s secret.KitService, value []byte, name string, runner sbx.Runner) error {
	svc := secret.Service{Name: s.Name}
	if s.Delivery == "placeholder" {
		svc = secret.Service{Name: s.Name, Placeholder: true, Env: s.Env, Hosts: s.Hosts,
			Value: strings.ReplaceAll(s.Placeholder, "{agent}", name)}
	}
	// The name is claimed and no sandbox has it: the key is stored now, wired at creation.
	store := &secret.Store{Agent: name, Services: []secret.Service{svc}, Root: secret.DefaultRoot(), Runner: runner,
		ReadSecret: func(string) ([]byte, error) { return value, nil }, Unborn: true}
	if _, _, err := store.Set(e.ctx, s.Name); err != nil {
		return err
	}
	if s.Delivery == "native" {
		k.natives = append(k.natives, published.Native{Name: s.Name, Command: store.Command(svc), Domains: s.Domains})
	} else {
		k.placeholders = append(k.placeholders, s.Name)
	}
	return nil
}

// readSecret reads a key without echoing it; tests replace it.
var readSecret = secret.TerminalReader

// slackSteps prints how to create the agent's Slack app: a one-click link that opens Slack's
// create-from-manifest flow pre-filled with the whole manifest, so no file is left behind.
func slackSteps(printer *progress.Printer, kit, display string, avatar bool) error {
	manifest, err := registry.SlackManifestIn(kit, display)
	if err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, manifest); err != nil {
		return err
	}
	link := "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(compact.String())
	// The whole address, which every terminal shows and most open on a click: a clickable label
	// alone (OSC 8) shows as plain text, with no address, where a terminal lacks it. Never last
	// on the line: a progress line ends with a period, which would join the link.
	open := "Open " + link + " to create " + display + "'s Slack app"
	steps := []string{
		open + ": pick the workspace, review and Create",
		// The app-level token first: Slack opens the new app on Basic Information.
		"Basic Information, App-Level Tokens: generate one with the connections:write scope and copy it (xapp-…)",
		"Install to Workspace, then copy the Bot User OAuth Token (xoxb-…)",
	}
	if avatar {
		// The picture as given, never its address here: an address may carry a token.
		steps = append(steps, "Optional: Basic Information, Display Information, App icon: upload the picture given with --avatar (download it first if you gave an address)")
	}
	for i, step := range steps {
		printer.Print("slack", progress.Info, fmt.Sprintf("%d. %s", i+1, step))
	}
	return nil
}

// askSlackManager asks for the agent's manager, as a member ID: the app cannot look people up
// (it has no users:read). The manager is the one person the agent first talks to, and its
// notification home is their DM once they write to it (it cannot open one); they admit
// others, set channels and hand the agent over by telling it in Slack.
func askSlackManager(e env, printer *progress.Printer) (string, error) {
	for tries := 0; ; tries++ {
		manager, err := readLine(e.ctx, "Manager's Slack member ID (U… or W…, usually yours; hand it over later in Slack): ")
		if err != nil {
			return "", err
		}
		if slackMember.MatchString(manager) {
			return manager, nil
		}
		if tries == 2 {
			return "", errors.New("the manager must be a member ID, such as U0123456789")
		}
		printer.Print("slack", progress.Info, "That is not a member ID: open the person's profile, click ⋮ and Copy member ID (U…)")
	}
}

// kitSettings are the settings the kit asks for, checked before any key is asked: none may set a
// line the kit's env template sets, or a placeholder service's variable.
func kitSettings(kit string) ([]published.Setting, error) {
	settings, err := published.ReadSettings(kit)
	if err != nil || len(settings) == 0 {
		return settings, err
	}
	taken := map[string]bool{}
	template, err := os.ReadFile(filepath.Join(kit, "env"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(string(template), "\n") {
		if key, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			taken[key] = true
		}
	}
	if exists(filepath.Join(kit, secret.ServicesFile)) {
		services, err := secret.KitServices(kit)
		if err != nil {
			return nil, err
		}
		for _, s := range services {
			if s.Env != "" {
				taken[s.Env] = true
			}
		}
	}
	for _, s := range settings {
		if taken[s.Env] {
			return nil, fmt.Errorf("settings.json: the kit already sets %s", s.Env)
		}
	}
	return settings, nil
}

// kitAsks is what create will ask for, in order, by name: the kit's settings, its services,
// and "manager" after its last Slack service.
func kitAsks(kit string, settings []published.Setting) ([]published.Ask, error) {
	var asks []published.Ask
	for _, s := range settings {
		asks = append(asks, published.Ask{Name: s.Env, Optional: true})
	}
	services, err := secret.KitServices(kit)
	if err != nil {
		return nil, err
	}
	slack := 0
	for _, s := range services {
		if s.Slack {
			slack++
		}
	}
	for _, s := range services {
		asks = append(asks, published.Ask{Name: s.Name, Optional: !s.Required})
		if s.Slack {
			if slack--; slack == 0 {
				asks = append(asks, published.Ask{Name: "manager"})
			}
		}
	}
	return asks, nil
}

// askSettings asks for each setting the kit declares, in its order, as ENV=value lines for the
// agent's .env. Enter leaves one empty; a third answer that does not match stops. Whatever the
// kit's pattern, an answer holds nothing a dotenv reader treats specially (a comment, quotes,
// an expansion), so every reader of the .env sees the same value.
func askSettings(e env, printer *progress.Printer, settings []published.Setting) ([]string, error) {
	var lines []string
	for _, s := range settings {
		if s.Help != "" {
			printer.Print("settings", progress.Info, s.Help)
		}
		pattern := regexp.MustCompile(`^(?:` + s.Pattern + `)$`)
		for tries := 0; ; tries++ {
			value, err := readLine(e.ctx, s.Prompt+" (Enter to skip): ")
			if err != nil {
				return nil, err
			}
			if value == "" || (pattern.MatchString(value) && !strings.ContainsAny(value, "#\"'$\\`") &&
				strings.IndexFunc(value, func(r rune) bool { return !unicode.IsPrint(r) }) < 0) {
				lines = append(lines, s.Env+"="+value)
				break
			}
			if tries == 2 {
				return nil, fmt.Errorf("%s: the answer is not in the form the kit asks for", s.Env)
			}
			printer.Print("settings", progress.Info, "That is not in the form "+s.Env+" needs: try again, or press Enter to skip")
		}
	}
	return lines, nil
}

// agentSettings are the agent's .env (the kit's env template, then its Slack manager, who is
// also its allowlist and its home, their DM once they write to it, then its kit's settings) and its own slack.env (the workspace, learned at
// its first Slack setup). A kit without a template keeps a comment-only .env.
func agentSettings(kit string, k keys, machine config.Machine) (agentEnv, slackEnv []byte, err error) {
	template, err := os.ReadFile(filepath.Join(kit, "env"))
	if errors.Is(err, os.ErrNotExist) {
		template, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(template) > 0 && !bytes.HasSuffix(template, []byte("\n")) {
		template = append(template, '\n')
	}
	agentEnv = template
	if m := k.slackManager; m != "" {
		agentEnv = append(agentEnv, []byte("SLACK_MANAGER="+m+"\nSLACK_ALLOWED_USERS="+m+"\nSLACK_HOME_CHANNEL="+m+"\n")...)
		slackEnv = []byte("SLACK_WORKSPACE=\n")
	}
	for _, line := range k.settings {
		agentEnv = append(agentEnv, []byte(line+"\n")...)
	}
	return agentEnv, slackEnv, nil
}

// readSoul is the agent's SOUL: --soul's file, else the kit's default with the agent's name, else
// none (Hermes writes its own).
func readSoul(path, kit, display string) ([]byte, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 || len(data) > 1<<20 {
			return nil, errors.New("--soul: the file must hold between 1 byte and 1 MiB")
		}
		return data, nil
	}
	data, err := os.ReadFile(filepath.Join(kit, "SOUL.md"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(strings.ReplaceAll(string(data), "{agent}", display)), nil
}

// source is a published agent's source.json.
type source struct {
	Reference, Type, Version string
	CreatedAt                string `json:"created_at"`
	DisplayName              string `json:"display_name"`
	Services                 []string
}

// addService records that a published agent now has a key for one more of its kit's placeholder
// services, keeping every other field of its source.json as written.
func addService(folder, service string) error {
	path := filepath.Join(folder, "source.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("%s: malformed", path)
	}
	var services []string
	if list, ok := fields["services"].([]any); ok {
		for _, s := range list {
			if name, ok := s.(string); ok {
				services = append(services, name)
			}
		}
	}
	if contains(services, service) {
		return nil
	}
	fields["services"] = append(services, service)
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(path, append(out, '\n'))
}

// replaceFile writes path's new content beside it (0600, synced) and renames it into place, so
// the file is never partial.
func replaceFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

func readSource(folder string) (source, error) {
	var s source
	data, err := os.ReadFile(filepath.Join(folder, "source.json"))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil || registry.KitFolder("", s.Reference) == "" {
		return s, fmt.Errorf("%s: malformed", filepath.Join(folder, "source.json"))
	}
	return s, nil
}

// askSudo asks for sudo now and keeps the credential fresh until stop is called.
func askSudo(ctx context.Context) (stop func(), err error) {
	verify := exec.CommandContext(ctx, "sudo", "-v")
	verify.Stdin, verify.Stdout, verify.Stderr = os.Stdin, os.Stderr, os.Stderr
	if verify.Run() != nil {
		return nil, errors.New("sudo_required")
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Minute):
				exec.Command("sudo", "-n", "-v").Run()
			}
		}
	}()
	return func() { close(done) }, nil
}

// readLine reads one visible line from the controlling terminal; tests replace it.
var readLine = func(ctx context.Context, prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("private_terminal_required")
	}
	defer tty.Close()
	// Closing the terminal ends a pending read when the operator presses Ctrl+C.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			tty.Close()
		case <-done:
		}
	}()
	fmt.Fprint(tty, prompt)
	line, err := bufio.NewReader(io.LimitReader(tty, 8192)).ReadString('\n')
	if ctx.Err() != nil {
		return "", secret.ErrInterrupted
	}
	if err != nil {
		return "", errors.New("no input")
	}
	return strings.TrimSpace(line), nil
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
