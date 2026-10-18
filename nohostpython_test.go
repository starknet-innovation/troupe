package troupe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pythonWord matches a Python interpreter named as a program, alone or by path.
var pythonWord = regexp.MustCompile(`^(.*/)?python[0-9.]*$`)

// shellWords splits a string the way a shell command line separates program names.
var shellWords = regexp.MustCompile(`[\s;&|()'"]+`)

// The one Python troupe names is Hermes's interpreter inside the VM: the Python constant in
// the guest package, used only by that package to build the sbx exec argument list.
const (
	guestFile  = "internal/guest/guest.go"
	guestConst = "Python"
)

// pythonUses returns the places in one Go source that name Python on the host: a string
// with a Python program word anywhere in it (exec.Command("python3"), "sh -c '... python3 x'"),
// or a reference to the guest package's Python constant from outside that package.
func pythonUses(t *testing.T, name, source string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ValueSpec:
			// The guest constant's own declaration is the allowed use; skip its value.
			if name == guestFile && len(n.Names) == 1 && n.Names[0].Name == guestConst {
				return false
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(n.Value); err == nil {
				for _, word := range shellWords.Split(value, -1) {
					if pythonWord.MatchString(word) {
						found = append(found, value)
						break
					}
				}
			}
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Name == "guest" && n.Sel.Name == guestConst {
				found = append(found, "guest."+guestConst)
			}
		}
		return true
	})
	return found
}

// Nothing troupe runs on the host is Python.
func TestNoHostPython(t *testing.T) {
	sawGuestConst := false
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if path == guestFile {
			sawGuestConst = strings.Contains(string(source), guestConst+` = "/`)
		}
		if found := pythonUses(t, path, string(source)); len(found) > 0 {
			t.Errorf("%s names Python on the host: %q", path, found)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scan must reach the one allowed declaration, or the allowance is stale.
	if !sawGuestConst {
		t.Errorf("%s no longer declares %s: update this test", guestFile, guestConst)
	}
}

// The scan catches each way host code could start Python, and leaves prose alone.
func TestNoHostPythonCatchesPythonCalls(t *testing.T) {
	caught := map[string]string{
		"program":       "package x\nimport \"os/exec\"\nfunc f() { exec.Command(\"python3\", \"-c\", \"print(1)\").Run() }\n",
		"path":          "package x\nconst s = \"/usr/bin/python3\"\n",
		"inside shell":  "package x\nconst s = \"cd /srv && python3 tool.py\"\n",
		"guest const":   "package x\nimport \"x/guest\"\nvar p = guest.Python\n",
		"guest literal": "package guest\nconst Other = \"/opt/hermes/venv/bin/python\"\n",
	}
	for title, source := range caught {
		if found := pythonUses(t, title+".go", source); len(found) != 1 {
			t.Errorf("%s: found %q", title, found)
		}
	}
	allowed := map[string]string{
		"prose":             "package x\nconst s = \"jobs run on Hermes's Python in the VM\"\n",
		"guest declaration": "package guest\nconst Python = \"/usr/bin/python3\"\n",
	}
	for title, source := range allowed {
		name := title + ".go"
		if title == "guest declaration" {
			name = guestFile
		}
		if found := pythonUses(t, name, source); len(found) != 0 {
			t.Errorf("%s flagged: %q", title, found)
		}
	}
}

// Every in-VM job on disk ships in exactly one build list: every build's, or the dev build's.
func TestEveryGuestJobShipsInOneBuild(t *testing.T) {
	disk, _ := filepath.Glob("guest/*.py")
	for _, path := range disk {
		_, inGuest := fs.Stat(Guest, path)
		_, inDev := fs.Stat(GuestDev, path)
		dev := map[string]bool{"guest/harvest.py": true}[path]
		if (inGuest == nil) == dev {
			t.Errorf("%s: in every build's jobs %v, a dev job %v", path, inGuest == nil, dev)
		}
		if inDev == nil && !dev {
			t.Errorf("%s: in the dev build's jobs", path)
		}
		// A dev build ships every dev job; a release ships none.
		if _, err := fs.Stat(GuestDev, "guest"); err == nil && dev && inDev != nil {
			t.Errorf("%s: missing from the dev build's jobs", path)
		}
	}
}
