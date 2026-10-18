package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// placeholder is the app name in the base kit's Slack manifest; each app gets the agent's name.
const placeholder = "AGENT"

// SlackManifest returns the base kit's Slack app manifest named after the agent, keeping the
// file's key order so the result reads like the source when pasted into Slack.
func (r Registry) SlackManifest(agent string) ([]byte, error) {
	base, err := r.Base(agent)
	if err != nil {
		return nil, err
	}
	return SlackManifestIn(base, capitalize(agent))
}

// SlackManifestIn returns the Slack app manifest of the kit in folder, its app and bot named
// display (the name people read).
func SlackManifestIn(folder, display string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(folder, "slack-app-manifest.json"))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	manifest, err := decodeValue(decoder)
	if err != nil {
		return nil, fmt.Errorf("slack-app-manifest.json: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("slack-app-manifest.json: trailing data")
	}
	app := field(manifest, "display_information", "name")
	bot := field(manifest, "features", "bot_user", "display_name")
	if app == nil || bot == nil || app.value != placeholder || bot.value != placeholder {
		return nil, fmt.Errorf("the base Slack app manifest must name the app %s", placeholder)
	}
	app.value, bot.value = display, display
	var out bytes.Buffer
	encodeValue(&out, manifest, 0)
	return out.Bytes(), nil
}

// capitalize upper-cases the first letter and lower-cases the rest, like Python's str.capitalize.
func capitalize(name string) string {
	first, size := utf8.DecodeRuneInString(name)
	if first == utf8.RuneError {
		return name
	}
	return string(unicode.ToUpper(first)) + strings.ToLower(name[size:])
}

// member is one key of a JSON object; objects keep their members in file order.
type member struct {
	key   string
	value any
}

type object []*member

// field returns the member holding path's last key, or nil.
func field(value any, path ...string) *member {
	var found *member
	for _, key := range path {
		obj, ok := value.(object)
		if !ok {
			return nil
		}
		found = nil
		for _, m := range obj {
			if m.key == key {
				found = m
			}
		}
		if found == nil {
			return nil
		}
		value = found.value
	}
	return found
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch delim := token.(type) {
	case json.Delim:
		switch delim {
		case '{':
			obj := object{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				value, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				obj = append(obj, &member{key: key.(string), value: value})
			}
			_, err := decoder.Token()
			return obj, err
		case '[':
			list := []any{}
			for decoder.More() {
				value, err := decodeValue(decoder)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
			_, err := decoder.Token()
			return list, err
		}
		return nil, fmt.Errorf("unexpected %v", delim)
	default:
		return token, nil
	}
}

// encodeValue writes value as Python's json.dumps(indent=2, ensure_ascii=False) does.
func encodeValue(out *bytes.Buffer, value any, depth int) {
	pad := func(level int) { out.WriteString("\n" + strings.Repeat("  ", level)) }
	switch v := value.(type) {
	case object:
		if len(v) == 0 {
			out.WriteString("{}")
			return
		}
		out.WriteString("{")
		for i, m := range v {
			if i > 0 {
				out.WriteString(",")
			}
			pad(depth + 1)
			encodeString(out, m.key)
			out.WriteString(": ")
			encodeValue(out, m.value, depth+1)
		}
		pad(depth)
		out.WriteString("}")
	case []any:
		if len(v) == 0 {
			out.WriteString("[]")
			return
		}
		out.WriteString("[")
		for i, item := range v {
			if i > 0 {
				out.WriteString(",")
			}
			pad(depth + 1)
			encodeValue(out, item, depth+1)
		}
		pad(depth)
		out.WriteString("]")
	case string:
		encodeString(out, v)
	case json.Number:
		out.WriteString(v.String())
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	}
}

func encodeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}
