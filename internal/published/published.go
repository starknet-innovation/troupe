// Package published turns a published agent kit into what troupe needs on the host: it resolves
// a kit's latest version to that version's own tag, keeps the kit's /usr/share/troupe/ files in
// troupe's data folder (laid out like a local kit folder, so the rest of troupe reads it
// unchanged), and writes a new agent's folder on that kit.
//
// The kit is the publisher's, read through the operator's Docker: its files are read from a tar
// stream with a size cap, as plain files and folders only.
package published

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// Image is one published kit version, pinned.
type Image struct {
	Reference string // REGISTRY/TYPE:VERSION
	Type      string // the repository's last component: sbx names the kit after it
	Version   string // the image's org.opencontainers.image.version label
	Hermes    string // the image's org.host.hermes.version label: the Hermes version
}

// ErrAccess means the registry refused this machine's Docker login (or there is none).
var ErrAccess = errors.New("registry_access_refused")

// Where a kit keeps what troupe reads, and what it holds.
const (
	troupeFiles = "/usr/share/troupe"
	fileLimit   = 64 << 20 // every file of one kit together
	entryLimit  = 4096
)

var (
	semver    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	container = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	// Refusals Docker prints for a missing or refused login.
	refused = regexp.MustCompile(`(?i)unauthorized|denied|authentication required|\b401\b|\b403\b`)
)

// Docker runs the Docker CLI with a deadline, logging the command and its output escaped and
// capped; out, when set, receives stdout instead (unlogged).
type Docker struct {
	Log io.Writer
}

