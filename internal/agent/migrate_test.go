package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrateSbx extends the recreate fake: workspace streams, archive copies and the backup jobs.
const migrateSbx = `#!/bin/sh
d=$(dirname "$0")/..
case "$*" in
  *"-cpf -"*) echo "sbx workspace export" >> "$d/calls"; [ -f "$d/fail-export" ] && exit 1
    if [ -f "$d/bigws" ]; then yes | head -c 200000; exit 0; fi
    echo TARDATA; exit 0 ;;
  *"-tf -"*) cat > /dev/null; echo "sbx workspace check" >> "$d/calls"; exit 0 ;;
  *"-xpf -"*) cat > "$d/restored-workspace"; echo "sbx workspace restore" >> "$d/calls"; exit 0 ;;
esac
if [ "$1" = cp ]; then echo "sbx cp $2 $3" >> "$d/calls"; exit 0; fi
case "$*" in
  *" cat -- "*) echo "sbx stream out" >> "$d/calls"
    cat "$d/backup.zip"; [ -f "$d/tamper" ] && printf x; exit 0 ;;
  *'cat > "$1"'*) cat > /dev/null; echo "sbx stream in" >> "$d/calls"; exit 0 ;;
esac
if [ "$1" = exec ]; then
  case "$*" in
    *" fresh"*) cat > /dev/null; echo "job fresh" >> "$d/calls"
      # Like the real job: a VM something was restored into is no longer fresh.
      if [ -f "$d/restored-workspace" ]; then echo '{"status": "failed", "class": "workspace_not_empty"}'; exit 1; fi
      echo '{"status": "passed"}'; exit 0 ;;
    *" backup make"*) cat > /dev/null; echo "job backup make" >> "$d/calls"
      [ -f "$d/fail-backup" ] && { echo '{"status": "failed", "class": "native_backup_failed"}'; exit 1; }
      # The VM's own check refuses names the restore would not write exactly.
      [ -f "$d/external" ] && { echo '{"status": "failed", "class": "archive_external_path_not_allowed"}'; exit 1; }
      python3 -c 'import sys, zipfile; z = zipfile.ZipFile(sys.argv[1], "w"); z.writestr("state.db", "x"); z.close()' "$d/backup.zip"
      sum=$(sha256sum "$d/backup.zip" | cut -d" " -f1); size=$(stat -c %s "$d/backup.zip")
      echo "{\"status\": \"passed\", \"lock_existed\": false, \"commit\": \"0123456789abcdef0123456789abcdef01234567\", \"sha256\": \"$sum\", \"size\": $size}"; exit 0 ;;
  esac
fi
exec "$(dirname "$0")/sbx-recreate" "$@"
`

func newMigrateRig(t *testing.T) *rig {
	r := newRig(t)
	bin := filepath.Join(r.dir, "bin")
	os.Rename(filepath.Join(bin, "sbx"), filepath.Join(bin, "sbx-recreate"))
	os.WriteFile(filepath.Join(bin, "sbx"), []byte(migrateSbx), 0o755)
	r.agent.Command = "migrate"
	// A host with plenty of room unless a test says otherwise.
	r.agent.Free = func(string) (int64, error) { return 1 << 50, nil }
	return r
}

func t0() context.Context { return context.Background() }

func (r *rig) migrate() error {
	return r.agent.Migrate(t0(), r.opts)
}

func (r *rig) migration(name string) string {
	return filepath.Join(r.agent.State, "ada", "current", name)
}

