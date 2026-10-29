package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

const migrateMode = "migrate"

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// reserve is the free space a migrate never writes into: a flood from the VM stops well before
// the host's disk, which sandboxd and every other agent's VM share and grow into, is full.
const reserve = 50 << 30

// enoughForAPair: room for one state and one workspace archive above the reserve, checked before
// any downtime. Each archive is at most what the root filesystem holds, as the VM reports it, plus
// a tenth; a VM with a large disk but little on it can then still be resized. The VM's answer is
// not trusted for the host's safety (room still caps each stream as it is written), and a
// missing or absurd answer counts the declared root disk, as before.
func (r *recreation) enoughForAPair(ctx context.Context, folder string) (bool, error) {
	free, err := r.free(folder)
	if err != nil {
		return false, nil
	}
	archive := int64(r.o.Kit.RootGiB) << 30
	// A stopped VM is not asked: an exec would boot it, and a refusal would leave it running.
	if r.vmStopped(ctx) {
		return free >= reserve+2*archive, nil
	}
	result, err := r.call(ctx, 60*time.Second, "disk")
	if errors.Is(err, ErrInterrupted) {
		return false, err
	}
	// Compared as a float first: a number past int64 would wrap when converted.
	if used, ok := result["used_bytes"].(float64); err == nil && result["status"] == "passed" && ok && used > 0 && used <= float64(archive) {
		archive = int64(used) + int64(used)/10
	}
	return free >= reserve+2*archive, nil
}

// room is how many bytes a stream from the VM may write under folder: never more than the
// agent's declared root disk (a VM cannot hold more) nor into the reserve.
func (r *recreation) room(folder string) (int64, error) {
	free, err := r.free(folder)
	if err != nil {
		return 0, err
	}
	room := free - reserve
	if limit := int64(r.o.Kit.RootGiB) << 30; limit < room {
		room = limit
	}
	if room <= 0 {
		return 0, errors.New("not enough free space")
	}
	return room, nil
}

