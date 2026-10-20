package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // a static binary must not depend on the host's zoneinfo files
)

// Setting is one machine setting troupe config can read and change.
type Setting struct {
	// Help says what it is; Default what unset means.
	Key, Help, Default string
	// parse turns the text a user types into the stored JSON value, refusing bad input.
	parse func(text string) (any, error)
	// stored is the stored value's JSON type: number or string.
	stored string
}

// check accepts a stored value of the right JSON type and shape. A kits folder that has since
// moved still loads; Resolve reports it when a command needs the kits.
func (s Setting) check(value any) error {
	switch v := value.(type) {
	case json.Number:
		if s.stored == "number" {
			_, err := s.parse(v.String())
			return err
		}
	case string:
		if s.stored == "string" {
			if s.Key == "kits" {
				if !filepath.IsAbs(v) {
					return errors.New("an absolute folder path")
				}
				return nil
			}
			_, err := s.parse(v)
			return err
		}
	}
	return fmt.Errorf("a %s", s.stored)
}

// Settings lists every machine setting, in the order help and list show them; the dev build
// adds the kits root second.
var Settings = withKits([]Setting{
	{
		Key:     "timezone",
		stored:  "string",
		Help:    "Time zone of progress lines, such as Europe/Paris.",
		Default: "the host's",
		parse: func(text string) (any, error) {
			if _, err := time.LoadLocation(text); err != nil || text == "" || text == "Local" {
				return nil, errors.New("an IANA zone name such as Europe/Paris")
			}
			return text, nil
		},
	},
	{
		Key:     "collector-timer",
		stored:  "string",
		Help:    "A host timer that reset and resize pause.",
		Default: "none",
		parse: func(text string) (any, error) {
			if !timerName.MatchString(text) {
				return nil, errors.New("a timer unit name, such as versions.timer")
			}
			return text, nil
		},
	},
	{
		Key:     "registry",
		stored:  "string",
		Help:    "Where the published kits are.",
		Default: DefaultRegistry,
		parse: func(text string) (any, error) {
			if !registryName.MatchString(text) {
				return nil, errors.New("a registry host and path, such as ghcr.io/example")
			}
			return text, nil
		},
	},
	boundarySetting("slack-boundary-public", "public"),
	boundarySetting("slack-boundary-private", "private"),
})

// devOnly reports whether key is the dev build's kits setting read by a release build, which
// keeps it in the file (a dev build may share the machine) without reading or checking it.
func devOnly(key string) bool { return key == "kits" && kitsSetting == nil }

// withKits puts the dev build's kits setting after the first (timezone).
func withKits(settings []Setting) []Setting {
	if kitsSetting == nil {
		return settings
	}
	return append(settings[:1:1], append([]Setting{*kitsSetting}, settings[1:]...)...)
}

// boundarySetting is one Slack channel every published agent must be unable to read or post in
// (troupe checks it when it sets up an agent's Slack); unset means no such check.
func boundarySetting(key, which string) Setting {
	return Setting{
		Key:     key,
		stored:  "string",
		Help:    "A " + which + " Slack channel ID (C…) agents must not reach.",
		Default: "none",
		parse: func(text string) (any, error) {
			if !slackChannel.MatchString(text) {
				return nil, errors.New("a Slack channel ID, such as C0123456789")
			}
			return text, nil
		},
	}
}