func TestMigrateRunsEveryStepInOrder(t *testing.T) {
	r := newMigrateRig(t)
	if err := r.migrate(); err != nil {
		t.Fatalf("%v\n%s%s\ncalls:\n%s", err, r.out.String(), r.err.String(), r.calls())
	}
	r.order("systemctl stop collector.timer", "holder start", "job slack channel", "job drain",
		"systemctl stop hermes-gateway@ada.service", "job gateway-stopped", "job backup make", "sbx stream out",
		"job backup clean 0", "sbx workspace export", "sbx workspace check",
		"sbx rm", "holder stop", "created with 20g", "holder start", "job fresh",
		"job align 0123456789abcdef0123456789abcdef01234567", "sbx stream in", "job restore", "sbx workspace restore",
		"sbx secret set-custom", "job slack move", "systemctl enable hermes-gateway@ada.service",
		"systemctl start hermes-gateway@ada.service", "job ready --platform slack", "holder stop",
		"job selftest", "systemctl start collector.timer")
	// A move checks the VM, not the Slack app: no scope or boundary probes (the guest's setup).
	if strings.Contains(r.calls(), "job slack setup") {
		t.Fatalf("a move ran the creation's Slack probes:\n%s", r.calls())
	}
	// No second backup after the move: the quiesced pre-move pair is the restore point.
	if strings.Count(r.calls(), "job backup make") != 1 {
		t.Fatalf("backups:\n%s", r.calls())
	}
	if strings.Contains(r.calls(), "kit setup.py") || strings.Contains(r.calls(), "seed-check") {
		t.Fatalf("a migrate ran the kit's creation steps:\n%s", r.calls())
	}
	// The long restore step says what it is doing.
	for _, line := range []string{"Ada restore: Updating Hermes in the new VM", "Ada restore: Restoring the workspace.", "Ada restore: Starting the gateway."} {
		if !strings.Contains(r.out.String(), line) {
			t.Fatalf("missing %q in:\n%s", line, r.out.String())
		}
	}
	if data, _ := os.ReadFile(filepath.Join(r.dir, "restored-workspace")); string(data) != "TARDATA\n" {
		t.Fatalf("restored workspace %q", data)
	}
	if updates, _ := filepath.Glob(filepath.Join(r.dir, "ada-updates", "*")); len(updates) != 0 {
		t.Fatalf("a second backup %v", updates)
	}
	done := r.completedMigrations()
	if len(done) != 1 {
		t.Fatalf("completed %v", done)
	}
	// The check's receipt: the selftest turn on this VM's gateway.
	data, _ := os.ReadFile(filepath.Join(r.agent.State, "ada", "completed", done[0], "live.json"))
	if !strings.Contains(string(data), `"execution_route":"selftest"`) || !strings.Contains(string(data), `"status":"passed"`) {
		t.Fatalf("live.json %s", data)
	}
}

