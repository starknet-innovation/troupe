package published

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/starknet-innovation/troupe/internal/secret"
)

// A kit tells troupe what create asks for and allocates in one image label, LabelName, holding
// one JSON document (Kit). troupe owns that format: `troupe kit label` is its only producer, and
// create reads it from the pinned image.
const (
	LabelName = "org.host.troupe.kit"
	// Format is the newest label format this troupe reads. It grows only for a change an older
	// troupe must not ignore: a field an older troupe may skip is added without it.
	Format     = 1
	labelLimit = 64 << 10
)

// Kit is a kit's label: its default sizes, the settings create asks for, then its services.
type Kit struct {
	Format   int                 `json:"format"`
	Defaults Defaults            `json:"defaults"`
	Settings []Setting           `json:"settings,omitempty"`
	Services []secret.KitService `json:"services"`
}

// Why a label cannot be used, apart from being malformed.
var (
	ErrNoLabel     = errors.New("the kit has no troupe label")
	ErrNewerFormat = errors.New("the kit's troupe label is in a newer format")
)

// DecodeLabel reads a kit's label. A field this troupe does not know is skipped, so a kit may
// add one without locking out installed troupes; a newer Format is refused before any other
// field is read, since only its publisher's troupe knows what it holds. The size cap comes
// first whatever the format: it bounds what troupe reads at all.
func DecodeLabel(text string) (Kit, error) {
	text = strings.TrimSpace(text)
	if text == "" || text == "<no value>" {
		return Kit{}, ErrNoLabel
	}
	if len(text) > labelLimit {
		return Kit{}, fmt.Errorf("larger than %d KiB", labelLimit>>10)
	}
	// The format alone first: a newer one may have changed any other field's shape.
	var only struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal([]byte(text), &only); err != nil {
		return Kit{}, errors.New("not a JSON document")
	}
	if only.Format > Format {
		return Kit{}, ErrNewerFormat
	}
	if only.Format < 1 {
		return Kit{}, errors.New("no format")
	}
	var k Kit
	if err := json.Unmarshal([]byte(text), &k); err != nil {
		return Kit{}, errors.New("a field of another type than this format's")
	}
	if err := k.check(); err != nil {
		return Kit{}, err
	}
	return k, nil
}

func (k Kit) check() error {
	if err := k.Defaults.check(); err != nil {
		return err
	}
	if err := checkSettings(k.Settings); err != nil {
		return err
	}
	return secret.CheckKitServices(k.Services)
}

// Encode is the label's canonical text: one line, in the struct's field order.
func (k Kit) Encode() (string, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(k); err != nil {
		return "", err
	}
	text := strings.TrimSpace(b.String())
	if len(text) > labelLimit {
		return "", fmt.Errorf("the label is larger than %d KiB", labelLimit>>10)
	}
	return text, nil
}

// SettingsClash refuses a setting that would set a line the kit's env template sets, or a
// placeholder service's variable: each line of an agent's .env has one writer.
func SettingsClash(settings []Setting, template []byte, services []secret.KitService) error {
	taken := map[string]bool{}
	for _, line := range strings.Split(string(template), "\n") {
		if key, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			taken[key] = true
		}
	}
	for _, s := range services {
		if s.Env != "" {
			taken[s.Env] = true
		}
	}
	for _, s := range settings {
		if taken[s.Env] {
			return fmt.Errorf("the kit already sets %s", s.Env)
		}
	}
	return nil
}

