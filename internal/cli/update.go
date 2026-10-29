package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/doctor"
	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/published"
)

// releases is where troupe's releases are published, and installScript where a release's
// installer is read; tests point both at a fake.
var (
	releases      = "https://github.com/starknet-innovation/troupe/releases"
	installScript = func(tag string) string {
		return "https://raw.githubusercontent.com/starknet-innovation/troupe/" + tag + "/install.sh"
	}
	// running is this troupe's version; tests set a release's.
	running = Version
	// installedHere refuses a troupe other than the installer's ~/.local/bin/troupe, which is
	// the one an update replaces; tests accept theirs.
	installedHere = func() error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		self, _ := filepath.EvalSymlinks(exe)
		installed, _ := filepath.EvalSymlinks(filepath.Join(home, ".local", "bin", "troupe"))
		if self == "" || self != installed {
			return fmt.Errorf("this troupe runs from %s, but troupe update replaces %s, the installer's: run that one", exe, filepath.Join(home, ".local", "bin", "troupe"))
		}
		return nil
	}
)

// maxInstaller caps the installer read: install.sh is a few KiB, and a cut one would run nothing.
const maxInstaller = 1 << 20

var updateHelp = help.Render(help.Command{
	Summary: "Update troupe to its latest release.",
	Usage:   []string{"update"},
	Notes: []string{
		"Runs that release's own installer: it checks the download against the release's checksums, puts the old troupe back on any failure, and asks for sudo only when troupe's systemd units changed.",
		"Running agents are not touched; they use the new troupe from their next command.",
		"troupe doctor says when a newer release is out.",
	},
})

func updateCommand(e env, args []string) int {
	// The dev build's maintainer command was called update; its flags would not parse here.
	if devBuild && len(args) > 0 && (args[0] == "skills" || args[0] == "scripts" || args[0] == "soul") {
		return usageError(e, updateHelp, "update %s is now troupe reseed %s", args[0], strings.Join(args, " "))
	}
	rest, code, ok := subcommand(e, "update", updateHelp, args, nil)
	if !ok {
		return code
	}
	if len(rest) != 0 {
		return usageError(e, updateHelp, "update takes no arguments")
	}
	printer := progress.New("troupe")
	printer.Out, printer.Err = e.out, e.err
	current := running()
	if devBuild || !release.MatchString("v"+current) {
		return fail(e, fmt.Errorf("this troupe (%s) is a build from a checkout, not a release: rebuild it from its checkout instead", current))
	}
	if err := installedHere(); err != nil {
		return fail(e, err)
	}
	latest, err := latestRelease(e.ctx)
	if err != nil {
		return fail(e, fmt.Errorf("could not read troupe's latest release: %v", err))
	}
	if newer, _ := published.Compare(latest, current); newer <= 0 {
		printer.Print("update", progress.OK, "troupe "+current+" is the latest")
		return 0
	}
	// sudo first: the installer asks for it only when units change, which would come midway.
	stopSudo, err := askSudo(e.ctx)
	if err != nil {
		return fail(e, err)
	}
	defer stopSudo()
	printer.Print("update", progress.Started, fmt.Sprintf("Updating troupe %s to %s with that release's installer", current, latest))
	return runInstaller(e, printer, "v"+latest)
}

// latestRelease is the newest release's version, read from where releases/latest redirects,
// without following it: no API, so no token and no rate limit.
func latestRelease(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, releases+"/latest", nil)
	if err != nil {
		return "", err
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	response.Body.Close()
	location := response.Header.Get("Location")
	tag := path.Base(location)
	if !strings.Contains(location, "/releases/tag/") || !release.MatchString(tag) {
		return "", fmt.Errorf("no release at %s/latest", releases)
	}
	return tag[1:], nil
}

// runInstaller reads the release's own install.sh and runs it, pointed at that release's files:
// the same installer, and the same trust, as the documented one-line install.
func runInstaller(e env, printer *progress.Printer, tag string) int {
	ctx, cancel := context.WithTimeout(e.ctx, time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, installScript(tag), nil)
	if err != nil {
		return fail(e, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fail(e, fmt.Errorf("could not download %s's installer: %v", tag, err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fail(e, fmt.Errorf("could not download %s's installer: %s", tag, response.Status))
	}
	script, err := io.ReadAll(io.LimitReader(response.Body, maxInstaller+1))
	if err != nil {
		return fail(e, fmt.Errorf("could not download %s's installer: %v", tag, err))
	}
	if len(script) > maxInstaller {
		return fail(e, fmt.Errorf("%s's installer is larger than %d bytes; nothing was run", tag, maxInstaller))
	}
	dir, err := os.MkdirTemp("", "troupe-update-")
	if err != nil {
		return fail(e, err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(file, script, 0o600); err != nil {
		return fail(e, err)
	}
	// Not tied to troupe's context: an interrupt reaches the installer from the terminal itself,
	// so its own traps put the old troupe back, as with curl | sh; killing it would skip them.
	cmd := exec.Command("sh", file)
	cmd.Env = append(os.Environ(), "TROUPE_RELEASE="+releases+"/download/"+tag)
	reader, writer := io.Pipe()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, writer, writer
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			// The installer's own lines become troupe's; the doctor's are already in its form.
			if line, ok := strings.CutPrefix(scanner.Text(), "troupe install: "); ok {
				printer.Print("update", progress.Info, line)
			} else {
				fmt.Fprintln(e.out, scanner.Text())
			}
		}
		io.Copy(io.Discard, reader)
	}()
	err = cmd.Run()
	writer.Close()
	<-done
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	} else if err != nil {
		return fail(e, err)
	}
	return 0
}

// releaseLine is the doctor's troupe line: a newer release is a note, so it never stops create.
func releaseLine(ctx context.Context) doctor.Line {
	current := running()
	if devBuild || !release.MatchString("v"+current) {
		return doctor.Line{Name: "troupe", Level: doctor.OK, Message: "troupe " + current + ", a build from a checkout, not checked for releases"}
	}
	latest, err := latestRelease(ctx)
	if err != nil {
		return doctor.Line{Name: "troupe", Level: doctor.Note, Message: fmt.Sprintf("could not check for a newer troupe: %v", err)}
	}
	if newer, _ := published.Compare(latest, current); newer > 0 {
		return doctor.Line{Name: "troupe", Level: doctor.Note, Message: fmt.Sprintf("troupe %s is out (this is %s): run troupe update", latest, current)}
	}
	return doctor.Line{Name: "troupe", Level: doctor.OK, Message: "troupe " + current + ", the latest"}
}