func (r *rig) completedMigrations() []string {
	entries, _ := os.ReadDir(filepath.Join(r.agent.State, "ada", "completed"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFailedBackupSetsTheAttemptAsideAndRestartsTheOriginal(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-backup", "")
	if err := r.migrate(); err == nil {
		t.Fatal("passed")
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("removed after a failed backup:\n%s", r.calls())
	}
	r.order("systemctl stop hermes-gateway@ada.service", "job backup make", "job drain cancel",
		"systemctl start hermes-gateway@ada.service")
	if aborted, _ := os.ReadDir(filepath.Join(r.agent.State, "ada", "aborted")); len(aborted) != 1 {
		t.Fatalf("aborted %v", aborted)
	}
	if _, err := os.Stat(r.migration("")); err == nil {
		t.Fatal("the failed attempt stayed current")
	}
}

func TestFailedDrainNeverStopsTheGateway(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-drain", "")
	if err := r.migrate(); err == nil {
		t.Fatal("passed")
	}
	if strings.Contains(r.calls(), "systemctl stop hermes-gateway") || strings.Contains(r.calls(), "backup make") {
		t.Fatalf("stopped or backed up after a failed drain:\n%s", r.calls())
	}
}

// An earlier run's backup is never used: a run killed after its backup (no cleanup ran) leaves
// it in current/, and the next run that finds the original sets it aside and takes a new one.
func TestOnlyThisRunsSnapshotAuthorizesRemoval(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-export", "")
	r.migrate() // backed up the state, then failed and set the attempt aside
	os.Remove(filepath.Join(r.dir, "fail-export"))
	aborted, _ := filepath.Glob(filepath.Join(r.agent.State, "ada", "aborted", "*"))
	// As if killed: the attempt, with its backup receipt, is still current.
	os.Rename(aborted[0], r.migration(""))
	os.WriteFile(r.migration("backup.json"), []byte(`{"schema":"quiesced-backup-v3","old_id":"old","sha256":"x","workspace_sha256":"y","hermes_commit":"0123456789abcdef0123456789abcdef01234567"}`), 0o600)
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.migrate(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	r.order("job backup make", "sbx workspace export", "sbx rm")
	if again, _ := filepath.Glob(filepath.Join(r.agent.State, "ada", "aborted", "*")); len(again) != 1 {
		t.Fatalf("the stale attempt was not set aside: %v", again)
	}
}

// A failure after the restore resumes: the new VM is not checked as empty again (it no longer
// is), and the restore steps run again over what they wrote.
func TestFailureAfterRestoreResumes(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-slack-move", "")
	if err := r.migrate(); err == nil {
		t.Fatal("passed")
	}
	if strings.Contains(r.calls(), "systemctl start hermes-gateway") {
		t.Fatalf("started a gateway after removal:\n%s", r.calls())
	}
	os.Remove(filepath.Join(r.dir, "fail-slack-move"))
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.migrate(); err != nil {
		t.Fatalf("resume: %v\n%s", err, r.calls())
	}
	for _, step := range []string{"job drain", "sbx rm", "sbx env create", "job fresh"} {
		if strings.Contains(r.calls(), step) {
			t.Fatalf("the resumed run repeated %s:\n%s", step, r.calls())
		}
	}
	r.order("job restore", "job slack move", "systemctl start hermes-gateway@ada.service")
}

func TestActivationRetryStartsWithoutRestoring(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-selftest", "")
	if err := r.migrate(); err == nil {
		t.Fatal("passed")
	}
	os.Remove(filepath.Join(r.dir, "calls"))
	os.Remove(filepath.Join(r.dir, "fail-selftest"))
	if err := r.migrate(); err != nil {
		t.Fatalf("resume: %v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "job restore") || strings.Contains(r.calls(), "job align") {
		t.Fatalf("restored again after activation:\n%s", r.calls())
	}
	r.order("systemctl start hermes-gateway@ada.service", "job ready", "job selftest")
}

// A move that fails its check never finishes: the transaction stays open for a rerun.
func TestAFailedCheckNeverFinishes(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-selftest", "")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "planted_failure") {
		t.Fatalf("err %v", err)
	}
	if r.completedMigrations() != nil {
		t.Fatal("finished without a passing check")
	}
}

func TestChangedArchiveIsRefused(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-slack-move", "")
	r.migrate()
	os.WriteFile(r.migration("workspace.tar.zst"), []byte("changed"), 0o600)
	os.Remove(filepath.Join(r.dir, "fail-slack-move"))
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "archive_digest_changed") {
		t.Fatalf("err %v", err)
	}
}

func TestAMissingVMNeedsThisBackupsRemovalReceipt(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-export", "")
	r.migrate()
	os.Remove(filepath.Join(r.dir, "fail-export"))
	r.write("inventory.json", `[]`)
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.migrate(); err == nil {
		t.Fatal("created a VM without a backup")
	}
	if strings.Contains(r.calls(), "sbx env create") {
		t.Fatalf("created:\n%s", r.calls())
	}
}

func TestAnOpenRecreateBlocksAMigrate(t *testing.T) {
	r := newMigrateRig(t)
	os.MkdirAll(filepath.Join(r.agent.State, "ada", "reseed", "current"), 0o700)
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "recreate_transaction_pending") {
		t.Fatalf("err %v", err)
	}
}

// Nothing the agent's VM prepares can reach the host as a file system object: archives cross
// only as bytes, never through a copy tool that recreates links, hardlinks or folders.
func TestArchivesCrossOnlyAsBytes(t *testing.T) {
	r := newMigrateRig(t)
	if err := r.migrate(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "sbx cp") {
		t.Fatalf("a copy tool wrote on the host:\n%s", r.calls())
	}
	done := r.completedMigrations()
	info, err := os.Lstat(filepath.Join(r.agent.State, "ada", "completed", done[0], "state.zip"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("state.zip %v %v", info, err)
	}
}

// shrinkingFree reports room for a whole archive pair on the first reading (the check before
// downtime), then only after bytes above the reserve.
func shrinkingFree(after int64) func(string) (int64, error) {
	first := true
	return func(string) (int64, error) {
		if first {
			first = false
			return reserve + 2*(20<<30), nil
		}
		return after, nil
	}
}

// Failed attempts are set aside without their archives: retries cannot pile the VM's data up.
func TestASetAsideAttemptKeepsNoArchives(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-export", "")
	r.migrate()
	kept, _ := filepath.Glob(filepath.Join(r.agent.State, "ada", "aborted", "*", "*"))
	for _, path := range kept {
		if strings.Contains(path, "state.zip") || strings.Contains(path, "workspace.tar.zst") {
			t.Fatalf("kept %s", path)
		}
	}
	if len(kept) == 0 {
		t.Fatal("kept no receipts either")
	}
}

// The VM decides how much it sends: every stream stops at the room left above the reserve, and
// a migrate without room refuses before any downtime.
func TestStreamsFromTheVMAreBounded(t *testing.T) {
	r := newMigrateRig(t)
	r.agent.Free = func(string) (int64, error) { return reserve, nil }
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "not_enough_free_space") {
		t.Fatalf("no room: %v", err)
	}
	if strings.Contains(r.calls(), "job drain") {
		t.Fatalf("drained without room:\n%s", r.calls())
	}

	// Room for the pair at first, then almost none: each stream still stops at what is left.
	r = newMigrateRig(t)
	r.agent.Free = shrinkingFree(reserve + 50)
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "backup_larger_than_allowed") {
		t.Fatalf("oversized backup: %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("removed after an oversized backup:\n%s", r.calls())
	}

	r = newMigrateRig(t)
	r.agent.Free = shrinkingFree(reserve + 10000)
	r.write("bigws", "")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "workspace_larger_than_allowed") {
		t.Fatalf("flooding workspace: %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("removed after a flooding workspace:\n%s", r.calls())
	}
	if partial, _ := filepath.Glob(filepath.Join(r.agent.State, "ada", "aborted", "*", "*.partial")); len(partial) != 0 {
		t.Fatalf("kept partial streams: %v", partial)
	}
}

// State outside the workspace in _external/ (persistence surviving a reset) is refused by the
// VM's own check, and the migrate stops before the old VM is removed.
func TestExternalStateOutsideTheWorkspaceIsRefusedBeforeRemoval(t *testing.T) {
	r := newMigrateRig(t)
	r.write("external", "")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "archive_external_path_not_allowed") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("removed:\n%s", r.calls())
	}
}

