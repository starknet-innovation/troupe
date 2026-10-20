package help

import (
	"strings"
	"testing"
)

func TestRenderGivesEachValueALineSayingRequiredOrItsDefault(t *testing.T) {
	out := Render(Command{
		Summary:  "Do a thing.",
		Usage:    []string{"thing AGENT [flags]"},
		Commands: []Row{{Name: "set KEY", Desc: "Set it."}},
		Args:     []Arg{{Name: "AGENT", Desc: "The agent."}, {Name: "DIR", Desc: "A folder.", Optional: true, Default: "here"}},
		Flags: []Flag{
			{Name: "name", Value: "NAME", Desc: "Its name.", Required: true},
			{Name: "wait", Value: "DURATION", Desc: "Retry.", Default: "0", Only: "probe"},
		},
		Sections: []Section{{Title: "Types", Rows: []Row{{Name: "coder", Desc: "A coder."}}}},
		Notes:    []string{"Asks for sudo."},
	})
	want := `Do a thing.

Usage:
  troupe thing AGENT [flags]

Commands:
  set KEY  Set it.

Arguments:
  AGENT  The agent. Required.
  DIR    A folder. Default: here.

Flags:
  --name NAME      Its name. Required.
  --wait DURATION  Retry. For probe only. Default: 0.
  -h, --help       Show this help.

Types:
  coder  A coder.

Notes:
  Asks for sudo.
`
	if out != want {
		t.Fatalf("got:\n%s", out)
	}
}

func TestRenderWrapsWithinTheWidthUnderItsColumn(t *testing.T) {
	out := Render(Command{Summary: "S.", Usage: []string{"x"},
		Args:  []Arg{{Name: "AGENT", Desc: strings.Repeat("word ", 40)}},
		Notes: []string{strings.Repeat("note ", 40)}})
	for _, line := range strings.Split(out, "\n") {
		if len(line) > Width {
			t.Fatalf("%d columns: %q", len(line), line)
		}
	}
	if !strings.Contains(out, "\n         word") || !strings.Contains(out, "\n    note") {
		t.Fatalf("continuations not indented:\n%s", out)
	}
}
