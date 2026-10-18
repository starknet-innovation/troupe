package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/starknet-innovation/troupe/internal/registry"
)

// displayLimit is the longest name a person sees: Slack's limit on an app's name.
const displayLimit = 35

// slackLike is a Slack ID, as troupe's seed checks recognize one.
var slackLike = regexp.MustCompile(`\b[TUWCD]0[A-Z0-9]{8,}\b`)

// fold maps the accented Latin letters an agent's name may hold to plain ones for its ID.
var fold = func() map[rune]string {
	m := map[rune]string{'ß': "ss", 'æ': "ae", 'œ': "oe", 'þ': "th", 'ð': "d"}
	for plain, accented := range map[string]string{
		"a": "àáâãäåāăą", "c": "çćĉċč", "d": "ďđ", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥħ",
		"i": "ìíîïĩīĭįı", "j": "ĵ", "k": "ķ", "l": "ĺļľŀł", "n": "ñńņňŉ", "o": "òóôõöøōŏő",
		"r": "ŕŗř", "s": "śŝşš", "t": "ţťŧ", "u": "ùúûüũūŭůűų", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range accented {
			m[r] = plain
		}
	}
	return m
}()

// agentNames turns the name a person gives an agent into its display name (as typed, trimmed)
// and its ID: lowercase ASCII letters and digits, accents folded, every other run of characters
// one hyphen. The ID names the sandbox, the folders and the keys; the display name is what people
// read (its SOUL, its Slack app, troupe's progress lines).
func agentNames(given string) (display, id string, err error) {
	display = strings.TrimSpace(given)
	switch {
	case display == "":
		return "", "", errors.New("give the agent a name")
	case !utf8.ValidString(display):
		return "", "", errors.New("the name is not valid text")
	case utf8.RuneCountInString(display) > displayLimit:
		return "", "", fmt.Errorf("the name is longer than %d characters (Slack's limit for an app)", displayLimit)
	case slackLike.MatchString(display):
		// It would sit in the agent's SOUL, where Slack IDs are refused.
		return "", "", errors.New("the name looks like a Slack ID; give the agent a name people would use")
	}
	var b strings.Builder
	hyphen := false
	for _, r := range display {
		if !unicode.IsPrint(r) {
			return "", "", errors.New("the name holds a character that is not printable")
		}
		r = unicode.ToLower(r)
		plain, folded := fold[r]
		switch {
		case folded:
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			plain = string(r)
		default:
			hyphen = b.Len() > 0
			continue
		}
		if hyphen {
			b.WriteByte('-')
			hyphen = false
		}
		b.WriteString(plain)
	}
	id = b.String()
	if id == "" {
		return "", "", errors.New("the name needs at least one Latin letter or digit, from which troupe makes its ID")
	}
	if !sandboxName.MatchString(id) {
		return "", "", fmt.Errorf("the name makes the ID %s, which is too long", id)
	}
	return display, id, nil
}

// displayName is how an agent is shown: the name a published agent was created with, else its
// ID capitalized (local agents, such as those in a maintainer's kits root).
func displayName(reg registry.Registry, id string) string {
	data, err := os.ReadFile(filepath.Join(reg.Data, "agents", id, "source.json"))
	if err == nil {
		var s struct {
			DisplayName string `json:"display_name"`
		}
		if json.Unmarshal(data, &s) == nil {
			if display, check, err := agentNames(s.DisplayName); err == nil && check == id {
				return display
			}
		}
	}
	return label(id)
}