// The bytes the host stored must be exactly the ones the VM hashed: a stream cut short or
// changed on the way never authorizes a removal.
func TestAnArchiveChangedInTransitIsRefused(t *testing.T) {
	r := newMigrateRig(t)
	r.write("tamper", "")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "archive_changed_in_transit") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "sbx rm") {
		t.Fatalf("removed:\n%s", r.calls())
	}
}

// The host never parses an archive from a VM: every parser is attack surface (a crafted zip
// index once made Go's reader load millions of entries), and one that read names differently
// from the restore's could be fooled. Content checks run in the VMs, with the restore's parser.
func TestTheHostNeverParsesVMArchives(t *testing.T) {
	// Every host package that handles data from a VM; internal/guest writes its own job files
	// into a tar for the VM and never reads one back.
	var files []string
	for _, dir := range []string{".", "../cli", "../sbx", "../secret", "../guard", "../registry", "../config",
		"../progress", "../..", "../../cmd/troupe"} {
		found, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		files = append(files, found...)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(file)
		for _, parser := range []string{`"archive/zip"`, `"archive/tar"`, `"compress/`} {
			if strings.Contains(string(data), parser) {
				t.Errorf("%s imports %s", file, parser)
			}
		}
	}
}

// After removal was authorized, a failure that still finds the old VM listed (sbx rm cut off
// mid-way) keeps the attempt's archives: they may be the agent's only copy.
func TestArchivesAreKeptOnceRemovalWasAuthorized(t *testing.T) {
	r := newMigrateRig(t)
	// The removal "fails" with the old VM still listed, after the authorization.
	r.write("fail-rm", "")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "removal_failed") {
		t.Fatalf("err %v", err)
	}
	// Left in place for the next run, never moved aside or restarted here.
	if _, err := os.Stat(r.migration("state.zip")); err != nil {
		t.Fatalf("the archives of an authorized removal were not left in place: %v", err)
	}
	if strings.Contains(r.calls(), "systemctl start hermes-gateway") {
		t.Fatalf("restarted a VM whose removal was under way:\n%s", r.calls())
	}
}

