package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/registry"
)

// Kit is everything a recreate reads from an agent's kits, read and checked once.
type Kit struct {
	Agent        string
	Base, Folder string
	// Commit is the Hermes commit the base kit's dockerfile pins (ARG HERMES_GIT_SHA); CPUs and
	// MemoryGiB the agent's declared VM size.
	Commit       string
	CPUs         int
	MemoryGiB    int
	RootGiB      int
	DockerGiB    int
	StorageEnv   []string // DOCKER_SANDBOXES_*_SIZE for sbx env create
	Seeds        []guest.File
	Manifest     []byte // {"sha256": {path: hash}} of every composed seed
	ConfigSHA    string
	Declarations string // one hash of the files the VM is created from
	Slack        *SlackKit
	SeedCheck    *guest.File // the base kit's tools/seed-check.py, sent as kit/seed-check.py
	Setup        *guest.File // the base kit's tools/setup.py
	// Reference is a published base kit's pinned reference (REGISTRY/TYPE:VERSION); empty
	// for a local base kit folder.
	Reference    string
	sbxenv, size string
	// The v3 kit files sbx builds the VM's image from: each kit's descriptor, dockerfile and
	// .dockerignore.
	baseDescriptor, baseDockerfile, baseIgnore string
	descriptor, dockerfile, ignore             string
}

// SlackKit is the base kit's Slack workspace (slack.env) and boundary channels. The agent's
// manager, allowlist and home are in its .env, live state the manager changes by telling it.
type SlackKit struct {
	Team, Public, Private string
	Scopes                []string
	// File is the slack.env these came from. A published agent keeps its own in its folder,
	// where its workspace is recorded at its first Slack setup.
	File string
}

// seedLimit bounds what one recreate sends into the VM.
const seedLimit = 64 << 20

