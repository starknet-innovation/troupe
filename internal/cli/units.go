package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/starknet-innovation/troupe"
)

const unitsHelp = `Usage:
  troupe units render UNIT

Prints a systemd unit template with this host's values filled in: @TROUPE@ (this troupe
binary), @USER@ and @HOME@ (the invoking user). Run it as that user, never as root, and
install the result yourself, for example:

  troupe units render sbx-daemon.service | sudo tee /etc/systemd/system/sbx-daemon.service

Units: %s
`

func units(e env, args []string) int {
	names, err := unitNames()
	if err != nil {
		return fail(e, err)
	}
	help := fmt.Sprintf(unitsHelp, strings.Join(names, ", "))
	if helpRequested(args) {
		fmt.Fprint(e.out, help)
		return 0
	}
	if len(args) == 0 || args[0] != "render" {
		return usageError(e, help, "units needs the render subcommand")
	}
	rest, code, ok := subcommand(e, "units render", help, args[1:], nil)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, help, "units render: name one unit")
	}
	text, err := renderUnit(filepath.Base(rest[0]))
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprint(e.out, text)
	return 0
}

func unitNames() ([]string, error) {
	entries, err := fs.ReadDir(troupe.Units, "systemd")
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names, nil
}

// geteuid is replaced in tests to prove a root caller is refused.
var geteuid = os.Geteuid

func renderUnit(name string) (string, error) {
	if geteuid() == 0 {
		return "", errors.New("render as the host user, not root")
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	// A unit outlives this process: a binary that later vanishes would make systemd skip the
	// unit without failing it.
	if transient(self) {
		return "", fmt.Errorf("troupe runs from a temporary folder (%s); build it to a lasting path first", self)
	}
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return fillUnit(name, self, account.Username, home)
}

func fillUnit(name, self, username, home string) (string, error) {
	template, err := fs.ReadFile(troupe.Units, "systemd/"+name)
	if err != nil {
		return "", fmt.Errorf("no unit template named %s", name)
	}
	return strings.NewReplacer("@TROUPE@", self, "@USER@", username, "@HOME@", home).Replace(string(template)), nil
}

// transient reports a binary path that will not last: the temporary folder, or Go's build
// cache, where go run and go test keep the programs they build.
func transient(path string) bool {
	if temp, err := filepath.EvalSymlinks(os.TempDir()); err == nil && strings.HasPrefix(path, temp+string(filepath.Separator)) {
		return true
	}
	return strings.Contains(path, string(filepath.Separator)+"go-build")
}
