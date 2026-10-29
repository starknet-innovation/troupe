// Package secret keeps each agent's API keys on the host, as strongly as the machine allows
// (see Tier), and gives the agent's sandbox access to them. A key is never shown, printed or
// logged: sandboxd reads it on demand and its proxy injects it, so the agent itself only ever
// sees a placeholder.
package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/sudo"
)

// Service is one secret an agent uses.
type Service struct {
	Name string
	// Placeholder services are injected by sandboxd as an environment value the guest sends
	// in place of the key (Env, Hosts, Value); native services are the sbxenv.yaml secrets.
	Placeholder bool
	Env         string
	Hosts       []string
	Value       string
}

// placeholderFile is the local base kit file that declares an agent's placeholder services;
// a published kit declares every service in ServicesFile instead.
const (
	placeholderFile = "secrets.json"
	ServicesFile    = "services.json"
)

// KitService is one service a published kit's services.json declares: how create asks for it
// and how sbx delivers it (native, through the sbxenv's secrets and bindings, with its domains;
// or a placeholder sandboxd swaps for the key on its hosts).
type KitService struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Help        string   `json:"help"` // printed before the prompt: what the key is and where to get it
	Required    bool     `json:"required"`
	Slack       bool     `json:"slack"`
	Delivery    string   `json:"delivery"`
	Domains     []string `json:"domains"`
	Env         string   `json:"env"`
	Hosts       []string `json:"hosts"`
	Placeholder string   `json:"placeholder"`
}

// domainName is a host name a binding or a placeholder may name.
var domainName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// KitServices reads and checks a published kit's services.json, in the order create asks.
func KitServices(base string) ([]KitService, error) {
	path := filepath.Join(base, ServicesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var declared struct {
		Services []KitService `json:"services"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declared); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, s := range declared.Services {
		bad := !serviceName.MatchString(s.Name) || seen[s.Name] || strings.TrimSpace(s.Prompt) == "" || len(s.Prompt) > 200 ||
			utf8.RuneCountInString(s.Help) > 500 || strings.IndexFunc(s.Prompt+s.Help, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0
		switch s.Delivery {
		case "native":
			bad = bad || len(s.Domains) == 0 || s.Env != "" || len(s.Hosts) != 0 || s.Placeholder != ""
			for _, d := range s.Domains {
				bad = bad || !domainName.MatchString(d)
			}
		case "placeholder":
			bad = bad || len(s.Domains) != 0 || !envName.MatchString(s.Env) || len(s.Hosts) == 0 || !strings.Contains(s.Placeholder, "{agent}")
			for _, h := range s.Hosts {
				bad = bad || !domainName.MatchString(h)
			}
		default:
			bad = true
		}
		if bad {
			return nil, fmt.Errorf("%s: service %q is malformed", path, s.Name)
		}
		seen[s.Name] = true
	}
	if len(declared.Services) == 0 {
		return nil, fmt.Errorf("%s: no services", path)
	}
	return declared.Services, nil
}

// A service name becomes a file name and a credential name; an env name, an environment
// variable.
var (
	serviceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	envName     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
)

// Services lists an agent's services: native ones from its sbxenv.yaml secrets, then the
// placeholder ones its base kit declares (secrets.json, or a published kit's services.json).
func Services(agent string, native []string, base string) ([]Service, error) {
	var services []Service
	for _, name := range native {
		if !serviceName.MatchString(name) {
			return nil, fmt.Errorf("%s: service %q is not a lowercase name", agent, name)
		}
		services = append(services, Service{Name: name})
	}
	if _, err := os.Stat(filepath.Join(base, ServicesFile)); err == nil {
		declared, err := KitServices(base)
		if err != nil {
			return nil, err
		}
		for _, s := range declared {
			if s.Delivery == "placeholder" {
				services = append(services, Service{Name: s.Name, Placeholder: true, Env: s.Env, Hosts: s.Hosts,
					Value: strings.ReplaceAll(s.Placeholder, "{agent}", agent)})
			}
		}
		return services, nil
	}
	data, err := os.ReadFile(filepath.Join(base, placeholderFile))
	if errors.Is(err, os.ErrNotExist) {
		return services, nil
	}
	if err != nil {
		return nil, err
	}
	var declared struct {
		Placeholders []struct {
			Service     string   `json:"service"`
			Env         string   `json:"env"`
			Hosts       []string `json:"hosts"`
			Placeholder string   `json:"placeholder"`
		} `json:"placeholders"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declared); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(base, placeholderFile), err)
	}
	for _, p := range declared.Placeholders {
		if !serviceName.MatchString(p.Service) || !envName.MatchString(p.Env) || len(p.Hosts) == 0 || !strings.Contains(p.Placeholder, "{agent}") {
			return nil, fmt.Errorf("%s: %q needs service, env, hosts and a placeholder naming {agent}", placeholderFile, p.Service)
		}
		services = append(services, Service{Name: p.Service, Placeholder: true, Env: p.Env, Hosts: p.Hosts,
			Value: strings.ReplaceAll(p.Placeholder, "{agent}", agent)})
	}
	return services, nil
}