// A failure before the removal is authorized never starts a gateway that was stopped before
// the migrate: the recovery restores only what the run itself stopped.
func TestMigrateFailureLeavesAStoppedAgentStopped(t *testing.T) {
	r := newMigrateRig(t)
	r.stopped("stopped")
	r.write("fail-backup", "")
	if err := r.migrate(); err == nil {
		t.Fatal("passed")
	}
	if !strings.Contains(r.calls(), "systemctl stop hermes-gateway") {
		t.Fatalf("failed before the gateway stop, so the recovery was never exercised:\n%s", r.calls())
	}
	// Only the restart depends on the gateway: the attempt is still set aside, so a later run
	// starts clean.
	if aborted, _ := os.ReadDir(filepath.Join(r.agent.State, "ada", "aborted")); len(aborted) != 1 {
		t.Fatalf("aborted %v", aborted)
	}
	if _, err := os.Stat(r.migration("")); err == nil {
		t.Fatal("the failed attempt stayed current")
	}
	if strings.Contains(r.calls(), "systemctl start hermes-gateway") {
		t.Fatalf("started the stopped agent's gateway:\n%s", r.calls())
	}
}

// A migrate proves its kits with sbx env plan before any change, and an open migration an older
// troupe recorded (replacement-v2) is never resumed.
func TestMigratePlansAndRefusesAnOlderReceipt(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-create", "")
	if err := r.migrate(); err == nil {
		t.Fatal("passed")
	}
	r.order("sbx env plan "+r.opts.Kit.sbxenv, "docker build coder -f coder.yaml --output type=cacheonly",
		"docker build ada -f ada.yaml --output type=cacheonly", "job drain", "sbx rm")
	data, err := os.ReadFile(r.migration("preflight.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.migration("preflight.json"), []byte(strings.Replace(string(data), "replacement-v3", "replacement-v2", 1)), 0o600)
	os.Remove(filepath.Join(r.dir, "fail-create"))
	os.Remove(filepath.Join(r.dir, "calls"))
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "receipt_schema_outdated") {
		t.Fatalf("err %v\n%s", err, r.calls())
	}
	if strings.Contains(r.calls(), "sbx env create") || strings.Contains(r.calls(), "job") {
		t.Fatalf("resumed an older migration:\n%s", r.calls())
	}
}

// A kit that does not build stops a migrate before its backup or any removal.
func TestMigrateStopsOnAFailingKitBuild(t *testing.T) {
	r := newMigrateRig(t)
	r.write("fail-build-ada", "")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "kit_build_failed: ada") {
		t.Fatalf("err %v\n%s", err, r.calls())
	}
	for _, step := range []string{"job drain", "backup make", "systemctl stop hermes-gateway", "sbx rm", "sbx env create"} {
		if strings.Contains(r.calls(), step) {
			t.Fatalf("%s ran after a failed build:\n%s", step, r.calls())
		}
	}
}