var (
	timerName = regexp.MustCompile(`^[A-Za-z0-9@._-]+\.timer$`)
	// A Slack channel ID.
	slackChannel = regexp.MustCompile(`^C[A-Z0-9]{2,}$`)
	// A registry host (with an optional port) and at least one lowercase path component.
	registryName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)+$`)
)

// DefaultRegistry is where published kits come from when the registry setting is unset.
const DefaultRegistry = "ghcr.io/starknet-innovation/agents"

// Machine is this machine's settings; zero values mean unset.
type Machine struct {
	Timezone       string // "": the host's zone
	Kits           string // "": none
	CollectorTimer string // "": none
	Registry       string // "": DefaultRegistry
	// The Slack channels published agents must not reach; "": none.
	BoundaryPublic, BoundaryPrivate string
}

// MachinePath is the settings file: $XDG_CONFIG_HOME/troupe/config.json, else ~/.config/troupe/config.json.
func MachinePath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "troupe", "config.json")
}

func lookup(key string) (Setting, error) {
	for _, s := range Settings {
		if s.Key == key {
			return s, nil
		}
	}
	keys := make([]string, len(Settings))
	for i, s := range Settings {
		keys[i] = s.Key
	}
	return Setting{}, fmt.Errorf("unknown setting %q; settings: %s", key, strings.Join(keys, ", "))
}

// readRaw returns the stored values as text keyed by setting; an absent file is empty.
func readRaw() (map[string]any, error) {
	values := map[string]any{}
	data, err := os.ReadFile(MachinePath())
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("%s: %w", MachinePath(), err)
	}
	if values == nil { // the file held null
		values = map[string]any{}
	}
	// Recheck every stored value's type and shape, so a hand edit cannot slip a bad one past
	// troupe config. Whether the kits folder still exists is checked only where it is used, so
	// a moved folder never breaks commands that don't need it, such as a unit's ExecStop.
	for key, value := range values {
		if devOnly(key) {
			continue // the dev build's kits root: a release keeps it, unread
		}
		if why, ok := retired[key]; ok {
			return nil, fmt.Errorf("%s: %s is retired: %s; run troupe config unset %s", MachinePath(), key, why, key)
		}
		setting, err := lookup(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", MachinePath(), err)
		}
		if err := setting.check(value); err != nil {
			return nil, fmt.Errorf("%s: %s must be %v", MachinePath(), key, err)
		}
	}
	return values, nil
}

// LoadMachine reads this machine's settings.
func LoadMachine() (Machine, error) {
	values, err := readRaw()
	if err != nil {
		return Machine{}, err
	}
	var machine Machine
	if v, ok := values["timezone"]; ok {
		machine.Timezone = fmt.Sprint(v)
	}
	if v, ok := values["kits"]; ok {
		machine.Kits = fmt.Sprint(v)
	}
	if v, ok := values["collector-timer"]; ok {
		machine.CollectorTimer = fmt.Sprint(v)
	}
	machine.Registry = DefaultRegistry
	if v, ok := values["registry"]; ok {
		machine.Registry = fmt.Sprint(v)
	}
	if v, ok := values["slack-boundary-public"]; ok {
		machine.BoundaryPublic = fmt.Sprint(v)
	}
	if v, ok := values["slack-boundary-private"]; ok {
		machine.BoundaryPrivate = fmt.Sprint(v)
	}
	return machine, nil
}

// DataDir holds what troupe creates for published agents: $XDG_DATA_HOME/troupe, else
// ~/.local/share/troupe (agents/NAME, each agent's environment; kits/, the published kits' files).
func DataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "troupe")
}

// Location is the zone progress times are printed in.
func (m Machine) Location() *time.Location {
	if m.Timezone == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(m.Timezone); err == nil {
		return loc
	}
	return time.Local
}

// Get returns a setting's stored text and whether it is set.
func Get(key string) (string, bool, error) {
	if _, err := lookup(key); err != nil {
		return "", false, err
	}
	values, err := readRaw()
	if err != nil {
		return "", false, err
	}
	value, ok := values[key]
	if !ok {
		return "", false, nil
	}
	return fmt.Sprint(value), true, nil
}

// Set validates and stores one setting.
func Set(key, text string) error {
	setting, err := lookup(key)
	if err != nil {
		return err
	}
	value, err := setting.parse(text)
	if err != nil {
		return fmt.Errorf("%s must be %v", key, err)
	}
	values, err := readRaw()
	if err != nil {
		return err
	}
	values[key] = value
	return write(values)
}

// retired settings: a file may still hold one, which only unset accepts.
var retired = map[string]string{"max-running-agents": "the resource budget replaces it"}

// Unset removes one setting, returning it to its default. It also removes a retired one.
func Unset(key string) error {
	if _, gone := retired[key]; gone || devOnly(key) {
		return unsetRetired(key)
	}
	if _, err := lookup(key); err != nil {
		return err
	}
	values, err := readRaw()
	if err != nil {
		return err
	}
	delete(values, key)
	return write(values)
}

// unsetRetired removes a retired key from the settings file, keeping every other value as stored.
func unsetRetired(key string) error {
	data, err := os.ReadFile(MachinePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	values := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return fmt.Errorf("%s: %w", MachinePath(), err)
	}
	delete(values, key)
	return write(values)
}

// write replaces the file atomically, readable only by its owner.
func write(values map[string]any) error {
	path := MachinePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "config.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
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