func (r *recreation) free(folder string) (int64, error) {
	if r.Free != nil {
		return r.Free(folder)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(folder, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Paths inside the VM the state archive travels through (the backup and restore jobs accept
// no others).
const (
	remoteBackup  = "/home/agent/.local/state/hermes-replacement-backup.zip"
	remoteRestore = "/home/agent/.local/state/hermes-replacement-state.zip"
)

// The workspace travels as a tar stream; project files are never unpacked on the host.
const (
	workspaceExport  = `set -eu; test -d /home/agent/workspace; tar --zstd -cpf - -C /home/agent/workspace .`
	workspaceCheck   = `set -eu; tar --zstd -tf - >/dev/null`
	workspaceRestore = `set -eu
cd /home/agent
target=/home/agent/workspace
test ! -L "$target"
! mountpoint -q "$target"
mkdir -p "$target"
# A holder's shell may already have this folder as its working directory: keep its inode.
find "$target" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
tar --zstd -xpf - --no-same-owner -C "$target"`
)

type backupReceipt struct {
	Schema          string `json:"schema"`
	CreatedAt       string `json:"created_at"`
	OldID           string `json:"old_id"`
	SHA256          string `json:"sha256"`
	WorkspaceSHA256 string `json:"workspace_sha256"`
	HermesCommit    string `json:"hermes_commit"`
}

// liveReceipt is the proof that the moved agent works from the new VM (its selftest turn).
type liveReceipt struct {
	Schema           string `json:"schema"`
	Status           string `json:"status"`
	ExitStatus       int    `json:"exit_status"`
	Route            string `json:"execution_route"`
	Step             string `json:"step"`
	FailureClass     string `json:"failure_class"`
	NewID            string `json:"new_id,omitempty"`
	GatewaySignature string `json:"gateway_signature,omitempty"`
}

// Migrate moves the agent into a new VM and keeps his state: a quiesced native backup and a
// workspace archive of the old VM, its removal (authorized only by this run's own backup), a
// new VM built from its kits, Hermes brought up to the preserved commit, the state and
// workspace restored, secrets and Slack wired, and the gateway started; one model turn through
// the moved agent then proves it works. The quiesced pre-move pair stays with the receipts as
// the restore point. Every step leaves a receipt; an interrupted run resumes. A failure before removal sets
// the attempt aside and starts the old gateway again when this run stopped it; after removal
// nothing is restored again.
func (a *Agent) Migrate(ctx context.Context, o RecreateOptions) (err error) {
	r := &recreation{Agent: a, o: o}
	base := filepath.Join(a.stateRoot(), a.Name)
	r.t = transaction{base: base, current: filepath.Join(base, "current"), completed: filepath.Join(base, "completed")}
	r.label = "resize"
	if o.Label != "" {
		r.label = o.Label
	}
	a.Printer.Print(r.label, progress.Started, fmt.Sprintf("Moving %s into a new VM with its state", a.Printer.Agent))
	s, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	defer func() {
		r.finishMigrate(&err)
		if err != nil && ctx.Err() != nil {
			err = ErrInterrupted
		}
	}()
	if err := r.migratePreflight(ctx); err != nil {
		return err
	}
	records := r.changing()
	if records == nil {
		var err error
		if records, err = r.Runner.Inventory(ctx); err != nil {
			return Failure{"preflight", "inventory_unreadable"}
		}
	}
	current := records[r.Name].ID
	if current != "" && current == r.pre.OldID {
		if err := r.step(ctx, "backup", r.backup); err != nil {
			return err
		}
	}
	if err := r.verifyArchives(); err != nil {
		return err
	}
	switch {
	case current != "" && current == r.pre.OldID:
		if err := r.step(ctx, "removal", r.removeForMigrate); err != nil {
			return err
		}
		if err := r.step(ctx, "creation", r.createForMigrate); err != nil {
			return err
		}
	case current == "":
		if err := r.step(ctx, "creation", r.createForMigrate); err != nil {
			return err
		}
	default:
		var cutover struct {
			NewID string `json:"new_id"`
		}
		if !readReceipt(r.t.file("cutover.json"), &cutover) || cutover.NewID != current {
			return Failure{r.label, "unexpected_vm_identity"}
		}
	}
	if r.t.has("activation-attempted.json") {
		// A start may have succeeded although its caller failed before the next receipt:
		// resume the current state only, never restore again.
		if err := r.resumeActivation(ctx); err != nil {
			return err
		}
	} else if err := r.step(ctx, "restore", r.restoreState); err != nil {
		return err
	}
	r.gatewayStopped = false
	if !r.liveCurrent() {
		if err := r.step(ctx, "check", r.moveCheck); err != nil {
			return err
		}
	}
	if err := r.restoreCollector(); err != nil {
		return Failure{r.label, "collector_restore_failed"}
	}
	// The quiesced pre-move pair (backup.json) stays with the receipts as the move's restore
	// point; no second backup is taken.
	if !r.t.has("finalized.json") {
		var cutover struct {
			NewID string `json:"new_id"`
		}
		readReceipt(r.t.file("cutover.json"), &cutover)
		if err := writeReceipt(r.t.file("finalized.json"), map[string]any{"finalized_at": now(), "new_id": cutover.NewID}); err != nil {
			return err
		}
	}
	var finalized struct {
		At string `json:"finalized_at"`
	}
	readReceipt(r.t.file("finalized.json"), &finalized)
	var authorized struct {
		OldImage string `json:"old_image"`
	}
	readReceipt(r.t.file("removal-authorized.json"), &authorized)
	if err := r.moveToCompleted(finalized.At); err != nil {
		return err
	}
	done := filepath.Join(r.t.completed, finalized.At)
	r.Printer.Print(r.label, progress.OK, "Done. Receipts: "+done)
	// Housekeeping after the agent is back: the replaced VM's image, logged with the receipts.
	if authorized.OldImage != "" {
		r.Runner.Log = filepath.Join(done, "native.log")
		if r.removeImage(ctx, r.label, authorized.OldImage) {
			r.Printer.Print(r.label, progress.Info, "Removed the replaced VM's image "+authorized.OldImage)
		}
	}
	return nil
}

func (r *recreation) migratePreflight(ctx context.Context) error {
	r.Printer.Print("preflight", progress.Started, "")
	if os.Geteuid() == 0 {
		return Failure{"preflight", "run_as_your_own_user"}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return Failure{"preflight", "docker_cli_missing"}
	}
	if _, err := os.Stat(filepath.Join(r.t.base, "reseed", "current")); err == nil {
		return Failure{"preflight", "recreate_transaction_pending"}
	}
	if guard.Suspended(r.Paths, r.Name) {
		return Failure{"preflight", "suspended_by_a_phase_record"}
	}
	if err := r.o.Secrets.Sources(); err != nil {
		return Failure{"preflight", "secret_" + err.Error() + "; troupe secret check " + r.Name}
	}
	if err := r.o.Units(); err != nil {
		return Failure{"preflight", "units_not_installed: " + err.Error()}
	}
	if err := r.archivePrevious(ctx); err != nil {
		return err
	}
	// A resumed run logs beside its receipts; a new one opens its folder only once its
	// preflight has passed, so a refused run leaves nothing that blocks the next one.
	if _, err := os.Stat(r.t.current); err == nil {
		if err := r.open(); err != nil {
			return err
		}
	}
	records, err := r.preflightInventory(ctx)
	if err != nil {
		return Failure{"preflight", "inventory_unreadable"}
	}
	if err := r.o.Secrets.NoGlobals(ctx); err != nil {
		return Failure{"preflight", err.Error()}
	}
	if err := r.pauseCollector(ctx); err != nil {
		return err
	}
	// The collector may have run a copy while the timer was stopping: list again from here.
	r.listing = nil
	if !readReceipt(r.t.file("preflight.json"), &r.pre) {
		if err := r.recordMigratePreflight(ctx); err != nil {
			return err
		}
	}
	// Any run that still finds the original takes a new snapshot: an earlier one cannot be
	// proven quiesced to the end (a kill or a reboot may have resumed the gateway).
	if records[r.Name].ID != "" && records[r.Name].ID == r.pre.OldID && r.t.has("backup.json") {
		if err := r.setAside(); err != nil {
			return Failure{"preflight", "earlier_attempt_not_set_aside"}
		}
		if err := r.recordMigratePreflight(ctx); err != nil {
			return err
		}
	}
	if err := r.currentSchema("preflight"); err != nil {
		return err
	}
	if r.pre.Mode != migrateMode || r.pre.Agent != r.Name {
		return Failure{"preflight", "receipt_from_another_transaction; review it by hand"}
	}
	if r.pre.Declarations != r.o.Kit.Declarations {
		return Failure{"preflight", "declarations_changed"}
	}
	if err := r.prebuildBeforeRemoval(ctx); err != nil {
		return err
	}
	r.Printer.Print("preflight", progress.OK, "")
	return nil
}

func (r *recreation) recordMigratePreflight(ctx context.Context) error {
	records, err := r.preflightInventory(ctx)
	if err != nil {
		return Failure{"preflight", "inventory_unreadable"}
	}
	record, exists := records[r.Name]
	if !exists {
		return Failure{"preflight", "sandbox_missing; troupe create makes an agent's first VM"}
	}
	if record.Status != "running" && record.Status != "stopped" {
		return Failure{"preflight", "sandbox_unavailable"}
	}
	if !r.active() && !r.vmStopped(ctx) {
		if result, err := r.call(ctx, 60*time.Second, "gateway-stopped"); err != nil || result["status"] != "passed" {
			return Failure{"preflight", "gateway_stop_unproven"}
		}
	}
	if err := r.plan(ctx, "preflight"); err != nil {
		return err
	}
	if err := r.prebuild(ctx, "preflight"); err != nil {
		return err
	}
	pre := preflightReceipt{Schema: receiptSchema, RecordedAt: now(), Agent: r.Name, Mode: migrateMode,
		OldID: record.ID, GatewayPID: r.mainPID(), TargetHermes: r.o.Kit.Hermes, Declarations: r.o.Kit.Declarations}
	if err := r.open(); err != nil {
		return err
	}
	if err := writeReceipt(r.t.file("preflight.json"), pre); err != nil {
		return err
	}
	r.pre = pre
	return nil
}

// setAside keeps an attempt that must not be reused as evidence under aborted/.
func (r *recreation) setAside() error {
	aborted := filepath.Join(r.t.base, "aborted")
	if err := os.MkdirAll(aborted, 0o700); err != nil {
		return err
	}
	destination := filepath.Join(aborted, fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), os.Getpid()))
	// Once removal was authorized, the old VM may be going or gone even if sbx still lists it:
	// that attempt's archives may be the agent's only copy, so they are kept.
	keepArchives := r.t.has("removal-authorized.json")
	if err := os.Rename(r.t.current, destination); err != nil {
		return err
	}
	// Otherwise the old VM is untouched and the attempt is never reused: keep its receipts and
	// log as evidence, not its archives, so failed attempts cannot pile up on the host's disk.
	for _, pattern := range []string{"state.zip*", "workspace.tar.zst*"} {
		if keepArchives {
			break
		}
		archives, _ := filepath.Glob(filepath.Join(destination, pattern))
		for _, archive := range archives {
			os.Remove(archive)
		}
	}
	if strings.HasPrefix(r.Runner.Log, r.t.current+string(filepath.Separator)) {
		r.Runner.Log = filepath.Join(destination, strings.TrimPrefix(r.Runner.Log, r.t.current+string(filepath.Separator)))
	}
	r.Printer.Print("recovery", progress.Info, "Kept the set-aside attempt at "+destination)
	r.snapshotFresh = false
	return nil
}

// backup quiesces the old VM and takes its state and workspace archives.
func (r *recreation) backup(ctx context.Context) error {
	if err := r.sameVM(ctx, r.pre.OldID, "backup"); err != nil {
		return err
	}
	// Room for the archives before any downtime.
	if enough, err := r.enoughForAPair(ctx, r.t.current); err != nil {
		return err
	} else if !enough {
		return Failure{"backup", "not_enough_free_space"}
	}
	if err := r.hold(ctx); err != nil {
		return err
	}
	if r.Slack {
		// The restored config is moved into the new VM as it is: it must still enable Slack
		// alone, or the new VM's Slack setup would refuse it after the removal.
		if result, err := r.call(ctx, 60*time.Second, "slack", "channel"); err != nil || result["status"] != "passed" {
			return Failure{"backup", "slack_not_the_only_channel"}
		}
	}
	// Current work finishes first; on failure the gateway keeps running.
	if err := r.Drain(ctx, 60); err != nil {
		if errors.Is(err, ErrInterrupted) {
			return err
		}
		return Failure{"backup", "drain_failed; the agent is still running"}
	}
	// Recovery starts the gateway again only when it was up (or restarting) right here: an
	// agent the operator had stopped stays stopped, as its VM would otherwise boot. Read live,
	// not from the receipt, because a resumed run follows whatever happened in between.
	r.gatewayWasUp = unitUp(r.Name)
	r.gatewayStopped = true
	if err := r.systemctl("stop", r.Unit()); err != nil {
		return Failure{"backup", "gateway_stop_failed"}
	}
	if err := r.guestStopped(ctx, "backup"); err != nil {
		return err
	}
	archive, workspace := r.t.file("state.zip"), r.t.file("workspace.tar.zst")
	commit, err := r.nativeBackup(ctx, archive, "backup")
	if err != nil {
		return err
	}
	if err := r.exportWorkspace(ctx, workspace, "backup"); err != nil {
		return err
	}
	if err := r.guestStopped(ctx, "backup"); err != nil {
		return Failure{"backup", "gateway_resumed_during_backup"}
	}
	stateSHA, err := fileSHA(archive)
	if err != nil {
		return err
	}
	workspaceSHA, err := fileSHA(workspace)
	if err != nil {
		return err
	}
	if err := writeReceipt(r.t.file("backup.json"), backupReceipt{Schema: "quiesced-backup-v3", CreatedAt: now(),
		OldID: r.pre.OldID, SHA256: stateSHA, WorkspaceSHA256: workspaceSHA, HermesCommit: commit}); err != nil {
		return err
	}
	r.snapshotFresh = true
	r.Printer.Print("backup", progress.Info, "Native backup and workspace archive verified")
	return nil
}

// guestStopped: the unit is down and no gateway runs inside the VM.
func (r *recreation) guestStopped(ctx context.Context, step string) error {
	if r.mainPID() != "0" {
		return Failure{step, "gateway_not_stopped"}
	}
	if result, err := r.call(ctx, 60*time.Second, "gateway-stopped"); err != nil || result["status"] != "passed" {
		return Failure{step, "gateway_not_stopped"}
	}
	return nil
}

// nativeBackup runs Hermes's own backup in the VM, which checks the archive there (entry CRCs,
// SQLite integrity, names the restore accepts), and streams it out. The host never parses an
// archive from a VM: any parser is attack surface, and one that read it differently from the
// restore's could be fooled. It only checks that the bytes it received are the ones the VM
// hashed, which catches a stream cut short or garbled on the way.
func (r *recreation) nativeBackup(ctx context.Context, destination, step string) (string, error) {
	partial := destination + ".partial"
	os.Remove(partial)
	out, err := guest.RunWithin(ctx, r.Runner, 30*time.Minute, r.Name, "backup", "make")
	if ctx.Err() != nil {
		return "", ErrInterrupted
	}
	var made map[string]any
	json.Unmarshal(lastLine(out), &made)
	if err != nil || made["status"] != "passed" {
		return "", Failure{step, Plain(made["class"])}
	}
	commit, _ := made["commit"].(string)
	existed, _ := made["lock_existed"].(bool)
	digest, _ := made["sha256"].(string)
	size, _ := made["size"].(float64)
	if !commitPattern.MatchString(commit) {
		return "", Failure{step, "hermes_commit_invalid"}
	}
	if !sha256Pattern.MatchString(digest) || size <= 0 {
		return "", Failure{step, "archive_digest_missing"}
	}
	// The VM is the agent's: only the archive's bytes cross to the host, streamed into a file
	// troupe creates itself. A copy tool would recreate whatever the VM holds at that path (a
	// link, a hardlink, a folder) on the host, where later steps would follow it.
	copyErr := r.streamOut(ctx, remoteBackup, partial)
	if errors.Is(copyErr, sbx.ErrTooLarge) {
		os.Remove(partial)
		r.call(ctx, 60*time.Second, "backup", "clean", map[bool]string{true: "1", false: "0"}[existed])
		return "", Failure{step, "backup_larger_than_allowed"}
	}
	lock := "0"
	if existed {
		lock = "1"
	}
	r.call(ctx, 60*time.Second, "backup", "clean", lock)
	if copyErr != nil {
		os.Remove(partial)
		return "", Failure{step, "backup_copy_failed"}
	}
	if err := plainFile(partial); err != nil {
		os.Remove(partial)
		return "", Failure{step, "backup_copy_not_a_plain_file"}
	}
	got, err := fileSHA(partial)
	info, statErr := os.Lstat(partial)
	if err != nil || statErr != nil || got != digest || info.Size() != int64(size) {
		os.Remove(partial)
		return "", Failure{step, "archive_changed_in_transit"}
	}
	return commit, os.Rename(partial, destination)
}

// streamOut writes the bytes of a file in the VM into a new private host file. cat reads the
// VM's file (a link there resolves inside the VM); the host side only ever writes bytes.
func (r *recreation) streamOut(ctx context.Context, remote, local string) error {
	limit, err := r.room(filepath.Dir(local))
	if err != nil {
		return err
	}
	file, err := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = r.Runner.Output(ctx, sbx.Call{Args: []string{"exec", r.Name, "cat", "--", remote}, Timeout: 30 * time.Minute,
		Stdout: file, Limit: limit})
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// streamIn writes a host file's bytes into a new private file in the VM.
func (r *recreation) streamIn(ctx context.Context, local, remote string) error {
	file, err := openPlain(local)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = r.Runner.Output(ctx, sbx.Call{Args: []string{"exec", "-i", r.Name, "sh", "-c", `umask 077; rm -f -- "$1"; cat > "$1"`, "sh", remote},
		Stdin: file, Timeout: 30 * time.Minute})
	return err
}

// plainFile accepts a regular file owned by this user, never following a link, and makes it
// private.
func plainFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || owner(info) != os.Getuid() {
		return errors.New("not a plain file of this user")
	}
	file, err := openPlain(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Chmod(0o600)
}

// openPlain opens a file for reading without following a link at its last component.
func openPlain(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func owner(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// exportWorkspace streams the VM's workspace into an archive and has the VM read it back.
func (r *recreation) exportWorkspace(ctx context.Context, destination, step string) error {
	partial := destination + ".partial"
	os.Remove(partial)
	limit, err := r.room(filepath.Dir(partial))
	if err != nil {
		return Failure{step, "not_enough_free_space"}
	}
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = r.Runner.Output(ctx, sbx.Call{Args: []string{"exec", r.Name, "sh", "-c", workspaceExport},
		Timeout: 30 * time.Minute, Stdout: file, Limit: limit})
	closeErr := file.Close()
	if errors.Is(err, sbx.ErrTooLarge) {
		os.Remove(partial)
		return Failure{step, "workspace_larger_than_allowed"}
	}
	if err != nil || closeErr != nil {
		os.Remove(partial)
		return Failure{step, "workspace_export_failed"}
	}
	if err := r.workspaceReadable(ctx, partial, step); err != nil {
		os.Remove(partial)
		return err
	}
	return os.Rename(partial, destination)
}

func (r *recreation) workspaceReadable(ctx context.Context, path, step string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := r.Runner.Output(ctx, sbx.Call{Args: []string{"exec", "-i", r.Name, "sh", "-c", workspaceCheck},
		Stdin: file, Timeout: 30 * time.Minute}); err != nil {
		return Failure{step, "workspace_archive_invalid"}
	}
	return nil
}

func fileSHA(path string) (string, error) {
	file, err := openPlain(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// verifyArchives proves both archives are this transaction's, unchanged and intact.
func (r *recreation) verifyArchives() error {
	var b backupReceipt
	if !readReceipt(r.t.file("backup.json"), &b) || b.OldID != r.pre.OldID || b.Schema != "quiesced-backup-v3" ||
		!commitPattern.MatchString(b.HermesCommit) {
		return Failure{"backup", "backup_receipt_missing"}
	}
	for path, want := range map[string]string{r.t.file("state.zip"): b.SHA256, r.t.file("workspace.tar.zst"): b.WorkspaceSHA256} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 {
			return Failure{"backup", "archive_missing_or_unsafe"}
		}
		if got, err := fileSHA(path); err != nil || got != want {
			return Failure{"backup", "archive_digest_changed"}
		}
	}
	r.backupSHA, r.commit = b.SHA256, b.HermesCommit
	return nil
}

// removeForMigrate authorizes the removal with this run's own backup and removes the old VM.
func (r *recreation) removeForMigrate(ctx context.Context) error {
	if !r.snapshotFresh {
		return Failure{"removal", "snapshot_not_from_this_run"}
	}
	if err := r.o.Secrets.Sources(); err != nil {
		return Failure{"removal", "secret_" + err.Error()}
	}
	// This run's backup proved no gateway runs inside, before and after its export; the unit
	// still being down shows the unit started none since.
	if r.mainPID() != "0" {
		return Failure{"removal", "gateway_not_stopped"}
	}
	// The image the old VM runs, removed once its replacement runs (removeReplaced).
	if err := writeReceipt(r.t.file("removal-authorized.json"), map[string]any{"authorized_at": now(),
		"old_id": r.pre.OldID, "backup_sha256": r.backupSHA, "old_image": r.image(ctx, r.Name)}); err != nil {
		return err
	}
	if err := r.sameVM(ctx, r.pre.OldID, "removal"); err != nil {
		return err
	}
	if _, err := r.Runner.Output(ctx, sbx.Call{Args: []string{"rm", r.Name, "--force"}, Timeout: 300 * time.Second}); err != nil {
		return Failure{"removal", "removal_failed"}
	}
	r.releaseHolder()
	if err := r.waitGone(ctx, true); err != nil {
		return err
	}
	return writeReceipt(r.t.file("removed.json"), map[string]any{"old_id": r.pre.OldID, "backup_sha256": r.backupSHA,
		"removed_at": now()})
}

// createForMigrate creates the new VM only after this backup's removal was authorized and proven.
func (r *recreation) createForMigrate(ctx context.Context) error {
	if guard.Suspended(r.Paths, r.Name) {
		return Failure{"creation", "suspended_by_a_phase_record"}
	}
	var authorized, removed struct {
		OldID     string `json:"old_id"`
		BackupSHA string `json:"backup_sha256"`
		RemovedAt string `json:"removed_at"`
	}
	if !readReceipt(r.t.file("removal-authorized.json"), &authorized) || authorized.OldID != r.pre.OldID ||
		authorized.BackupSHA != r.backupSHA {
		return Failure{"creation", "removal_not_authorized"}
	}
	if !readReceipt(r.t.file("removed.json"), &removed) || removed.OldID != r.pre.OldID ||
		removed.BackupSHA != r.backupSHA || removed.RemovedAt == "" {
		return Failure{"creation", "removal_receipt_missing"}
	}
	if r.t.has("cutover.json") || r.t.has("activation-attempted.json") {
		return Failure{"creation", "new_sandbox_missing_after_creation"}
	}
	records, err := r.Runner.Inventory(ctx)
	if err != nil {
		return Failure{"creation", "inventory_unreadable"}
	}
	if _, exists := records[r.Name]; exists || !guard.VMGone(r.Paths, r.Name) {
		return Failure{"creation", "unexpected_sandbox"}
	}
	return r.provision(ctx, "creation")
}

// restoreState checks the new VM, brings its Hermes up to the preserved commit, restores the
// state and workspace, wires the secrets and Slack, and starts the gateway.
func (r *recreation) restoreState(ctx context.Context) error {
	var cutover struct {
		NewID string `json:"new_id"`
	}
	if !readReceipt(r.t.file("cutover.json"), &cutover) {
		return Failure{"restore", "cutover_receipt_missing"}
	}
	if err := r.sameVM(ctx, cutover.NewID, "restore"); err != nil {
		return err
	}
	if r.stopHolder == nil {
		if err := r.hold(ctx); err != nil {
			return err
		}
	}
	// No gateway may run while state is restored: the unit is down, and every restore job
	// (fresh, align, restore, slack) refuses a running gateway inside the VM itself.
	if r.mainPID() != "0" {
		return Failure{"restore", "gateway_not_stopped"}
	}
	// Once per new VM, before anything is restored into it: it holds nothing, mounts no host
	// folder and matches its declared CPUs, memory and disks. A retry after a partial restore
	// has the receipt and goes on (the restore steps below replace what they wrote).
	var fresh struct {
		NewID string `json:"new_id"`
	}
	if !readReceipt(r.t.file("fresh.json"), &fresh) || fresh.NewID != cutover.NewID {
		r.note("Checking the new VM is empty and matches its declared size")
		k := r.o.Kit
		request, _ := json.Marshal(map[string]any{"cpus": k.CPUs, "memory_gib": k.MemoryGiB, "config_sha256": k.ConfigSHA,
			"root_gib": k.RootGiB, "docker_gib": k.DockerGiB})
		if err := r.guestJob(ctx, "restore", 5*time.Minute, request, "fresh"); err != nil {
			return err
		}
		if err := writeReceipt(r.t.file("fresh.json"), map[string]any{"new_id": cutover.NewID, "checked_at": now()}); err != nil {
			return err
		}
	}
	r.note("Updating Hermes in the new VM to the preserved state's version or newer; this takes several minutes")
	if err := r.guestJob(ctx, "restore", 30*time.Minute, nil, "align", r.commit); err != nil {
		return err
	}
	r.note("Restoring memories, sessions, skills and settings")
	if err := r.streamIn(ctx, r.t.file("state.zip"), remoteRestore); err != nil {
		return Failure{"restore", "archive_copy_failed"}
	}
	if err := r.guestJob(ctx, "restore", 10*time.Minute, nil, "restore"); err != nil {
		return err
	}
	// The workspace archive is authoritative over overlapping external-memory files: it goes last.
	r.note("Restoring the workspace")
	workspace, err := os.Open(r.t.file("workspace.tar.zst"))
	if err != nil {
		return err
	}
	_, err = r.Runner.Output(ctx, sbx.Call{Args: []string{"exec", "-i", r.Name, "sh", "-c", workspaceRestore},
		Stdin: workspace, Timeout: 30 * time.Minute})
	workspace.Close()
	if err != nil {
		return Failure{"restore", "workspace_restore_failed"}
	}
	r.note("Wiring the secrets")
	if err := r.o.Secrets.WireNew(ctx); err != nil {
		return Failure{"restore", err.Error()}
	}
	if r.o.Kit.Slack != nil {
		r.note("Setting up Slack and checking its permission boundary")
		if err := r.slackSetup(ctx); err != nil {
			return err
		}
	}
	r.note("Starting the gateway")
	if err := writeReceipt(r.t.file("activation-attempted.json"), map[string]any{"new_id": cutover.NewID, "attempted_at": now()}); err != nil {
		return err
	}
	if err := r.systemctl("enable", r.Unit()); err != nil {
		return Failure{"restore", "gateway_enable_failed"}
	}
	if err := r.systemctl("start", r.Unit()); err != nil {
		return Failure{"restore", "gateway_start_failed"}
	}
	if err := r.ready(ctx, cutover.NewID); err != nil {
		return err
	}
	r.releaseHolder()
	var full map[string]any
	readReceipt(r.t.file("cutover.json"), &full)
	full["state"], full["activated_at"] = "active", now()
	return writeReceipt(r.t.file("cutover.json"), full)
}

// note tells the operator which part of a long step is running.
func (r *recreation) note(message string) { r.Printer.Print("restore", progress.Info, message) }

// guestJob runs a job that answers {"status"} and fails the step with its plain class.
func (r *recreation) guestJob(ctx context.Context, step string, timeout time.Duration, request []byte, job string, args ...string) error {
	out, err := guest.RunRequest(ctx, r.Runner, timeout, r.Name, request, job, args...)
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	var result map[string]any
	json.Unmarshal(lastLine(out), &result)
	if err != nil || result["status"] != "passed" {
		return Failure{step, job + ": " + Plain(result["class"])}
	}
	return nil
}

// liveSchema names the moved agent's check receipt (the selftest turn); an older run's Slack
// continuity receipt is not accepted, so a resumed run checks again.
const liveSchema = "selftest-check-v1"

// liveCurrent: this VM's current gateway passed the check.
func (r *recreation) liveCurrent() bool {
	var live liveReceipt
	var cutover struct {
		NewID string `json:"new_id"`
	}
	return readReceipt(r.t.file("live.json"), &live) && readReceipt(r.t.file("cutover.json"), &cutover) &&
		r.active() && live.Schema == liveSchema && live.Status == "passed" && live.ExitStatus == 0 &&
		live.Route == "selftest" && live.NewID == cutover.NewID && live.GatewaySignature == r.signature()
}

// moveCheck proves the moved agent works: one model turn through its own Hermes and provider in
// the new VM (the selftest job, judged on transport), with the gateway and the VM unchanged
// across it. The gateway's Slack connection was already proven by ready.json.
func (r *recreation) moveCheck(ctx context.Context) error {
	var cutover struct {
		NewID string `json:"new_id"`
	}
	readReceipt(r.t.file("cutover.json"), &cutover)
	path := r.t.file("live.json")
	if _, err := os.Stat(path); err == nil {
		os.Rename(path, fmt.Sprintf("%s.%d", path, time.Now().UnixNano()))
	}
	live := liveReceipt{Schema: liveSchema, Status: "failed", ExitStatus: 1, Route: "selftest", Step: "turn",
		FailureClass: "interrupted", NewID: cutover.NewID, GatewaySignature: r.signature()}
	fail := func(class string) error {
		live.FailureClass = class
		writeReceipt(path, live)
		return Failure{"check", class}
	}
	if err := writeReceipt(path, live); err != nil {
		return err
	}
	result, err := r.call(ctx, 12*time.Minute, "selftest")
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	if err != nil || result["status"] != "passed" {
		class := "selftest_failed"
		if c, ok := result["class"].(string); ok && c != "" {
			class = Plain(c)
		}
		return fail(class)
	}
	if r.signature() != live.GatewaySignature || r.sameVM(ctx, cutover.NewID, "check") != nil {
		return fail("gateway_or_vm_changed")
	}
	live.Status, live.ExitStatus, live.Step, live.FailureClass = "passed", 0, "complete", "none"
	return writeReceipt(path, live)
}

// finishMigrate runs on every exit: it stops the holder and, when the run failed while the
// original VM still exists with its gateway stopped, sets the attempt aside and starts the
// original gateway again if this run stopped it while it was up; it restores the collector
// timer.
func (r *recreation) finishMigrate(err *error) {
	r.releaseHolder()
	// Once removal was authorized the old VM may be going: never restart it or move the
	// attempt aside here; the next run resumes from current/ (its preflight sets the attempt
	// aside, archives kept, only if the old VM is still listed).
	if *err != nil && r.gatewayStopped && r.pre.OldID != "" && !r.t.has("removal-authorized.json") {
		lister := r.Runner
		lister.Detached = true
		records, listErr := lister.Inventory(context.Background())
		old := records[r.Name]
		if listErr == nil && old.ID == r.pre.OldID && (old.Status == "running" || old.Status == "stopped") {
			// The copy is taken after setAside, which moves the log out of current/.
			if aside := r.setAside(); aside == nil && !r.gatewayWasUp {
				r.Printer.Print("recovery", progress.Info, "The attempt was set aside; the agent was stopped before the resize and stays stopped")
			} else if aside == nil && !guard.Suspended(r.Paths, r.Name) {
				quiet := *r.Agent
				quiet.Runner.Detached = true
				r.quietCancel()
				if quiet.systemctl("start", r.Unit()) == nil {
					r.Printer.Print("recovery", progress.Info, "The old gateway is running again; nothing was removed")
				}
			} else {
				r.Printer.Print("recovery", progress.Info, "The attempt could not be set aside; the old gateway stays stopped")
			}
		}
	}
	if *err != nil && r.paused {
		if restoreErr := r.restoreCollector(); restoreErr != nil {
			r.Printer.Print("recovery", progress.Info, "Starting the collector timer again is pending: the next troupe resize or reset does it")
		}
	}
}