// ReadSource reads a kit's source folder into its label, refusing whatever create would: its
// troupe/ files (defaults.json, services.json and, when it has settings, settings.json), read
// strictly so a misspelled field is caught before the kit is published; a setting that clashes
// with templates/env or a service; and Slack services without the Slack app manifest and the
// env template create needs for them, or the manifest without Slack services.
func ReadSource(dir string) (Kit, error) {
	folder := filepath.Join(dir, "troupe")
	k := Kit{Format: Format}
	var err error
	if k.Defaults, err = ReadDefaults(folder); err != nil {
		return Kit{}, err
	}
	if k.Settings, err = ReadSettings(folder); err != nil {
		return Kit{}, err
	}
	if k.Services, err = secret.KitServices(folder); err != nil {
		return Kit{}, err
	}
	template, err := os.ReadFile(filepath.Join(dir, "templates", "env"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Kit{}, err
	}
	hasTemplate := err == nil
	if err := SettingsClash(k.Settings, template, k.Services); err != nil {
		return Kit{}, fmt.Errorf("%s: %w", filepath.Join(folder, "settings.json"), err)
	}
	slack := false
	for _, s := range k.Services {
		slack = slack || s.Slack
	}
	_, err = os.Stat(filepath.Join(dir, "slack-app-manifest.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Kit{}, err
	}
	switch manifest := err == nil; {
	case slack && !manifest:
		return Kit{}, fmt.Errorf("%s: Slack services without slack-app-manifest.json", dir)
	case manifest && !slack:
		return Kit{}, fmt.Errorf("%s: slack-app-manifest.json without a Slack service", dir)
	case slack && !hasTemplate:
		return Kit{}, fmt.Errorf("%s: a kit with Slack needs templates/env", dir)
	}
	return k, nil
}

// capped keeps what Docker prints up to a limit, and notes when there was more.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.over = true
		c.buf.Write(p[:room])
	} else {
		c.buf.Write(p)
	}
	return len(p), nil
}

// Label reads the kit's label from the local image at reference, which the caller has pulled.
func (d Docker) Label(ctx context.Context, reference string) (Kit, error) {
	out := &capped{limit: labelLimit + 1}
	if _, err := d.run(ctx, time.Minute, out, "image", "inspect", reference, "--format",
		`{{index .Config.Labels "`+LabelName+`"}}`); err != nil {
		return Kit{}, err
	}
	if out.over {
		return Kit{}, fmt.Errorf("%s: its troupe label: larger than %d KiB", reference, labelLimit>>10)
	}
	return decodeFor(reference, out.buf.String())
}

// decodeFor is the label of the kit at reference, with what to do about one troupe cannot read.
func decodeFor(reference, text string) (Kit, error) {
	k, err := DecodeLabel(text)
	if err != nil {
		return Kit{}, decodeForError(reference, err)
	}
	return k, nil
}

func decodeForError(reference string, err error) error {
	switch {
	case errors.Is(err, ErrNoLabel):
		return fmt.Errorf("%s is too old for this troupe (it has no troupe label)", reference)
	case errors.Is(err, ErrNewerFormat):
		return fmt.Errorf("%s needs a newer troupe: run `troupe update`", reference)
	}
	return fmt.Errorf("%s: its troupe label: %w", reference, err)
}

// Describe reads a kit's label and its one line description from the registry, without pulling
// the kit: enough for help. create never trusts this read: it reads the label again from the
// image it pinned and pulled.
func (d Docker) Describe(ctx context.Context, reference string) (Kit, string, error) {
	out := &capped{limit: 4 * labelLimit}
	if _, err := d.run(ctx, time.Minute, out, "buildx", "imagetools", "inspect", reference, "--format",
		"{{json .Image.Config.Labels}}"); err != nil {
		return Kit{}, "", err
	}
	var labels map[string]string
	if out.over || json.Unmarshal(out.buf.Bytes(), &labels) != nil {
		return Kit{}, "", fmt.Errorf("%s: unreadable image labels", reference)
	}
	k, err := decodeFor(reference, labels[LabelName])
	if err != nil {
		return Kit{}, "", err
	}
	// The description is the publisher's text, shown on a terminal: one printable line of it.
	description := strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, labels["org.opencontainers.image.description"])
	if runes := []rune(description); len(runes) > 300 {
		description = string(runes[:300])
	}
	return k, strings.Join(strings.Fields(description), " "), nil
}

// write keeps the label in a kit folder as the files the rest of troupe reads there.
func (k Kit) write(folder string) error {
	files := map[string]any{
		"defaults.json":     k.Defaults,
		secret.ServicesFile: map[string]any{"services": k.Services},
	}
	if len(k.Settings) > 0 {
		files["settings.json"] = map[string]any{"settings": k.Settings}
	}
	for name, value := range files {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(folder, name), append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// labelFiles are the names the label owns in a kit folder: a copy inside the image is never read.
var labelFiles = map[string]bool{"defaults.json": true, "settings.json": true, secret.ServicesFile: true}