var (
	slackID      = regexp.MustCompile(`\b[TUWCD]0[A-Z0-9]{8,}\b`)
	teamID       = regexp.MustCompile(`^T[A-Z0-9]+$`)
	channelID    = regexp.MustCompile(`^C[A-Z0-9]+$`)
	memberID     = regexp.MustCompile(`^[UW][A-Z0-9]+$`)
	gibSize      = regexp.MustCompile(`^([1-9][0-9]*)g$`)
	envLine      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*=`)
	plainSetting = regexp.MustCompile(`^\s*([a-z]+):\s*"?([^"#\s]+)"?\s*(#.*)?$`)
	hermesSHA    = regexp.MustCompile(`^ARG HERMES_GIT_SHA=([0-9a-f]{40})$`)
	commitSHA    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ReadKit composes the agent's seeds and runs the generic checks every kit must pass: no path
// from two kits, a well-formed .env and, for Slack kits, well-formed Slack IDs, a manager on the
// allowlist, the home not a boundary channel and no Slack ID in SOUL.md or the skills (Slack IDs live in
// config only; the model never needs one). Kit-specific rules run in a VM (tools/seed-check.py).
func ReadKit(reg registry.Registry, agent string) (*Kit, error) {
	kits, err := reg.Kits(agent)
	if err != nil {
		return nil, err
	}
	k := &Kit{Agent: agent, Base: kits[0], Folder: kits[len(kits)-1]}
	k.sbxenv = filepath.Join(k.Folder, "sbxenv.yaml")
	k.size = filepath.Join(k.Folder, "storage.env")
	k.baseDescriptor = registry.Descriptor(k.Base)
	k.baseDockerfile = filepath.Join(k.Base, filepath.Base(k.Base)+".dockerfile")
	k.baseIgnore = filepath.Join(k.Base, ".dockerignore")
	k.descriptor = registry.Descriptor(k.Folder)
	k.dockerfile = filepath.Join(k.Folder, filepath.Base(k.Folder)+".dockerfile")
	k.ignore = filepath.Join(k.Folder, ".dockerignore")
	if published, err := registry.ReadPublished(k.Base); err == nil {
		// A published base: troupe's record of it stands in for its recipe files, which the
		// image does not carry, and names the commit the image's revision label gave.
		if !commitSHA.MatchString(published.Commit) {
			return nil, fmt.Errorf("%s: no Hermes commit", filepath.Join(k.Base, registry.PublishedFile))
		}
		k.Commit, k.Reference = published.Commit, published.Reference
		k.baseDescriptor = filepath.Join(k.Base, registry.PublishedFile)
		k.baseDockerfile, k.baseIgnore = "", ""
	} else if k.Commit, err = pinnedCommit(k.baseDockerfile); err != nil {
		return nil, err
	}
	cpus, err := setting(k.sbxenv, "cpus")
	if err != nil {
		return nil, err
	}
	if k.CPUs, err = strconv.Atoi(cpus); err != nil || k.CPUs < 1 {
		return nil, fmt.Errorf("%s: cpus must be a whole number", k.sbxenv)
	}
	memory, err := setting(k.sbxenv, "memory")
	if err != nil {
		return nil, err
	}
	if k.MemoryGiB, err = gib(memory); err != nil {
		return nil, fmt.Errorf("%s: memory must be whole gigabytes, such as 12g", k.sbxenv)
	}
	if err := k.readStorage(); err != nil {
		return nil, err
	}
	seeds, err := reg.Compose(agent)
	if err != nil {
		return nil, err
	}
	hashes := map[string]string{}
	total := 0
	for _, seed := range seeds {
		data, err := os.ReadFile(seed.Source)
		if err != nil {
			return nil, err
		}
		if total += len(data); total > seedLimit {
			return nil, errors.New("the composed seeds are larger than 64 MiB")
		}
		sum := sha256.Sum256(data)
		hashes[seed.Path] = hex.EncodeToString(sum[:])
		k.Seeds = append(k.Seeds, guest.File{Name: "seeds/" + seed.Path, Data: data})
	}
	k.ConfigSHA = hashes["config.yaml"]
	if k.Manifest, err = json.Marshal(map[string]any{"sha256": hashes}); err != nil {
		return nil, err
	}
	if err := k.checkSeeds(reg); err != nil {
		return nil, err
	}
	for name, slot := range map[string]**guest.File{"seed-check.py": &k.SeedCheck, "setup.py": &k.Setup} {
		data, err := os.ReadFile(filepath.Join(k.Base, "tools", name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		*slot = &guest.File{Name: "kit/" + name, Data: data}
	}
	k.Declarations, err = k.declarations()
	return k, err
}

// Setting reads one plain key: value line of a kit file (sbxenv.yaml).
func Setting(path, key string) (string, error) { return setting(path, key) }

// StorageEnv reads a storage.env as the DOCKER_SANDBOXES_*_SIZE values sbx env create takes.
func StorageEnv(path string) ([]string, error) {
	k := &Kit{size: path}
	if err := k.readStorage(); err != nil {
		return nil, err
	}
	return k.StorageEnv, nil
}

// setting reads the first `key: value` line of a YAML file, without a YAML library.
func setting(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m := plainSetting.FindStringSubmatch(line); m != nil && m[1] == key {
			return m[2], nil
		}
	}
	return "", fmt.Errorf("%s: no %s", path, key)
}

// pinnedCommit reads the Hermes commit a workload dockerfile pins: exactly one line
// ARG HERMES_GIT_SHA=<40 lowercase hex>. A bare ARG HERMES_GIT_SHA only brings that global
// value into a build stage (Dockerfile scoping), so it pins nothing and is skipped.
func pinnedCommit(dockerfile string) (string, error) {
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		return "", err
	}
	commit := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ARG HERMES_GIT_SHA") || line == "ARG HERMES_GIT_SHA" {
			continue
		}
		m := hermesSHA.FindStringSubmatch(line)
		if m == nil || commit != "" {
			commit = ""
			break
		}
		commit = m[1]
	}
	if commit == "" {
		return "", fmt.Errorf("%s: pin Hermes with exactly one line ARG HERMES_GIT_SHA=<40 hex commit>", dockerfile)
	}
	return commit, nil
}