// Tier is how an agent's keys are kept on this host, strongest first. create picks the
// strongest the machine offers and asks before anything weaker than TPM.
type Tier string

const (
	TPM  Tier = "tpm"  // systemd-creds, sealed by the TPM and the host key
	Host Tier = "host" // systemd-creds, the host key only: a copy of the disk can read them
	File Tier = "file" // a plain file only the host user can read
)

// Describe is what an agent's keys get at this tier, for a line people read.
func (t Tier) Describe() string {
	switch t {
	case TPM:
		return "keys are sealed by this machine's TPM"
	case Host:
		return "no usable TPM: keys are encrypted with the host key only, so anyone with a copy of this disk can read them"
	}
	return "no systemd-creds from systemd 256 or newer: keys are plain files only your user can read, so anyone with a copy of this disk can read them"
}

// Upgrade is what this machine needs for TPM sealing, from this tier.
func (t Tier) Upgrade() string {
	if t == File {
		return "systemd 256 or newer with systemd-creds, and a TPM 2.0"
	}
	return "a TPM 2.0: enable it in the firmware, or on EC2 use an AMI registered with NitroTPM"
}

var firstNumber = regexp.MustCompile(`[0-9]+`)

// Detect is the strongest tier this machine offers. run is a program's standard output.
func Detect(run func(name string, args ...string) (string, error)) Tier {
	out, err := run("systemctl", "--version")
	version, _ := strconv.Atoi(firstNumber.FindString(out))
	// sandboxd runs as the host user, so it decrypts with systemd-creds --user, from systemd 256.
	if err != nil || version < 256 {
		return File
	}
	if _, err := run("systemd-creds", "--version"); err != nil {
		return File
	}
	// has-tpm2 moved from systemd-creds to systemd-analyze in 257; 0 means full support
	// (firmware, driver, system, subsystem, libraries).
	for _, tool := range []string{"systemd-analyze", "systemd-creds"} {
		if _, err := run(tool, "has-tpm2", "--quiet"); err == nil {
			return TPM
		}
	}
	return Host
}

// DetectHere is Detect on this machine, each program with a deadline.
func DetectHere() Tier {
	return Detect(func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).Output()
		return string(out), err
	})
}

// tierFile records an agent's tier in its keys folder. Agents created before tiers have none:
// theirs is TPM.
const tierFile = "tier"

// SetTier records the tier create chose, in the agent's keys folder (which create made).
func SetTier(root, agent string, t Tier) error {
	return os.WriteFile(filepath.Join(root, agent, tierFile), []byte(string(t)+"\n"), 0o600)
}

// Store is an agent's secrets on this host.
type Store struct {
	// Agent is the sandbox the secrets are wired to.
	Agent    string
	Services []Service
	// Root holds one folder per agent: ~/.local/state/credentials.
	Root   string
	Runner sbx.Runner
	// ReadSecret reads the secret from the operator; tests replace it.
	ReadSecret func(prompt string) ([]byte, error)
	// Unborn says the agent's sandbox cannot exist yet (a create that claimed the name): Set
	// stores the key without asking sandboxd, and the creation wires it.
	Unborn bool
}

