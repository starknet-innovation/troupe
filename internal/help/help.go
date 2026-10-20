// Package help renders every command's --help in one layout: a one-line summary, the usage
// forms, then each subcommand, argument and flag on its own line, saying whether it is
// required or what its default is.
package help

import (
	"strings"
	"unicode/utf8"
)

// Width is the widest line help prints.
const Width = 100

// Command is one command's help.
type Command struct {
	Summary  string   // one sentence
	Usage    []string // forms, without the leading "troupe "
	Commands []Row    // subcommands: "set AGENT SERVICE" and what it does
	Args     []Arg
	Flags    []Flag
	Sections []Section // further tables, such as a type's asked values or config's settings
	Notes    []string  // what a user must know before running it, one short line each
}

// Arg is a positional argument. An optional one names its default.
type Arg struct {
	Name, Desc string
	Optional   bool
	Default    string
}

// Flag is one flag. One that is not required names its default; Only names the subcommand a
// flag belongs to when the command has several.
type Flag struct {
	Name, Value, Desc string
	Required          bool
	Default           string
	Only              string
}

// Row is one line of a table: a name and its description.
type Row struct{ Name, Desc string }

// Section is a titled table.
type Section struct {
	Title string
	Rows  []Row
}

// Render lays the command's help out, each table's descriptions aligned and wrapped to Width.
func Render(c Command) string {
	var b strings.Builder
	b.WriteString(c.Summary + "\n\nUsage:\n")
	for _, u := range c.Usage {
		b.WriteString("  troupe " + u + "\n")
	}
	table(&b, "Commands", c.Commands)
	var args []Row
	for _, a := range c.Args {
		args = append(args, Row{a.Name, a.Desc + " " + state(!a.Optional, a.Default)})
	}
	table(&b, "Arguments", args)
	var flags []Row
	for _, f := range c.Flags {
		name := "--" + f.Name
		if f.Value != "" {
			name += " " + f.Value
		}
		desc := f.Desc
		if f.Only != "" {
			desc += " For " + f.Only + " only."
		}
		flags = append(flags, Row{name, desc + " " + state(f.Required, f.Default)})
	}
	flags = append(flags, Row{"-h, --help", "Show this help."})
	table(&b, "Flags", flags)
	for _, s := range c.Sections {
		table(&b, s.Title, s.Rows)
	}
	if len(c.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range c.Notes {
			wrap(&b, "  ", "    ", n)
		}
	}
	return b.String()
}

func state(required bool, def string) string {
	if required {
		return "Required."
	}
	return "Default: " + def + "."
}

// table prints rows under a title, the descriptions in one column after the widest name.
func table(b *strings.Builder, title string, rows []Row) {
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n" + title + ":\n")
	column := 0
	for _, r := range rows {
		column = max(column, utf8.RuneCountInString(r.Name))
	}
	for _, r := range rows {
		name := "  " + r.Name + strings.Repeat(" ", column-utf8.RuneCountInString(r.Name)) + "  "
		wrap(b, name, strings.Repeat(" ", utf8.RuneCountInString(name)), r.Desc)
	}
}

// wrap writes text after first, continuing on lines that start with rest, within Width.
func wrap(b *strings.Builder, first, rest, text string) {
	line := first
	start := utf8.RuneCountInString(first)
	for _, word := range strings.Fields(text) {
		if utf8.RuneCountInString(line) > start && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > Width {
			b.WriteString(line + "\n")
			line, start = rest, utf8.RuneCountInString(rest)
		}
		if utf8.RuneCountInString(line) > start {
			line += " "
		}
		line += word
	}
	b.WriteString(line + "\n")
}