// A migrate without the docker CLI is refused before anything changes.
func TestMigrateNeedsTheDockerCLI(t *testing.T) {
	r := newMigrateRig(t)
	r.withoutDocker()
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "docker_cli_missing") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "systemctl stop") || strings.Contains(r.calls(), "job") {
		t.Fatalf("acted:\n%s", r.calls())
	}
}

// A resize is a migrate whose lines say resize.
func TestAResizeSaysSo(t *testing.T) {
	r := newMigrateRig(t)
	r.agent.Command = "resize"
	if err := r.migrate(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	out := r.out.String()
	if !strings.Contains(out, "Ada resize: started. Moving Ada into a new VM with its state") || !strings.Contains(out, "Ada resize: ok") || strings.Contains(out, "Ada migrate:") {
		t.Fatalf("output:\n%s", out)
	}
}

// A resize removes the replaced VM's image once the new VM runs and its receipts are filed.
func TestAResizeRemovesTheReplacedVMsImage(t *testing.T) {
	r := newMigrateRig(t)
	r.write("image-ada", "sandbox-kit-assembled:old")
	r.write("image-grace", "sandbox-kit-assembled:grace")
	if err := r.migrate(); err != nil {
		t.Fatalf("%v\n%s", err, r.calls())
	}
	r.order("sbx rm", "created with 20g", "job ready --platform slack", "job selftest", "sbx template template rm --force sandbox-kit-assembled:old")
}

// The room a resize needs is what the VM's root filesystem holds, not its declared size: a large
// disk with little on it can still move. The VM's answer only lowers the bar within the declared
// disk; a missing or absurd one counts the declared disk.
func TestRoomForAPairCountsWhatTheRootHolds(t *testing.T) {
	declared := 100
	for name, c := range map[string]struct {
		answer string // the disk job's answer; "" is the fake's default, with no used_bytes
		ok     bool
	}{
		"10 GiB held":          {`{"step": "disk", "status": "passed", "used_bytes": 10737418240}`, true},
		"20 GiB held":          {`{"step": "disk", "status": "passed", "used_bytes": 21474836480}`, false},
		"no answer":            {"", false},
		"more than declared":   {`{"step": "disk", "status": "passed", "used_bytes": 1099511627776}`, false},
		"a failed job":         {`{"step": "disk", "status": "failed"}`, false},
		"a negative used size": {`{"step": "disk", "status": "passed", "used_bytes": -1}`, false},
		"past int64":           {`{"step": "disk", "status": "passed", "used_bytes": 1e30}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newMigrateRig(t)
			r.opts.Kit.RootGiB = declared
			// Room for two 11 GiB archives above the reserve, never for two declared disks.
			r.agent.Free = func(string) (int64, error) { return reserve + 30<<30, nil }
			if c.answer != "" {
				r.write("answer-disk", c.answer+"\n")
			}
			err := r.migrate()
			refused := err != nil && strings.Contains(err.Error(), "not_enough_free_space")
			if refused == c.ok {
				t.Fatalf("err %v\n%s", err, r.calls())
			}
			if refused && strings.Contains(r.calls(), "job drain") {
				t.Fatalf("drained without room:\n%s", r.calls())
			}
		})
	}
}

// A stopped agent's VM is never asked how much it holds: the exec would boot it, and a refusal for
// space would leave it running. Its declared disk is counted instead.
func TestRoomForAPairNeverBootsAStoppedVM(t *testing.T) {
	r := newMigrateRig(t)
	r.stopped("stopped")
	r.opts.Kit.RootGiB = 100
	r.agent.Free = func(string) (int64, error) { return reserve + 30<<30, nil }
	r.write("answer-disk", `{"step": "disk", "status": "passed", "used_bytes": 1024}`+"\n")
	if err := r.migrate(); err == nil || !strings.Contains(err.Error(), "not_enough_free_space") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(r.calls(), "job disk") {
		t.Fatalf("asked a stopped VM:\n%s", r.calls())
	}
}