// DefaultRoot is where secrets live.
func DefaultRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "credentials")
}

func (s *Store) service(name string) (Service, error) {
	var names []string
	for _, svc := range s.Services {
		if svc.Name == name {
			return svc, nil
		}
		names = append(names, svc.Name)
	}
	sort.Strings(names)
	return Service{}, fmt.Errorf("%s has no service %q; its services: %s", s.Agent, name, strings.Join(names, ", "))
}

// Tier is the agent's tier, as its creation recorded it.
func (s *Store) Tier() (Tier, error) {
	path := filepath.Join(s.Root, s.Agent, tierFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return TPM, nil
	}
	if err != nil || !info.Mode().IsRegular() || owner(info) != os.Getuid() || info.Size() > 16 {
		return "", Failure{"unknown_key_tier"}
	}
	data, _ := os.ReadFile(path)
	switch t := Tier(strings.TrimSpace(string(data))); t {
	case TPM, Host, File:
		return t, nil
	}
	return "", Failure{"unknown_key_tier"}
}

// tier is the agent's tier where a failure has already been reported (Set and Check read it
// first): TPM when unreadable.
func (s *Store) tier() Tier {
	if t, err := s.Tier(); err == nil {
		return t
	}
	return TPM
}

// File is where a service's key lives; Name is its systemd credential name.
func (s *Store) File(svc Service) string {
	if s.tier() == File {
		return filepath.Join(s.Root, s.Agent, svc.Name+".key")
	}
	return filepath.Join(s.Root, s.Agent, svc.Name+".cred")
}
func (s *Store) Name(svc Service) string { return s.Agent + "-" + svc.Name }

// Command is how sandboxd reads the key on demand, as the host user.
func (s *Store) Command(svc Service) string {
	if s.tier() == File {
		return "cat " + s.File(svc)
	}
	return fmt.Sprintf("systemd-creds --user --no-ask-password --name=%s decrypt %s -", s.Name(svc), s.File(svc))
}

// header starts every ciphertext systemd 259 writes for host+tpm2 user-scoped credentials
// (AES-256-GCM, HOST_AND_TPM2_HMAC_SCOPED, no signed PCR policy); hostHeader, for host-only
// user-scoped ones (HOST_SCOPED), both from systemd's creds-util.h. Each tier accepts only its
// own, so a weaker ciphertext never stands in for a TPM-sealed one.
var (
	header, _     = hex.DecodeString("ef4ac13679a9480ea7db68897f9f165d")
	hostHeader, _ = hex.DecodeString("55b9ed1d38594d43a8319d2ebb332ac6")
)

// checkFile accepts only a private regular file in private folders, holding the tier's format:
// a ciphertext with its header, or for File any non-empty content, read only by its owner. It
// reads a ciphertext's format; a plain key it only opens, never reading the secret.
func checkFile(path string, tier Tier) error {
	uid := os.Getuid()
	for _, dir := range []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path)} {
		info, err := os.Lstat(dir)
		if err != nil {
			return errors.New("missing")
		}
		if !info.IsDir() || owner(info) != uid || info.Mode().Perm() != 0o700 {
			return errors.New("unsafe_folder")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("missing")
	}
	mode := os.FileMode(0o600)
	if tier == File {
		mode = 0o400
	}
	if !info.Mode().IsRegular() || owner(info) != uid || info.Mode().Perm() != mode || info.Size() == 0 || info.Size() >= 16384 {
		return errors.New("unsafe_file")
	}
	if tier == File {
		file, err := os.Open(path)
		if err != nil {
			return errors.New("unreadable")
		}
		file.Close()
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("unreadable")
	}
	want := header
	if tier == Host {
		want = hostHeader
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(data)), ""))
	if err != nil || len(raw) <= 100 || !bytes.Equal(raw[:16], want) {
		return errors.New("wrong_format")
	}
	return nil
}

