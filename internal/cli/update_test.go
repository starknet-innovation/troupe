package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starknet-innovation/troupe/internal/doctor"
)

// fakeReleases serves releases/latest as GitHub does (a redirect to the latest tag) and each
// tag's install.sh, which records the TROUPE_RELEASE it ran with and exits with exit.
func fakeReleases(t *testing.T, latest, exit string) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "ran")
	// sudo that needs no password, and this test's troupe taken as the installed one.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest" && latest != "":
			w.Header().Set("Location", "https://github.example/o/troupe/releases/tag/"+latest)
			w.WriteHeader(http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/install.sh"):
			if exit == "huge" {
				w.Write([]byte(strings.Repeat("#", maxInstaller+1)))
				return
			}
			w.Write([]byte("echo \"$TROUPE_RELEASE\" > " + record + "\necho 'troupe install: installed troupe 0.3.0'\nexit " + exit + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	saved, savedScript, savedRunning, savedHere := releases, installScript, running, installedHere
	releases = server.URL + "/releases"
	installScript = func(tag string) string { return server.URL + "/" + tag + "/install.sh" }
	installedHere = func() error { return nil }
	t.Cleanup(func() { releases, installScript, running, installedHere = saved, savedScript, savedRunning, savedHere })
	return record
}

func TestLatestReleaseReadsTheRedirect(t *testing.T) {
	fakeReleases(t, "v0.3.0", "0")
	if v, err := latestRelease(context.Background()); err != nil || v != "0.3.0" {
		t.Fatalf("%q %v", v, err)
	}
	fakeReleases(t, "nightly", "0")
	if _, err := latestRelease(context.Background()); err == nil {
		t.Fatal("a tag that is no release passed")
	}
	fakeReleases(t, "", "0")
	if _, err := latestRelease(context.Background()); err == nil {
		t.Fatal("no redirect passed")
	}
}

// The doctor's troupe line: a newer release is a note naming troupe update, never in the way.
func TestReleaseLine(t *testing.T) {
	for name, c := range map[string]struct {
		running, latest string
		level           doctor.Level
		want            string
	}{
		"the latest":      {"0.3.0", "v0.3.0", doctor.OK, "the latest"},
		"newer is out":    {"0.2.0", "v0.3.0", doctor.Note, "troupe 0.3.0 is out (this is 0.2.0): run troupe update"},
		"unreachable":     {"0.2.0", "", doctor.Note, "could not check"},
		"a checkout":      {"1390d19", "v0.3.0", doctor.OK, "not checked"},
		"ahead of latest": {"0.4.0", "v0.3.0", doctor.OK, "the latest"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeReleases(t, c.latest, "0")
			running = func() string { return c.running }
			if devBuild && c.running != "1390d19" {
				t.Skip("a dev build is never checked")
			}
			l := releaseLine(context.Background())
			if l.Level != c.level || !strings.Contains(l.Message, c.want) {
				t.Fatalf("%+v", l)
			}
		})
	}
}

// update runs the latest release's own installer, pointed at that release's files, and exits
// as it does; on the latest it does nothing.
func TestUpdateRunsTheReleasesInstaller(t *testing.T) {
	if devBuild {
		t.Skip("a dev build refuses update")
	}
	record := fakeReleases(t, "v0.3.0", "0")
	running = func() string { return "0.3.0" }
	if code, out, _ := run("update"); code != 0 || !strings.Contains(out, "troupe 0.3.0 is the latest") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("ran an installer on the latest")
	}
	running = func() string { return "0.2.0" }
	code, out, errs := run("update")
	if code != 0 || !strings.Contains(out, "Updating troupe 0.2.0 to 0.3.0") || !strings.Contains(out, "troupe update: installed troupe 0.3.0.") {
		t.Fatalf("exit %d: %s%s", code, out, errs)
	}
	if ran, _ := os.ReadFile(record); strings.TrimSpace(string(ran)) != releases+"/download/v0.3.0" {
		t.Fatalf("TROUPE_RELEASE %q", ran)
	}
	fakeReleases(t, "v0.3.0", "3")
	running = func() string { return "0.2.0" }
	if code, _, _ := run("update"); code != 3 {
		t.Fatalf("installer failure: exit %d", code)
	}
	fakeReleases(t, "v0.3.0", "huge")
	running = func() string { return "0.2.0" }
	if code, _, errs := run("update"); code == 0 || !strings.Contains(errs, "larger than") {
		t.Fatalf("oversized installer: exit %d: %s", code, errs)
	}
	fakeReleases(t, "v0.3.0", "0")
	running = func() string { return "0.2.0" }
	installedHere = func() error { return errors.New("this troupe runs from /elsewhere") }
	if code, _, errs := run("update"); code == 0 || !strings.Contains(errs, "runs from /elsewhere") {
		t.Fatalf("not the installed troupe: exit %d: %s", code, errs)
	}
	running = func() string { return "1390d19 modified" }
	if code, _, errs := run("update"); code == 0 || !strings.Contains(errs, "not a release") {
		t.Fatalf("checkout: exit %d: %s", code, errs)
	}
}

// In the dev build, update refuses, and the maintainer command it used to be points at reseed.
func TestADevBuildRefusesUpdate(t *testing.T) {
	if !devBuild {
		t.Skip("release build")
	}
	if code, _, errs := run("update"); code == 0 || !strings.Contains(errs, "not a release") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, args := range [][]string{{"update", "skills", "ada"}, {"update", "soul", "ada", "--dry-run"}} {
		if code, _, errs := run(args...); code != 2 || !strings.Contains(errs, "now troupe reseed "+strings.Join(args[1:], " ")) {
			t.Fatalf("%v: exit %d: %s", args, code, errs)
		}
	}
}