func (d Docker) run(ctx context.Context, timeout time.Duration, out io.Writer, args ...string) (string, error) {
	fmt.Fprintf(d.Log, "$ docker %s\n", strings.Join(args, " "))
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(call, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.MultiWriter(&stderr, d.Log)
	if out != nil {
		cmd.Stdout = out
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if out == nil {
		d.Log.Write(stdout.Bytes())
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		if refused.Match(stderr.Bytes()) {
			return "", ErrAccess
		}
		return "", fmt.Errorf("docker %s failed", args[0])
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Resolve pulls repository:latest (REGISTRY/TYPE), reads the kit and Hermes versions its
// labels name, and pins that version's own tag, which never moves: it pulls REGISTRY/TYPE:VERSION.
func (d Docker) Resolve(ctx context.Context, repository string) (Image, error) {
	latest := repository + ":latest"
	if _, err := d.run(ctx, 60*time.Minute, nil, "pull", "--quiet", latest); err != nil {
		return Image{}, err
	}
	out, err := d.run(ctx, time.Minute, nil, "image", "inspect", latest, "--format",
		`{{index .Config.Labels "org.opencontainers.image.version"}} {{index .Config.Labels "org.host.hermes.version"}}`)
	if err != nil {
		return Image{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return Image{}, fmt.Errorf("%s: unreadable image labels", latest)
	}
	img := Image{Type: path.Base(repository), Version: fields[0], Hermes: fields[1]}
	switch {
	case !semver.MatchString(img.Version):
		return Image{}, fmt.Errorf("%s: no MAJOR.MINOR.PATCH version label", latest)
	case !semver.MatchString(img.Hermes):
		return Image{}, fmt.Errorf("%s: no Hermes version label", latest)
	}
	img.Reference = repository + ":" + img.Version
	if _, err := d.run(ctx, 60*time.Minute, nil, "pull", "--quiet", img.Reference); err != nil {
		return Image{}, err
	}
	return img, nil
}

// Public reports whether anyone can read the kit at reference, without a login: Docker reads its
// manifest with an empty configuration, so no stored credential is used. sbx and its daemon
// read a public kit without one too (measured with sbx 0.50.1), so no token is needed.
func (d Docker) Public(ctx context.Context, reference string) bool {
	empty, err := os.MkdirTemp("", "troupe-docker-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(empty)
	call, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(call, "docker", "manifest", "inspect", reference)
	// DOCKER_AUTH_CONFIG carries credentials whatever the configuration folder holds.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "DOCKER_CONFIG=") && !strings.HasPrefix(v, "DOCKER_AUTH_CONFIG=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+empty)
	fmt.Fprintf(d.Log, "$ DOCKER_CONFIG=(empty) docker manifest inspect %s\n", reference)
	cmd.Stderr = d.Log
	return cmd.Run() == nil
}

// Extract keeps the pinned kit's troupe files in data/kits/TYPE-VERSION once: services.json,
// defaults.json and the rest at the top, its seeds under files/home/.hermes, and troupe's own
// record (kit.json). A folder already there is reused.
func (d Docker) Extract(ctx context.Context, data string, img Image) (string, error) {
	folder := registry.KitFolder(data, img.Reference)
	if folder == "" {
		return "", fmt.Errorf("%s is not a pinned reference", img.Reference)
	}
	if kept, err := registry.ReadPublished(folder); err == nil {
		if kept.Reference != img.Reference {
			return "", fmt.Errorf("%s holds %s, not %s", folder, kept.Reference, img.Reference)
		}
		return folder, nil
	}
	if err := os.MkdirAll(filepath.Dir(folder), 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(folder), ".extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	id, err := d.run(ctx, 10*time.Minute, nil, "create", img.Reference)
	if err != nil {
		return "", err
	}
	if !container.MatchString(id) {
		return "", errors.New("docker create gave no container ID")
	}
	defer d.run(context.Background(), time.Minute, nil, "rm", id)
	reader, writer := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		_, err := d.run(ctx, 10*time.Minute, writer, "cp", id+":"+troupeFiles, "-")
		writer.CloseWithError(err)
		copied <- err
	}()
	unpackErr := unpack(reader, staging)
	reader.CloseWithError(errors.New("stopped"))
	if err := <-copied; err != nil && unpackErr == nil {
		return "", err
	}
	if unpackErr != nil {
		return "", unpackErr
	}
	// Kits before 1.2.0 carry no copy of their seeds, which troupe checks the new VM against.
	for _, need := range []string{"services.json", "defaults.json", "files/home/.hermes/config.yaml"} {
		if info, err := os.Lstat(filepath.Join(staging, filepath.FromSlash(need))); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s: the kit has no %s; it is too old for troupe create", img.Reference, need)
		}
	}
	record, err := json.MarshalIndent(registry.Published{Reference: img.Reference, Type: img.Type,
		Version: img.Version, Hermes: img.Hermes}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, registry.PublishedFile), append(record, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(staging, folder); err != nil {
		return "", err
	}
	return folder, nil
}

// unpack writes the tar stream of /usr/share/troupe (docker cp names its entries troupe/...)
// into dir: plain files and folders only, no path outside it, the seeds/ folder as
// files/home/.hermes, and at most fileLimit bytes in all.
func unpack(stream io.Reader, dir string) error {
	archive := tar.NewReader(stream)
	total, entries := int64(0), 0
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("the kit's files: %w", err)
		}
		if entries++; entries > entryLimit {
			return errors.New("the kit's files: too many entries")
		}
		name := path.Clean(header.Name)
		top, rest, _ := strings.Cut(name, "/")
		if top != path.Base(troupeFiles) || strings.HasPrefix(name, "/") || strings.Contains("/"+name+"/", "/../") {
			return fmt.Errorf("the kit's files: unexpected entry %q", header.Name)
		}
		if rest == "" {
			continue
		}
		if first, after, _ := strings.Cut(rest, "/"); first == "seeds" {
			rest = path.Join("files/home/.hermes", after)
		}
		target := filepath.Join(dir, filepath.FromSlash(rest))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += header.Size; total > fileLimit {
				return errors.New("the kit's files are larger than 64 MiB")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(file, io.LimitReader(archive, header.Size))
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("the kit's files: %q is not a plain file or folder", header.Name)
		}
	}
}

// Defaults are a kit's default sizes (defaults.json).
type Defaults struct {
	CPUs      int `json:"cpus"`
	MemoryGiB int `json:"memory_gib"`
	RootGiB   int `json:"root_gib"`
	DockerGiB int `json:"docker_gib"`
}

// ReadDefaults reads a published kit folder's defaults.json.
func ReadDefaults(folder string) (Defaults, error) {
	var d Defaults
	data, err := os.ReadFile(filepath.Join(folder, "defaults.json"))
	if err != nil {
		return d, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil || d.CPUs < 1 || d.MemoryGiB < 1 || d.RootGiB < 1 || d.DockerGiB < 0 {
		return Defaults{}, fmt.Errorf("%s: malformed defaults", filepath.Join(folder, "defaults.json"))
	}
	return d, nil
}

// Setting is one value a kit's settings.json asks for at create, never a key: the answer goes
// into the agent's .env as ENV=value. Every setting is optional; a non-empty answer must match
// Pattern whole.
type Setting struct {
	Env     string `json:"env"`
	Prompt  string `json:"prompt"`
	Help    string `json:"help"` // printed before the prompt: what the value is and how to change it later
	Pattern string `json:"pattern"`
}

var settingEnv = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// ReadSettings reads and checks a kit's settings.json, in the order create asks; a kit without
// one has none. Slack's settings are troupe's own, never a kit's.
func ReadSettings(folder string) ([]Setting, error) {
	path := filepath.Join(folder, "settings.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var declared struct {
		Settings []Setting `json:"settings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declared); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, s := range declared.Settings {
		_, compileErr := regexp.Compile(s.Pattern)
		if !settingEnv.MatchString(s.Env) || strings.HasPrefix(s.Env, "SLACK_") || seen[s.Env] ||
			strings.TrimSpace(s.Prompt) == "" || len(s.Prompt) > 200 || len([]rune(s.Help)) > 500 ||
			strings.IndexFunc(s.Prompt+s.Help, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 ||
			s.Pattern == "" || compileErr != nil {
			return nil, fmt.Errorf("%s: setting %q is malformed", path, s.Env)
		}
		seen[s.Env] = true
	}
	return declared.Settings, nil
}

// Native is one native service an agent's sbxenv declares: the binding's domains and the
// command sandboxd runs to decrypt the key.
type Native struct {
	Name, Command string
	Domains       []string
}

// Agent is what a new published agent's folder holds.
type Agent struct {
	Name        string // the ID: the sandbox's name, the folders', the keys'
	DisplayName string // the name people read, as given
	Image       Image
	Sizes       Defaults
	Soul        []byte // the agent's SOUL.md; empty leaves Hermes's own default
	Natives     []Native
	Created     time.Time
	// Env is the agent's .env (its settings, never a key); empty writes a comment only.
	Env []byte
	// SlackEnv is the agent's own Slack settings (workspace, boundary channels), kept in its
	// folder and never built into its image; empty means the agent has no Slack.
	SlackEnv []byte
	// Services are the placeholder services the agent was given keys for: only these are wired.
	Services []string
	// Avatar is the agent's profile picture and its extension (png or jpg); empty means none.
	Avatar    []byte
	AvatarExt string
}

// The build frontends every published agent's mixin uses, as the kits do.
const (
	kitSyntax        = "docker/sandbox-kit:3"
	dockerfileSyntax = "docker/dockerfile:1.7"
	busybox          = "docker.io/library/busybox:1.37"
)

// Write creates data/agents/NAME for a new agent and returns it; it refuses a folder already
// there. The folder is written aside and renamed into place, so a failure leaves none.
func Write(data string, a Agent) (string, error) {
	folder := filepath.Join(data, "agents", a.Name)
	if _, err := os.Lstat(folder); err == nil {
		return "", fmt.Errorf("%s already exists", folder)
	}
	if err := os.MkdirAll(filepath.Dir(folder), 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(folder), "."+a.Name+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	var env strings.Builder
	fmt.Fprintf(&env, "schemaVersion: \"1\"\nname: %s\nagent: %s\n\nkits:\n  - %q\n  - \".\"\n\n", a.Name, a.Image.Type, a.Image.Reference)
	env.WriteString("env:\n  HERMES_WRITE_SAFE_ROOT: \"/home/agent/.hermes:/home/agent/workspace\"\n\n")
	fmt.Fprintf(&env, "sandboxOptions:\n  skills: \"off\"\n  cpus: %d\n  memory: %dg\n", a.Sizes.CPUs, a.Sizes.MemoryGiB)
	if len(a.Natives) > 0 {
		env.WriteString("\nbindings:\n")
		for _, n := range a.Natives {
			fmt.Fprintf(&env, "  %s:\n    apiKey:\n      domains:\n", n.Name)
			for _, domain := range n.Domains {
				fmt.Fprintf(&env, "        - %s\n", domain)
			}
		}
		env.WriteString("\nsecrets:\n")
		for _, n := range a.Natives {
			fmt.Fprintf(&env, "  %s:\n    command: %s\n    refresh: on-demand\n", n.Name, n.Command)
		}
	}
	services := a.Services
	if services == nil {
		services = []string{}
	}
	source, err := json.MarshalIndent(map[string]any{"reference": a.Image.Reference, "type": a.Image.Type,
		"version": a.Image.Version, "created_at": a.Created.UTC().Format(time.RFC3339), "display_name": a.DisplayName,
		"services": services}, "", "  ")
	if err != nil {
		return "", err
	}
	files := map[string]string{
		"sbxenv.yaml": env.String(),
		a.Name + ".yaml": fmt.Sprintf("# syntax=%s\n# %s on the published %s kit, written by troupe: only who the agent is.\n"+
			"schemaVersion: \"3\"\nkind: mixin\ndisplayName: %s\ndescription: \"%s's identity on the %s kit\"\n"+
			"version: \"1.0.0\"\nprovides: [\"%s@1.0.0\"]\nrequires: [\"%s\"]\n",
			kitSyntax, a.Name, a.Image.Type, a.Name, a.Name, a.Image.Type, a.Name, a.Image.Type),
		a.Name + ".dockerfile": fmt.Sprintf("# syntax=%s\n\n# The agent's seeds, owned by the agent and private to it, on an empty image.\n"+
			"FROM %s AS build\nCOPY files/home/ /out/home/agent/\n"+
			"RUN chown -R 1000:1000 /out/home/agent \\\n && find /out/home/agent/.hermes -type d -exec chmod 700 {} + \\\n"+
			" && find /out/home/agent/.hermes -type f -exec chmod 600 {} +\n\nFROM scratch\nCOPY --from=build /out/ /\n",
			dockerfileSyntax, busybox),
		".dockerignore":           ".dockerignore\nsbxenv.yaml\nstorage.env\nsource.json\nslack.env\n",
		"storage.env":             fmt.Sprintf("DOCKER_SANDBOXES_ROOT_SIZE=%dg\n", a.Sizes.RootGiB),
		"source.json":             string(source) + "\n",
		"files/home/.hermes/.env": "# The agent's settings, written by troupe. Keys are never here: sandboxd injects them.\n",
	}
	if a.Sizes.DockerGiB > 0 {
		files["storage.env"] += fmt.Sprintf("DOCKER_SANDBOXES_DOCKER_SIZE=%dg\n", a.Sizes.DockerGiB)
	}
	if len(a.Soul) > 0 {
		files["files/home/.hermes/SOUL.md"] = string(a.Soul)
	}
	if len(a.Env) > 0 {
		files["files/home/.hermes/.env"] = string(a.Env)
	}
	if len(a.SlackEnv) > 0 {
		files["slack.env"] = string(a.SlackEnv)
	}
	if len(a.Avatar) > 0 {
		if a.AvatarExt != "png" && a.AvatarExt != "jpg" {
			return "", fmt.Errorf("an avatar must be png or jpg, not %q", a.AvatarExt)
		}
		files["files/home/.hermes/assets/profile-picture."+a.AvatarExt] = string(a.Avatar)
	}
	for name, text := range files {
		target := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, []byte(text), 0o600); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(staging, folder); err != nil {
		return "", err
	}
	return folder, nil
}

// HasRegistrySecret reports whether sbx holds a registry credential for host (host only): sbx
// pulls a kit's image with it, never with Docker's login.
func HasRegistrySecret(ctx context.Context, runner sbx.Runner, host string) (bool, error) {
	out, err := runner.Output(ctx, sbx.Call{Args: []string{"secret", "ls", "--json"}, Timeout: time.Minute, Private: true})
	if err != nil {
		return false, err
	}
	var listing struct {
		Secrets []struct{ Type, Name string } `json:"secrets"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return false, errors.New("unreadable sbx secret listing")
	}
	for _, s := range listing.Secrets {
		if s.Type == "registry" && s.Name == host {
			return true, nil
		}
	}
	return false, nil
}

// StoreToken gives one registry token to the readers of the kits that need it: Docker (troupe
// reads the kit's files through it) and sbx (it pulls the VM image only with its own
// credential). The token travels on stdin only.
func StoreToken(ctx context.Context, d Docker, runner sbx.Runner, host, user string, token []byte, docker, sandboxes bool) error {
	if docker {
		login, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(login, "docker", "login", host, "--username", user, "--password-stdin")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(token), d.Log, d.Log
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		if err := cmd.Run(); err != nil {
			return errors.New("docker_login_failed")
		}
	}
	if !sandboxes {
		return nil
	}
	if _, err := runner.Output(ctx, sbx.Call{Args: []string{"secret", "set", "--registry", host, "--username", user, "--password-stdin"},
		Stdin: bytes.NewReader(token), Timeout: time.Minute, Private: true}); err != nil {
		return errors.New("sbx_registry_secret_failed")
	}
	return nil
}

// Resize rewrites a published agent's sizes in the files troupe wrote for it: cpus and memory
// in its sbxenv.yaml, the root disk in its storage.env. Each line must appear exactly once, as
// Write writes it; anything else is refused and nothing changes. restore puts both files back.
func Resize(folder string, cpus, memoryGiB, rootGiB int) (restore func(), err error) {
	edits := []struct {
		file    string
		pattern *regexp.Regexp
		line    string
	}{
		{"sbxenv.yaml", regexp.MustCompile(`(?m)^  cpus: [1-9][0-9]*$`), fmt.Sprintf("  cpus: %d", cpus)},
		{"sbxenv.yaml", regexp.MustCompile(`(?m)^  memory: [1-9][0-9]*g$`), fmt.Sprintf("  memory: %dg", memoryGiB)},
		{"storage.env", regexp.MustCompile(`(?m)^DOCKER_SANDBOXES_ROOT_SIZE=[1-9][0-9]*g$`), fmt.Sprintf("DOCKER_SANDBOXES_ROOT_SIZE=%dg", rootGiB)},
	}
	texts, originals := map[string]string{}, map[string]string{}
	for _, e := range edits {
		text, ok := texts[e.file]
		if !ok {
			data, err := os.ReadFile(filepath.Join(folder, e.file))
			if err != nil {
				return nil, err
			}
			text, originals[e.file] = string(data), string(data)
		}
		if n := len(e.pattern.FindAllStringIndex(text, -1)); n != 1 {
			return nil, fmt.Errorf("%s: expected one %q line, found %d", filepath.Join(folder, e.file), e.line, n)
		}
		texts[e.file] = e.pattern.ReplaceAllLiteralString(text, e.line)
	}
	restore = func() {
		for name, text := range originals {
			replace(filepath.Join(folder, name), text)
		}
	}
	for name, text := range texts {
		if err := replace(filepath.Join(folder, name), text); err != nil {
			restore()
			return nil, err
		}
	}
	return restore, nil
}

// Compare orders two MAJOR.MINOR.PATCH versions: negative when a is older, zero when equal,
// positive when newer.
func Compare(a, b string) (int, error) {
	pa, pb := semver.FindStringSubmatch(a), semver.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return 0, fmt.Errorf("not MAJOR.MINOR.PATCH versions: %q, %q", a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y, nil
		}
	}
	return 0, nil
}

// Major is a MAJOR.MINOR.PATCH version's MAJOR; "" for anything else.
func Major(version string) string {
	if m := semver.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return ""
}

// Repin points a published agent's folder at another version of its kit: the one kits line
// of its sbxenv.yaml, as Write writes it, and source.json's reference and version, keeping
// every other field. Anything else is refused and nothing changes. restore puts both back.
func Repin(folder, oldReference string, img Image) (restore func(), err error) {
	envPath, sourcePath := filepath.Join(folder, "sbxenv.yaml"), filepath.Join(folder, "source.json")
	envData, err := os.ReadFile(envPath)
	if err != nil {
		return nil, err
	}
	sourceData, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, err
	}
	line := regexp.MustCompile(`(?m)^  - ` + regexp.QuoteMeta(strconv.Quote(oldReference)) + `$`)
	if n := len(line.FindAllIndex(envData, -1)); n != 1 {
		return nil, fmt.Errorf("%s: expected one kits line for %s, found %d", envPath, oldReference, n)
	}
	newEnv := line.ReplaceAllLiteral(envData, []byte("  - "+strconv.Quote(img.Reference)))
	var fields map[string]any
	if err := json.Unmarshal(sourceData, &fields); err != nil {
		return nil, fmt.Errorf("%s: malformed", sourcePath)
	}
	if fields["reference"] != oldReference {
		return nil, fmt.Errorf("%s: names %v, not %s", sourcePath, fields["reference"], oldReference)
	}
	fields["reference"], fields["version"] = img.Reference, img.Version
	newSource, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, err
	}
	if bytes.HasSuffix(sourceData, []byte("\n")) {
		newSource = append(newSource, '\n')
	}
	restore = func() {
		replace(envPath, string(envData))
		replace(sourcePath, string(sourceData))
	}
	if err := replace(envPath, string(newEnv)); err != nil {
		return nil, err
	}
	if err := replace(sourcePath, string(newSource)); err != nil {
		restore()
		return nil, err
	}
	return restore, nil
}

// replace writes path's new text beside it and renames it into place.
func replace(path, text string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	if _, err := temporary.WriteString(text); err != nil {
		temporary.Close()
		os.Remove(temporary.Name())
		return err
	}
	temporary.Close()
	if err := os.Rename(temporary.Name(), path); err != nil {
		os.Remove(temporary.Name())
		return err
	}
	return nil
}