func gib(text string) (int, error) {
	m := gibSize.FindStringSubmatch(text)
	if m == nil {
		return 0, errors.New("not whole gigabytes")
	}
	return strconv.Atoi(m[1])
}

// readStorage reads storage.env: the root disk size, and the Docker disk size when the VM
// runs Docker. sbx gets these values only (internal/sbx drops any inherited ones).
func (k *Kit) readStorage() error {
	values, err := envFile(k.size)
	if err != nil {
		return err
	}
	for key := range values {
		if key != "DOCKER_SANDBOXES_ROOT_SIZE" && key != "DOCKER_SANDBOXES_DOCKER_SIZE" {
			return fmt.Errorf("%s: unexpected setting %s", k.size, key)
		}
	}
	if k.RootGiB, err = gib(values["DOCKER_SANDBOXES_ROOT_SIZE"]); err != nil {
		return fmt.Errorf("%s: DOCKER_SANDBOXES_ROOT_SIZE must be whole gigabytes, such as 200g", k.size)
	}
	k.StorageEnv = []string{"DOCKER_SANDBOXES_ROOT_SIZE=" + values["DOCKER_SANDBOXES_ROOT_SIZE"]}
	if docker, ok := values["DOCKER_SANDBOXES_DOCKER_SIZE"]; ok {
		if k.DockerGiB, err = gib(docker); err != nil {
			return fmt.Errorf("%s: DOCKER_SANDBOXES_DOCKER_SIZE must be whole gigabytes, such as 50g", k.size)
		}
		k.StorageEnv = append(k.StorageEnv, "DOCKER_SANDBOXES_DOCKER_SIZE="+docker)
	}
	return nil
}

// envFile reads KEY=VALUE lines, skipping blanks and comments; a malformed line or a repeated
// key is an error.
func envFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !envLine.MatchString(line) {
			return nil, fmt.Errorf("%s: malformed line", path)
		}
		key, value, _ := strings.Cut(line, "=")
		if _, seen := values[key]; seen {
			return nil, fmt.Errorf("%s: %s is set twice", path, key)
		}
		values[key] = value
	}
	return values, nil
}

func (k *Kit) seed(path string) []byte {
	for _, f := range k.Seeds {
		if f.Name == "seeds/"+path {
			return f.Data
		}
	}
	return nil
}

func (k *Kit) checkSeeds(reg registry.Registry) error {
	envPath := filepath.Join(k.Folder, "files", "home", ".hermes", ".env")
	env, err := envFile(envPath)
	if err != nil {
		return fmt.Errorf("the agent's .env: %w", err)
	}
	// A local agent's workspace and boundary channels are its base kit's. A published agent's
	// workspace is its own, empty until its first Slack setup; its boundary channels are this
	// machine's settings, if any.
	published := k.Reference != ""
	file := filepath.Join(k.Base, "slack.env")
	if published {
		file = filepath.Join(k.Folder, "slack.env")
	}
	workspace, err := envFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s := &SlackKit{Team: workspace["SLACK_WORKSPACE"],
		Public: workspace["SLACK_BOUNDARY_PUBLIC"], Private: workspace["SLACK_BOUNDARY_PRIVATE"], File: file}
	if published {
		// The boundary channels are this machine's settings as they are now, so one set after the
		// agent was created still applies to it.
		s.Public, s.Private = reg.Boundary[0], reg.Boundary[1]
	}
	manager, home := env["SLACK_MANAGER"], env["SLACK_HOME_CHANNEL"]
	optional := func(value string, valid *regexp.Regexp) bool {
		return value == "" && published || valid.MatchString(value)
	}
	switch {
	case !optional(s.Team, teamID):
		return errors.New("slack.env: SLACK_WORKSPACE must be a workspace ID (T...)")
	case !optional(s.Public, channelID) || !optional(s.Private, channelID):
		return errors.New("slack.env: the boundary channels must be channel IDs (C...)")
	case !memberID.MatchString(manager):
		return errors.New("the agent's .env: SLACK_MANAGER must be a member ID (U... or W...)")
	case home != manager && !channelID.MatchString(home):
		// The manager's member ID is their DM with the agent.
		return errors.New("the agent's .env: SLACK_HOME_CHANNEL must be a channel ID (C...) or the manager's member ID")
	case home == s.Public || home == s.Private:
		return errors.New("the agent's .env: the home channel cannot be a boundary channel")
	}
	allowed := strings.Split(env["SLACK_ALLOWED_USERS"], ",")
	for _, member := range allowed {
		if !memberID.MatchString(member) {
			return errors.New("the agent's .env: SLACK_ALLOWED_USERS must list member IDs (U... or W...)")
		}
	}
	if !slices.Contains(allowed, manager) {
		return errors.New("the agent's .env: SLACK_ALLOWED_USERS must include the manager")
	}
	for _, f := range k.Seeds {
		path := strings.TrimPrefix(f.Name, "seeds/")
		if (path == "SOUL.md" || strings.HasPrefix(path, "skills/")) && slackID.Match(f.Data) {
			return fmt.Errorf("a Slack ID in %s: Slack IDs belong in the .env and config only", path)
		}
	}
	data, err := os.ReadFile(filepath.Join(k.Base, "slack-app-manifest.json"))
	if err != nil {
		return err
	}
	var manifest struct {
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.OAuth.Scopes.Bot) == 0 {
		return errors.New("slack-app-manifest.json: no bot scopes")
	}
	s.Scopes = append([]string(nil), manifest.OAuth.Scopes.Bot...)
	sort.Strings(s.Scopes)
	k.Slack = s
	return nil
}