func owner(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// decrypts proves the key reads back as sandboxd will read it, discarding the plaintext: a
// ciphertext opens with this machine's keys; a plain file was already read by checkFile.
func (s *Store) decrypts(svc Service, path string) bool {
	if s.tier() == File {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-creds", "--user", "--no-ask-password", "--name="+s.Name(svc), "decrypt", path, "-")
	return cmd.Run() == nil // stdout is discarded: the secret never reaches a buffer here
}

// Failure is a refusal with a short class; it never carries a secret.
type Failure struct{ Class string }

func (f Failure) Error() string { return f.Class }

// Set asks for a secret, stores it at the agent's tier, replaces the stored one only once the
// new file is proven, and wires it to the agent's sandbox. It reports whether the sandbox
// had no record of this service before (a new service, which reaches a running sandbox only
// after sandboxd restarts).
func (s *Store) Set(ctx context.Context, name string) (wired, newService bool, err error) {
	svc, err := s.service(name)
	if err != nil {
		return false, false, Failure{err.Error()}
	}
	if os.Geteuid() == 0 {
		return false, false, Failure{"run as your own user, not root"}
	}
	tier, err := s.Tier()
	if err != nil {
		return false, false, err
	}
	// Encryption reads the root-only host key; a plain file needs no sudo.
	if tier != File {
		if err := sudo.Ask(ctx, os.Stdin); err != nil {
			return false, false, Failure{"sudo_required"}
		}
	}
	// Refuse redirected folders rather than following them.
	for _, dir := range []string{s.Root, filepath.Join(s.Root, s.Agent)} {
		if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return false, false, Failure{"folder_is_symlink"}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, false, err
		}
		if info, err := os.Lstat(dir); err != nil || owner(info) != os.Getuid() || info.Mode().Perm() != 0o700 {
			return false, false, Failure{"unsafe_folder"}
		}
	}
	path := s.File(svc)
	temporary, err := os.CreateTemp(filepath.Dir(path), ".enroll.")
	if err != nil {
		return false, false, err
	}
	defer os.Remove(temporary.Name())
	account, err := user.Current()
	if err != nil {
		temporary.Close()
		return false, false, err
	}
	value, err := s.ReadSecret(fmt.Sprintf("Enter %s's %s secret (hidden): ", s.Agent, svc.Name))
	if err != nil || ctx.Err() != nil {
		clear(value)
		temporary.Close()
		if errors.Is(err, ErrInterrupted) || ctx.Err() != nil {
			return false, false, ErrInterrupted
		}
		return false, false, Failure{err.Error()}
	}
	if len(bytes.TrimSpace(value)) == 0 {
		temporary.Close()
		return false, false, Failure{"empty_input"}
	}
	var encryptErr error
	switch tier {
	case File:
		_, encryptErr = temporary.Write(value)
		if encryptErr == nil {
			encryptErr = temporary.Chmod(0o400)
		}
	default:
		key := []string{"--with-key=host+tpm2", "--tpm2-pcrs=", "--tpm2-public-key-pcrs="}
		if tier == Host {
			key = []string{"--with-key=host"}
		}
		args := append(append([]string{"-n", "systemd-creds", "--uid=" + account.Username}, key...),
			"--no-ask-password", "--name="+s.Name(svc), "encrypt", "-", "-")
		encrypt := exec.CommandContext(ctx, "sudo", args...)
		encrypt.Stdin, encrypt.Stdout = bytes.NewReader(value), temporary
		encryptErr = encrypt.Run()
	}
	clear(value)
	syncErr := temporary.Sync()
	temporary.Close()
	if encryptErr != nil || syncErr != nil || checkFile(temporary.Name(), tier) != nil || !s.decrypts(svc, temporary.Name()) {
		return false, false, Failure{"encryption_or_verification_failed; the stored secret is unchanged"}
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return false, false, err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return s.wire(ctx, svc)
}

// listing is the sandbox's own secret records, metadata only.
type listing struct {
	Secrets []struct {
		Scope, Type, Name string
	} `json:"secrets"`
	Custom []struct {
		Env, Scope, Placeholder string
	} `json:"custom_secrets"`
}

func (s *Store) records(ctx context.Context) (*listing, error) {
	// Metadata only: never the --service form, which prints part of each value.
	out, err := s.Runner.Output(ctx, sbx.Call{Args: []string{"secret", "ls", "--sandbox", s.Agent, "--json"},
		Timeout: 60 * time.Second, Private: true})
	if err != nil {
		return nil, err
	}
	var l listing
	if err := json.Unmarshal(out, &l); err != nil || l.Secrets == nil && l.Custom == nil {
		return nil, Failure{"sandbox_secret_listing_unreadable"}
	}
	return &l, nil
}

// Record states: a service's records in the sandbox's listing.
const (
	unwired    = "not_wired_to_the_sandbox"
	unexpected = "unexpected_sandbox_record"
)

// state is "" when the sandbox holds exactly one record for the service and it is the
// agent's own; unwired when it holds none; unexpected when another record claims the
// service's name or variable (a global one, another scope, an old placeholder), which this
// tool never writes and refuses to paper over.
func (l *listing) state(agent string, svc Service) string {
	mine, others := 0, 0
	if svc.Placeholder {
		for _, c := range l.Custom {
			switch {
			case c.Env == svc.Env && c.Scope == agent && c.Placeholder == svc.Value:
				mine++
			case c.Env == svc.Env || c.Placeholder == svc.Value:
				others++
			}
		}
	} else {
		for _, r := range l.Secrets {
			switch {
			case r.Name != svc.Name || r.Type != "service":
			case r.Scope == agent:
				mine++
			default:
				others++
			}
		}
	}
	switch {
	case others > 0 || mine > 1:
		return unexpected
	case mine == 0:
		return unwired
	}
	return ""
}

// Wire wires an already stored service to the sandbox, as Set does after storing it: for
// another sandbox sharing the group's secrets.
func (s *Store) Wire(ctx context.Context, name string) (wired, newService bool, err error) {
	svc, err := s.service(name)
	if err != nil {
		return false, false, Failure{err.Error()}
	}
	return s.wire(ctx, svc)
}

// wire stores the decrypt command in the agent's sandbox scope (never global). A sandbox that
// does not exist yet is left alone: its creation reads the same declarations.
func (s *Store) wire(ctx context.Context, svc Service) (wired, newService bool, err error) {
	if s.Unborn {
		return false, false, nil
	}
	inventory, err := s.Runner.Inventory(ctx)
	if err != nil {
		return false, false, Failure{"sandbox_inventory_unreadable; the secret is stored"}
	}
	if _, exists := inventory[s.Agent]; !exists {
		return false, false, nil
	}
	before, err := s.records(ctx)
	if err != nil {
		return false, false, Failure{"sandbox_secret_listing_unreadable; the secret is stored"}
	}
	switch before.state(s.Agent, svc) {
	case unexpected:
		return false, false, Failure{unexpected + "; the secret is stored, not wired"}
	case unwired:
		newService = true
	}
	if err := s.setRecord(ctx, svc); err != nil {
		return false, newService, Failure{"sandbox_wiring_failed; the secret is stored"}
	}
	after, err := s.records(ctx)
	if err != nil || after.state(s.Agent, svc) != "" {
		return false, newService, Failure{"sandbox_wiring_unconfirmed; the secret is stored"}
	}
	return true, newService, nil
}

// setRecord writes the service's record in the agent's sandbox scope.
func (s *Store) setRecord(ctx context.Context, svc Service) error {
	// --force: rewiring overwrites the agent's own record. set-custom overwrites without asking
	// and has no --force (measured on sbx 0.48.0).
	args := []string{"secret", "set", svc.Name, "--sandbox", s.Agent, "--command", s.Command(svc), "--refresh", "on-demand", "--force"}
	if svc.Placeholder {
		args = []string{"secret", "set-custom", "--sandbox", s.Agent, "--env", svc.Env, "--placeholder", svc.Value}
		for _, host := range svc.Hosts {
			args = append(args, "--host", host)
		}
		args = append(args, "--command", s.Command(svc), "--refresh", "on-demand")
	}
	_, err := s.Runner.Output(ctx, sbx.Call{Args: args, Timeout: 60 * time.Second})
	return err
}

// Result is one service's check.
type Result struct {
	Service string
	Problem string // empty when stored, readable and, when the sandbox exists, wired
	Wired   bool   // false with no Problem: the sandbox does not exist yet
}

// Check verifies every service: a private key file in the agent's tier's format that reads
// back, then, when the
// agent's sandbox exists, exactly one scoped record for it. The host checks run even when
// sandboxd cannot be reached. It never reads a secret's value.
func (s *Store) Check(ctx context.Context) ([]Result, error) {
	var l *listing
	sandbox := ""
	if inventory, err := s.Runner.Inventory(ctx); err != nil {
		sandbox = "sandboxd_unreachable_wiring_unchecked"
	} else if _, exists := inventory[s.Agent]; exists {
		if l, err = s.records(ctx); err != nil {
			sandbox = "sandbox_secret_listing_unreadable"
		}
	}
	tier, tierErr := s.Tier()
	var results []Result
	for _, svc := range s.Services {
		r := Result{Service: svc.Name}
		path := s.File(svc)
		switch {
		case tierErr != nil:
			r.Problem = tierErr.Error()
		case checkFile(path, tier) != nil:
			r.Problem = checkFile(path, tier).Error()
		case !s.decrypts(svc, path):
			r.Problem = "does_not_decrypt"
		case sandbox != "":
			r.Problem = sandbox
		case l != nil:
			r.Problem = l.state(s.Agent, svc)
			r.Wired = r.Problem == ""
		}
		results = append(results, r)
	}
	return results, nil
}

// Sources checks every service's host side only: a private key file in the agent's tier's
// format that reads back. It needs no sandboxd, and names the first service that fails.
func (s *Store) Sources() error {
	tier, err := s.Tier()
	if err != nil {
		return err
	}
	for _, svc := range s.Services {
		path := s.File(svc)
		if err := checkFile(path, tier); err != nil {
			return Failure{svc.Name + "_" + err.Error()}
		}
		if !s.decrypts(svc, path) {
			return Failure{svc.Name + "_does_not_decrypt"}
		}
	}
	return nil
}

// NoGlobals fails when sandboxd holds a global service or custom record: every key must be
// scoped to one sandbox, so no other sandbox can use it.
func (s *Store) NoGlobals(ctx context.Context) error {
	out, err := s.Runner.Output(ctx, sbx.Call{Args: []string{"secret", "ls", "--global", "--json"},
		Timeout: 60 * time.Second, Private: true})
	if err != nil {
		return Failure{"global_secret_listing_unreadable"}
	}
	var l listing
	if json.Unmarshal(out, &l) != nil || l.Secrets == nil || l.Custom == nil {
		return Failure{"global_secret_listing_unreadable"}
	}
	for _, r := range l.Secrets {
		if r.Type == "service" {
			return Failure{"global_secret_present"}
		}
	}
	if len(l.Custom) > 0 {
		return Failure{"global_secret_present"}
	}
	return nil
}

// WireNew wires every placeholder service to a sandbox just created (its native services come
// from its sbxenv.yaml), then proves every service, native and placeholder, has exactly its
// own record. The caller has just proven the sandbox, so it reads the records once before
// (no foreign record may be overwritten) and once after, not once per service.
func (s *Store) WireNew(ctx context.Context) error {
	before, err := s.records(ctx)
	if err != nil {
		return Failure{"sandbox_secret_listing_unreadable"}
	}
	for _, svc := range s.Services {
		if svc.Placeholder && before.state(s.Agent, svc) == unexpected {
			return Failure{svc.Name + "_" + unexpected}
		}
	}
	for _, svc := range s.Services {
		if svc.Placeholder {
			if err := s.setRecord(ctx, svc); err != nil {
				return Failure{svc.Name + "_sandbox_wiring_failed"}
			}
		}
	}
	l, err := s.records(ctx)
	if err != nil {
		return Failure{"sandbox_secret_listing_unreadable"}
	}
	for _, svc := range s.Services {
		if problem := l.state(s.Agent, svc); problem != "" {
			return Failure{svc.Name + "_" + problem}
		}
	}
	return nil
}