// declarations hashes the files the VM itself is created from (the environment, both kits'
// descriptors, dockerfiles and .dockerignore files, and the disk sizes; for a published base,
// troupe's record of the pinned kit), so a resumed recreate can refuse when they changed.
func (k *Kit) declarations() (string, error) {
	sum := sha256.New()
	for _, path := range []string{k.sbxenv, k.baseDescriptor, k.baseDockerfile, k.baseIgnore, k.descriptor, k.dockerfile, k.ignore, k.size} {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(sum, "%s %d\n", filepath.Base(path), len(data))
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ManifestSHA is the seed manifest's hash, recorded at preflight.
func (k *Kit) ManifestSHA() string {
	sum := sha256.Sum256(k.Manifest)
	return hex.EncodeToString(sum[:])
}

// slackRequest is the Slack guest job's request.
func (k *Kit) slackRequest(tokens map[string]string) []byte {
	// Only the boundary channels that are set: an empty one would pass the check vacuously.
	boundary := map[string]string{}
	if k.Slack.Public != "" {
		boundary["public"] = k.Slack.Public
	}
	if k.Slack.Private != "" {
		boundary["private"] = k.Slack.Private
	}
	data, _ := json.Marshal(map[string]any{
		"home":     map[string]string{"scope_id": k.Slack.Team},
		"scopes":   k.Slack.Scopes,
		"boundary": boundary,
		"tokens":   tokens,
	})
	return data
}

// recordWorkspace notes the Slack workspace a published agent's first Slack setup found, in its
// own slack.env, so every later setup, probe and continuity check holds the agent to it.
func (k *Kit) recordWorkspace(team string) error {
	if !teamID.MatchString(team) {
		return errors.New("slack_workspace_unreadable")
	}
	data, err := os.ReadFile(k.Slack.File)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, "SLACK_WORKSPACE=") {
			lines[i], found = "SLACK_WORKSPACE="+team, true
		}
	}
	if !found {
		lines = append(lines, "SLACK_WORKSPACE="+team)
	}
	temporary, err := os.CreateTemp(filepath.Dir(k.Slack.File), ".slack.env.")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.WriteString(strings.Join(lines, "\n") + "\n")
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), k.Slack.File); err != nil {
		return err
	}
	k.Slack.Team = team
	return nil
}

// kitFiles is what a kit tool call sends: the tool and the composed seeds.
func (k *Kit) kitFiles(tool *guest.File) []guest.File {
	return append([]guest.File{*tool}, k.Seeds...)
}
