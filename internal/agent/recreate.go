package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/guest"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// RecreateOptions is what a recreate needs beyond the agent.
type RecreateOptions struct {
	// Create is an agent's first creation: there is no VM to remove, and an agent that ever
	// existed is refused.
	Create  bool
	Kit     *Kit
	Secrets *secret.Store
	// Units fails when the installed units differ from troupe's or their guard is not loaded.
	Units func() error
	// CollectorTimer is a host timer whose job execs into VMs, stopped for the whole run and
	// started again after ("": none).
	CollectorTimer string
	// Hold keeps the new VM running until its gateway starts; nil runs a holder sbx exec.
	Hold func(ctx context.Context, log string) (stop func(), err error)
	// Label names a move into a new VM in its progress lines: "resize" when empty, "upgrade"
	// for one onto a newer kit version.
	Label string
}

// transaction is one recreate's receipts, kept across interrupted runs.
type transaction struct {
	base, current, completed string
}

func (t transaction) file(name string) string { return filepath.Join(t.current, name) }

func (t transaction) has(name string) bool {
	info, err := os.Stat(t.file(name))
	return err == nil && info.Size() > 0
}

// StateRoot holds every agent's transaction folders.
func (a *Agent) stateRoot() string {
	if a.State != "" {
		return a.State
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "sandbox-replacements")
}

// Receipt fields every reader relies on.
type preflightReceipt struct {
	Schema       string `json:"schema"`
	RecordedAt   string `json:"recorded_at"`
	Agent        string `json:"agent"`
	OldID        string `json:"old_id"`
	TargetHermes string `json:"target_hermes"`
	GatewayPID   string `json:"gateway_pid"`
	Mode         string `json:"mode"`
	SeedSHA      string `json:"seed_sha256"`
	Declarations string `json:"declarations_sha256"`
	Create       bool   `json:"create"`
}

type recreation struct {
	*Agent
	o              RecreateOptions
	label          string // the run's progress name: create or recreate
	t              transaction
	pre            preflightReceipt
	stopHolder     func()
	gatewayStopped bool
	// gatewayWasUp: the gateway was up when this run stopped it, so recovery may start it again.
	gatewayWasUp bool
	paused       bool
	// A migrate's backup: taken by this run, its state archive's hash and Hermes commit.
	snapshotFresh     bool
	backupSHA, commit string
	// built: this run built both kits into the host's build cache.
	built bool
	// listing: the sandbox inventory, read once and reused by the preflight's checks (each
	// listing costs seconds); cleared after the collector pause and when the preflight ends.
	// What it proves about the agent's own VM is proven again before any removal.
	listing map[string]sbx.Record
}

// preflightInventory is the preflight's one sandbox listing.
func (r *recreation) preflightInventory(ctx context.Context) (map[string]sbx.Record, error) {
	if r.listing != nil {
		return r.listing, nil
	}
	records, err := r.Runner.Inventory(ctx)
	if err == nil {
		r.listing = records
	}
	return records, err
}

// changing ends the preflight's listing: from here every proof reads sandboxd again.
func (r *recreation) changing() map[string]sbx.Record {
	records := r.listing
	r.listing = nil
	return records
}

const mode = "recreate"

// receiptSchema names this preflight receipt's shape. An open transaction an older troupe
// recorded (replacement-v2, for image based kits) is never resumed: its inputs no longer exist.
const receiptSchema = "replacement-v3"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Recreate deletes the agent's VM and creates a fresh one from its kits and seeds, with his
// secrets wired again, his kit's setup run and his gateway started. Every step leaves a
// receipt, so an interrupted run resumes where it stopped. Before the VM is removed a failure
// starts the old gateway again; after, nothing is restored or recreated automatically.
func (a *Agent) Recreate(ctx context.Context, o RecreateOptions) (err error) {
	r := &recreation{Agent: a, o: o}
	base := filepath.Join(a.stateRoot(), a.Name)
	r.t = transaction{base: base, current: filepath.Join(base, "reseed", "current"),
		completed: filepath.Join(base, "reseed", "completed")}
	verb := "Resetting"
	r.label = "reset"
	if o.Create {
		verb, r.label = "Creating", "create"
	}
	a.Printer.Print(r.label, progress.Started, fmt.Sprintf("%s the %s sandbox from its kits and seeds", verb, a.Printer.Agent))
	s, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	defer func() {
		r.finish(&err)
		if err != nil && ctx.Err() != nil {
			err = ErrInterrupted
		}
	}()
	if err := r.preflight(ctx); err != nil {
		return err
	}
	records := r.changing()
	if records == nil {
		var err error
		if records, err = a.Runner.Inventory(ctx); err != nil {
			return Failure{"preflight", "inventory_unreadable"}
		}
	}
	current := records[a.Name]
	switch {
	case current.ID != "" && current.ID == r.pre.OldID:
		if err := r.step(ctx, "removal", r.remove); err != nil {
			return err
		}
		if err := r.step(ctx, "creation", r.create); err != nil {
			return err
		}
	case current.ID == "":
		// Creation needs both the removal authorization and the removal's receipt.
		if err := r.step(ctx, "creation", r.create); err != nil {
			return err
		}
	default:
		var cutover struct {
			NewID string `json:"new_id"`
		}
		if !readReceipt(r.t.file("cutover.json"), &cutover) || cutover.NewID != current.ID {
			return Failure{r.label, "unexpected_vm_identity"}
		}
	}
	if r.t.has("activation-attempted.json") {
		if err := r.resumeActivation(ctx); err != nil {
			return err
		}
	} else {
		if r.stopHolder == nil {
			if err := r.hold(ctx); err != nil {
				return err
			}
		}
		if err := r.initialize(ctx); err != nil {
			return err
		}
	}
	return r.finalize(ctx)
}

// Check proves now, changing nothing but the host's build cache, that a recreate would pass its preflight: sandboxd, the
// secrets, the units, capacity, the VM and the kits (sbx env plan, and a build of both into the
// host's build cache), or for an open
// transaction its recorded inputs. It takes no sudo, lock or receipt, pauses nothing and never
// execs into the VM, so a tool can ask before it does anything a refused recreate would undo
// badly. The recreate itself checks everything again.
func (a *Agent) Check(ctx context.Context, o RecreateOptions) error {
	r := &recreation{Agent: a, o: o}
	base := filepath.Join(a.stateRoot(), a.Name)
	r.t = transaction{base: base, current: filepath.Join(base, "reseed", "current"),
		completed: filepath.Join(base, "reseed", "completed")}
	a.Printer.Print("check", progress.Started, "Proving a reset would pass its preflight; nothing changes")
	if err := r.check(ctx); err != nil {
		return err
	}
	a.Printer.Print("check", progress.OK, "A reset would pass its preflight now")
	return nil
}

func (r *recreation) check(ctx context.Context) error {
	if os.Geteuid() == 0 {
		return Failure{"check", "run_as_your_own_user"}
	}
	if err := r.probe(ctx); err != nil {
		return Failure{"check", "sandboxd_unhealthy"}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return Failure{"check", "docker_cli_missing"}
	}
	if _, err := os.Stat(filepath.Join(r.t.base, "current")); err == nil {
		return Failure{"check", "other_maintenance_pending"}
	}
	if guard.Suspended(r.Paths, r.Name) {
		return Failure{"check", "suspended_by_a_phase_record"}
	}
	if err := r.o.Secrets.Sources(); err != nil {
		return Failure{"check", "secret_" + err.Error() + "; troupe secret check " + r.Name}
	}
	if err := r.o.Units(); err != nil {
		return Failure{"check", "units_not_installed: " + err.Error()}
	}
	records, err := r.preflightInventory(ctx)
	if err != nil {
		return Failure{"check", "inventory_unreadable"}
	}
	if err := r.o.Secrets.NoGlobals(ctx); err != nil {
		return Failure{"check", err.Error()}
	}
	// An open transaction resumes from its receipts: its recorded inputs must still hold.
	if readReceipt(r.t.file("preflight.json"), &r.pre) && !r.t.has("finalized.json") {
		if err := r.currentSchema("check"); err != nil {
			return err
		}
		if r.pre.Create {
			return Failure{"check", "open_creation; run troupe create " + r.Name}
		}
		if r.pre.Mode != mode || r.pre.Agent != r.Name {
			return Failure{"check", "receipt_from_another_transaction"}
		}
		return r.validateInputs()
	}
	record, exists := records[r.Name]
	if !exists {
		return Failure{"check", "sandbox_missing; troupe create makes an agent's first VM"}
	}
	if record.Status != "running" && record.Status != "stopped" {
		return Failure{"check", "sandbox_unavailable"}
	}
	if err := r.plan(ctx, "check"); err != nil {
		return err
	}
	return r.prebuild(ctx, "check")
}

// step prints a phase's start and end around it.
func (r *recreation) step(ctx context.Context, name string, run func(context.Context) error) error {
	r.Printer.Print(name, progress.Started, "")
	if err := run(ctx); err != nil {
		return err
	}
	r.Printer.Print(name, progress.OK, "")
	return nil
}

// preflight changes nothing in the VM: it proves the host, the secrets, the kits and the seeds, archives a finished earlier run and records (or resumes) this one.
func (r *recreation) preflight(ctx context.Context) error {
	r.Printer.Print("preflight", progress.Started, "")
	if os.Geteuid() == 0 {
		return Failure{"preflight", "run_as_your_own_user"}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return Failure{"preflight", "docker_cli_missing"}
	}
	// A replacement's open transaction lives beside this one; they never interleave.
	if _, err := os.Stat(filepath.Join(r.t.base, "current")); err == nil {
		return Failure{"preflight", "other_maintenance_pending"}
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
	if err := r.o.Secrets.NoGlobals(ctx); err != nil {
		return Failure{"preflight", err.Error()}
	}
	if err := r.pauseCollector(ctx); err != nil {
		return err
	}
	// The collector may have run a copy while the timer was stopping: list again from here.
	r.listing = nil
	if r.o.Create {
		if err := r.createPreflight(ctx); err != nil {
			return err
		}
	}
	if !readReceipt(r.t.file("preflight.json"), &r.pre) {
		if err := r.recordPreflight(ctx); err != nil {
			return err
		}
	}
	if err := r.currentSchema("preflight"); err != nil {
		return err
	}
	if r.pre.Create && !r.o.Create {
		return Failure{"preflight", "open_creation; run troupe create " + r.Name}
	}
	if r.pre.Mode != mode || r.pre.Agent != r.Name {
		return Failure{"preflight", "receipt_from_another_transaction"}
	}
	if err := r.validateInputs(); err != nil {
		return err
	}
	if err := r.prebuildBeforeRemoval(ctx); err != nil {
		return err
	}
	r.Printer.Print("preflight", progress.OK, "")
	return nil
}

// currentSchema refuses a preflight receipt an older troupe recorded: its open transaction is
// reviewed by hand, never resumed.
func (r *recreation) currentSchema(step string) error {
	if r.pre.Schema != receiptSchema {
		return Failure{step, "receipt_schema_outdated; an older troupe opened " + r.t.current + ", review it by hand"}
	}
	return nil
}

// open creates the transaction's folder and sends native output to its log.
func (r *recreation) open() error {
	if err := os.MkdirAll(r.t.current, 0o700); err != nil {
		return err
	}
	os.Chmod(r.t.base, 0o700)
	r.Runner.Log = r.t.file("native.log")
	return nil
}

// archivePrevious moves a finished run to completed/ (removing the image of the VM it replaced,
// if that run stopped before it could), keeps an open one for resuming, and removes an empty
// folder.
func (r *recreation) archivePrevious(ctx context.Context) error {
	entries, err := os.ReadDir(r.t.current)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var finalized struct {
		At string `json:"finalized_at"`
	}
	switch {
	case readReceipt(r.t.file("finalized.json"), &finalized):
		r.removeReplaced(ctx)
		return r.moveToCompleted(finalized.At)
	case len(entries) > 0:
		r.Printer.Print("preflight", progress.Info, "Resuming the open transaction at "+r.t.current)
		return nil
	}
	return os.Remove(r.t.current)
}

var stamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

func (r *recreation) moveToCompleted(at string) error {
	if !stamp.MatchString(at) {
		return Failure{"receipts", "invalid_finalized_timestamp"}
	}
	if err := os.MkdirAll(r.t.completed, 0o700); err != nil {
		return err
	}
	destination := filepath.Join(r.t.completed, at)
	if _, err := os.Stat(destination); err == nil {
		return Failure{"receipts", "completed_destination_exists"}
	}
	return os.Rename(r.t.current, destination)
}

// createPreflight allows a first creation only for an agent that never existed: any completed
// recreate or replacement refuses it (so a missing agent is never recreated by mistake), as do
// an existing sandbox and earlier receipts; an open creation resumes.
func (r *recreation) createPreflight(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.t.base, "completed")); err == nil {
		return Failure{"preflight", "create_refused_agent_existed_before"}
	}
	var pre preflightReceipt
	if readReceipt(r.t.file("preflight.json"), &pre) {
		if !pre.Create {
			return Failure{"preflight", "create_refused_open_recreate"}
		}
		return nil
	}
	if _, err := os.Stat(r.t.completed); err == nil {
		return Failure{"preflight", "create_refused_agent_existed_before"}
	}
	if r.t.has("cutover.json") || r.t.has("activation-attempted.json") {
		return Failure{"preflight", "create_refused_earlier_receipts"}
	}
	records, err := r.preflightInventory(ctx)
	if err != nil {
		return Failure{"preflight", "inventory_unreadable"}
	}
	if _, exists := records[r.Name]; exists {
		return Failure{"preflight", "create_refused_sandbox_exists"}
	}
	return nil
}

// plan asks sbx, read only, to resolve the agent's environment and its kits (sbx env plan),
// so a kit sbx would refuse fails here, before anything changes. sbx builds the image itself
// at sbx env create.
func (r *recreation) plan(ctx context.Context, step string) error {
	if err := r.allowKitSource(ctx, step); err != nil {
		return err
	}
	if _, err := r.Runner.Output(ctx, sbx.Call{Args: []string{"env", "plan", r.o.Kit.sbxenv}, Env: r.o.Kit.StorageEnv}); err != nil {
		return Failure{step, "kit_invalid; sbx env plan " + r.o.Kit.sbxenv}
	}
	return nil
}

// allowKitSource makes sbx accept a published kit's registry (a fresh sbx accepts only
// docker.io/). plan runs it first, while the old VM still runs, before sbx resolves the kit; a
// creation, which has no plan, runs it before sbx env create.
func (r *recreation) allowKitSource(ctx context.Context, step string) error {
	if r.o.Kit.Reference == "" {
		return nil
	}
	added, err := r.Runner.AllowKitSource(ctx, path.Dir(r.o.Kit.Reference))
	if err != nil {
		return Failure{step, "kit_source_not_allowed"}
	}
	if added {
		r.Printer.Print(step, progress.Info, "Allowed sbx to read kits from "+path.Dir(r.o.Kit.Reference)+"/")
	}
	return nil
}

// prebuild builds both kits with the host's BuildKit, the engine and cache sbx env create uses
// for local v3 kits, into the build cache only: a kit that does not build fails here, while the
// old VM still runs, and the creation after the removal finds every layer cached. The kits are
// the operator's, but their build runs commands, so its output is logged escaped and capped. A
// published base kit is pulled instead, by its pinned version: one the registry no longer serves,
// or this machine may no longer read, fails here too.
func (r *recreation) prebuild(ctx context.Context, step string) error {
	log, err := openLog(r.Runner.Log)
	if err != nil {
		return err
	}
	defer log.Close()
	for _, kit := range []string{r.o.Kit.Base, r.o.Kit.Folder} {
		args := []string{"buildx", "build", kit, "-f", registry.Descriptor(kit), "--output", "type=cacheonly"}
		name := filepath.Base(kit)
		if kit == r.o.Kit.Base && r.o.Kit.Reference != "" {
			// REGISTRY/TYPE:VERSION is shown as "TYPE VERSION".
			args, name = []string{"pull", "--quiet", r.o.Kit.Reference}, strings.Replace(path.Base(r.o.Kit.Reference), ":", " ", 1)
			r.Printer.Print(step, progress.Info, "Pulling the "+name+" kit")
		} else {
			r.Printer.Print(step, progress.Info, "Building the "+name+" kit into the build cache")
		}
		out := sbx.LogWriter(log)
		fmt.Fprintf(out, "$ docker %s\n", strings.Join(args, " "))
		build, cancel := context.WithTimeout(ctx, 60*time.Minute)
		cmd := exec.CommandContext(build, "docker", args...)
		cmd.Stdout, cmd.Stderr = out, out
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		err := cmd.Run()
		timedOut := build.Err() == context.DeadlineExceeded
		cancel()
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		failed := "kit_build_failed: "
		if args[0] == "pull" {
			failed = "kit_pull_failed: "
		}
		if timedOut {
			return Failure{step, failed + name + " timed out"}
		}
		if err != nil {
			return Failure{step, failed + name}
		}
	}
	r.built = true
	return nil
}

// prebuildBeforeRemoval builds the kits again when a resumed run has not removed the old VM
// yet: the cache an earlier run filled may be gone, and a failure must still come before the
// removal.
func (r *recreation) prebuildBeforeRemoval(ctx context.Context) error {
	if r.built || r.t.has("removed.json") {
		return nil
	}
	return r.prebuild(ctx, "preflight")
}

// recordPreflight proves everything before the first change and records it.
func (r *recreation) recordPreflight(ctx context.Context) error {
	records, err := r.preflightInventory(ctx)
	if err != nil {
		return Failure{"preflight", "inventory_unreadable"}
	}
	record, exists := records[r.Name]
	pre := preflightReceipt{Schema: receiptSchema, RecordedAt: now(), Agent: r.Name, Mode: mode,
		Create: r.o.Create, TargetHermes: r.o.Kit.Hermes,
		SeedSHA: r.o.Kit.ManifestSHA(), Declarations: r.o.Kit.Declarations}
	if r.o.Create {
		if exists {
			return Failure{"preflight", "create_refused_sandbox_exists"}
		}
		pre.OldID, pre.GatewayPID = "absent", "0"
	} else {
		if !exists {
			return Failure{"preflight", "sandbox_missing; troupe create makes an agent's first VM"}
		}
		if record.Status != "running" && record.Status != "stopped" {
			return Failure{"preflight", "sandbox_unavailable"}
		}
		// An interrupted run may have left the gateway stopped; then nothing may run inside. A
		// stopped VM runs nothing, and is never booted just to ask.
		if !r.active() && !r.vmStopped(ctx) {
			if result, err := r.call(ctx, 60*time.Second, "gateway-stopped"); err != nil || result["status"] != "passed" {
				return Failure{"preflight", "gateway_stop_unproven"}
			}
		}
		pre.OldID, pre.GatewayPID = record.ID, r.mainPID()
	}
	// A replacement proves the kits before the old VM goes; a creation has no old VM to protect,
	// and sbx env create refuses a bad kit the same way.
	if !r.o.Create {
		if err := r.plan(ctx, "preflight"); err != nil {
			return err
		}
		if err := r.prebuild(ctx, "preflight"); err != nil {
			return err
		}
	}
	// The kit's own seed rules, run in the current VM before any downtime when it is running
	// (a stopped one is never booted for this). Only advisory: that VM is the agent's. The new
	// VM runs the full check again before the first start.
	if !r.o.Create && r.o.Kit.SeedCheck != nil && record.Status == "running" && !r.vmStopped(ctx) {
		if err := r.kitTool(ctx, "preflight", r.o.Kit.SeedCheck, "validate", nil); err != nil {
			return err
		}
	}
	if err := r.open(); err != nil {
		return err
	}
	if err := writeFile(r.t.file("seed-manifest.json"), r.o.Kit.Manifest); err != nil {
		return err
	}
	// The seeds' bytes too: what later updates and harvests measure the agent's edits against.
	for _, seed := range r.o.Kit.Seeds {
		target := r.t.file(seed.Name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := writeFile(target, seed.Data); err != nil {
			return err
		}
	}
	if r.o.Create {
		// Nothing to remove: record the creation in place of the removal receipts.
		if err := writeReceipt(r.t.file("removal-authorized.json"), map[string]any{"mode": mode, "old_id": "absent",
			"create": true}); err != nil {
			return err
		}
		if err := writeReceipt(r.t.file("removed.json"), map[string]any{"mode": mode, "old_id": "absent",
			"removed_at": now(), "create": true}); err != nil {
			return err
		}
	}
	// preflight.json appears only complete, after the receipts above: an interrupted
	// preflight simply runs again.
	if err := writeReceipt(r.t.file("preflight.json"), pre); err != nil {
		return err
	}
	r.pre = pre
	return nil
}

// validateInputs refuses a resumed run whose seeds or declarations (the kits' descriptors and
// dockerfiles, so the pinned Hermes commit too) changed.
func (r *recreation) validateInputs() error {
	switch {
	case r.pre.SeedSHA != r.o.Kit.ManifestSHA():
		return Failure{"preflight", "seeds_changed"}
	case r.pre.Declarations != r.o.Kit.Declarations:
		return Failure{"preflight", "declarations_changed"}
	}
	return nil
}

// remove drains and stops the old gateway, stops the VM, rechecks its identity and removes it.
func (r *recreation) remove(ctx context.Context) error {
	if r.t.has("cutover.json") || r.t.has("activation-attempted.json") {
		return Failure{"removal", "unexpected_original_identity"}
	}
	if err := r.sameVM(ctx, r.pre.OldID, "removal"); err != nil {
		return err
	}
	if err := r.o.Secrets.Sources(); err != nil {
		return Failure{"removal", "secret_" + err.Error()}
	}
	// Current work finishes first; on failure the gateway keeps running.
	if err := r.Drain(ctx, 60); err != nil {
		if errors.Is(err, ErrInterrupted) {
			return err
		}
		return Failure{"removal", "drain_failed; the agent is still running"}
	}
	// Recovery starts the gateway again only when it was up (or restarting) right here: an
	// agent the operator had stopped stays stopped, as its VM would otherwise boot. Read live,
	// not from the receipt, because a resumed run follows whatever happened in between.
	r.gatewayWasUp = unitUp(r.Name)
	r.gatewayStopped = true
	if err := r.systemctl("stop", r.Unit()); err != nil {
		return Failure{"removal", "gateway_stop_failed"}
	}
	if r.mainPID() != "0" {
		return Failure{"removal", "gateway_not_stopped"}
	}
	if !r.vmStopped(ctx) {
		if result, err := r.call(ctx, 60*time.Second, "gateway-stopped"); err != nil || result["status"] != "passed" {
			return Failure{"removal", "gateway_not_stopped"}
		}
	}
	// The image the old VM runs, removed once its replacement runs (removeReplaced).
	if err := writeReceipt(r.t.file("removal-authorized.json"), map[string]any{"mode": mode,
		"old_id": r.pre.OldID, "old_image": r.image(ctx, r.Name)}); err != nil {
		return err
	}
	r.Runner.Output(ctx, sbx.Call{Args: []string{"stop", r.Name}, Timeout: 120 * time.Second})
	if err := r.waitGone(ctx, false); err != nil {
		return err
	}
	// Recheck the identity immediately before removal, not only before the drain.
	if err := r.sameVM(ctx, r.pre.OldID, "removal"); err != nil {
		return err
	}
	if _, err := r.Runner.Output(ctx, sbx.Call{Args: []string{"rm", r.Name, "--force"}, Timeout: 300 * time.Second}); err != nil {
		return Failure{"removal", "removal_failed"}
	}
	if err := r.waitGone(ctx, true); err != nil {
		return err
	}
	return writeReceipt(r.t.file("removed.json"), map[string]any{"mode": mode, "old_id": r.pre.OldID, "removed_at": now()})
}

func (r *recreation) sameVM(ctx context.Context, id, step string) error {
	records, err := r.Runner.Inventory(ctx)
	if err != nil {
		return Failure{step, "inventory_unreadable"}
	}
	if records[r.Name].ID != id {
		return Failure{step, "vm_identity_changed"}
	}
	return nil
}

// waitGone waits until no VM process is left, and with absent, until sbx lists no sandbox.
func (r *recreation) waitGone(ctx context.Context, absent bool) error {
	deadline := time.Now().Add(r.Timing.StopWait)
	for {
		records, err := r.Runner.Inventory(ctx)
		_, listed := records[r.Name]
		if err == nil && (!absent || !listed) && (absent || records[r.Name].Status != "running") && guard.VMGone(r.Paths, r.Name) {
			return nil
		}
		if time.Now().After(deadline) {
			if absent {
				return Failure{"removal", "removal_unproven"}
			}
			return Failure{"removal", "vm_still_running"}
		}
		if err := sleep(ctx, r.Timing.StopEvery); err != nil {
			return err
		}
	}
}

// create proves the removal, creates the new VM from the agent's declarations and records it.
func (r *recreation) create(ctx context.Context) error {
	if guard.Suspended(r.Paths, r.Name) {
		return Failure{"creation", "suspended_by_a_phase_record"}
	}
	var authorized, removed struct {
		Mode      string `json:"mode"`
		OldID     string `json:"old_id"`
		RemovedAt string `json:"removed_at"`
	}
	if !readReceipt(r.t.file("removal-authorized.json"), &authorized) || authorized.Mode != mode ||
		authorized.OldID != r.pre.OldID {
		return Failure{"creation", "removal_not_authorized"}
	}
	if !readReceipt(r.t.file("removed.json"), &removed) || removed.OldID != r.pre.OldID || removed.RemovedAt == "" {
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

// provision creates the new VM from the agent's declarations, keeps it running with a holder
// and records its identity in cutover.json. Callers prove the removal first.
func (r *recreation) provision(ctx context.Context, step string) error {
	if err := r.o.Secrets.Sources(); err != nil {
		return Failure{step, "secret_" + err.Error()}
	}
	// A published kit arrives built (sbx downloads it unless its store already holds that
	// version); only the agent's own small layer (its settings) is built here before sbx
	// assembles the VM. A local kit is built from its source.
	creating := "Building the image and creating the new VM from the kits"
	if r.o.Kit.Reference != "" {
		creating = "Getting the published kit, adding " + r.Printer.Agent + "'s settings and creating the new VM"
	}
	if err := r.allowKitSource(ctx, step); err != nil {
		return err
	}
	r.Printer.Print(step, progress.Info, creating)
	if _, err := r.Runner.Output(ctx, sbx.Call{Args: []string{"env", "create", r.o.Kit.sbxenv, "--force"},
		Timeout: 2 * time.Hour, Env: r.o.Kit.StorageEnv}); err != nil {
		return Failure{step, "sandbox_create_failed"}
	}
	if err := r.hold(ctx); err != nil {
		return err
	}
	var newID string
	for try := 0; ; try++ {
		records, err := r.Runner.Inventory(ctx)
		if err == nil && records[r.Name].Status == "running" {
			newID = records[r.Name].ID
			break
		}
		if try == 45 {
			return Failure{step, "sandbox_not_running"}
		}
		if err := sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	if newID == "" || newID == r.pre.OldID {
		return Failure{step, "new_identity_invalid"}
	}
	if err := writeReceipt(r.t.file("cutover.json"), map[string]any{"created_at": now(), "old_id": r.pre.OldID,
		"new_id": newID, "target_hermes": r.pre.TargetHermes, "state": "created"}); err != nil {
		return err
	}
	r.Printer.Print(step, progress.Info, "The new VM is running")
	return nil
}

// hold keeps the new VM running until its gateway starts: Docker Sandbox stops a VM nothing
// is attached to.
func (r *recreation) hold(ctx context.Context) error {
	if guard.Suspended(r.Paths, r.Name) {
		return Failure{"creation", "suspended_by_a_phase_record"}
	}
	if err := r.probe(ctx); err != nil {
		return Failure{"creation", "sandboxd_unhealthy"}
	}
	hold := r.o.Hold
	if hold == nil {
		hold = r.holder
	}
	stop, err := hold(ctx, r.t.file("new-holder.log"))
	if err != nil {
		return Failure{"creation", "holder_failed"}
	}
	r.stopHolder = stop
	return nil
}

func (r *recreation) holder(_ context.Context, log string) (func(), error) {
	out, err := openLog(log)
	if err != nil {
		return nil, err
	}
	binary := r.Runner.Binary
	if binary == "" {
		binary = "sbx"
	}
	// Its own process group: a Ctrl+C reaches troupe, which stops it in order.
	cmd := exec.Command(binary, "exec", r.Name, "sh", "-c", "while :; do sleep 3600; done", r.Name+"-holder")
	// The holder's output comes from the VM: escaped and capped like every other log.
	logw := sbx.LogWriter(out)
	cmd.Stdout, cmd.Stderr = logw, logw
	cmd.SysProcAttr = detachedProcess()
	// Its output now passes through a pipe: a child of sbx holding it open must not make the
	// release wait forever.
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		out.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); out.Close(); close(done) }()
	select {
	case <-done:
		return nil, errors.New("holder exited")
	case <-time.After(time.Second):
	}
	// Released by a kill: sbx exec ignores SIGTERM and kept polling until a 10 s kill anyway
	// (measured with sbx 0.48.0); the gateway's own exec holds the VM by then.
	return func() {
		cmd.Process.Kill()
		<-done
	}, nil
}

// initialize checks the new VM, wires the secrets, sets up Slack and the kit, then starts the
// gateway for the first time.
func (r *recreation) initialize(ctx context.Context) error {
	var cutover struct {
		NewID string `json:"new_id"`
	}
	if !readReceipt(r.t.file("cutover.json"), &cutover) {
		return Failure{"fresh_checks", "cutover_receipt_missing"}
	}
	var seeds struct {
		NewID  string `json:"new_id"`
		Status string `json:"status"`
	}
	if !readReceipt(r.t.file("seeds.json"), &seeds) {
		if err := r.step(ctx, "fresh checks", r.fresh); err != nil {
			return err
		}
		if err := writeReceipt(r.t.file("seeds.json"), map[string]any{"new_id": cutover.NewID, "status": "passed"}); err != nil {
			return err
		}
	} else if seeds.NewID != cutover.NewID || seeds.Status != "passed" {
		return Failure{"fresh_checks", "seed_receipt_identity_mismatch"}
	}
	if err := r.step(ctx, "secrets", func(ctx context.Context) error {
		if err := r.o.Secrets.WireNew(ctx); err != nil {
			return Failure{"secrets", err.Error()}
		}
		return nil
	}); err != nil {
		return err
	}
	if r.o.Kit.Slack != nil {
		if err := r.step(ctx, "slack", r.slackSetup); err != nil {
			return err
		}
	}
	if r.o.Kit.Setup != nil {
		if err := r.step(ctx, "setup", func(ctx context.Context) error {
			request, _ := json.Marshal(map[string]string{"agent": r.Name})
			return r.kitTool(ctx, "setup", r.o.Kit.Setup, "run", request)
		}); err != nil {
			return err
		}
	}
	return r.step(ctx, "start", func(ctx context.Context) error {
		if err := writeReceipt(r.t.file("activation-attempted.json"), map[string]any{"new_id": cutover.NewID, "attempted_at": now()}); err != nil {
			return err
		}
		if err := r.systemctl("enable", r.Unit()); err != nil {
			return Failure{"start", "gateway_enable_failed"}
		}
		if err := r.systemctl("start", r.Unit()); err != nil {
			return Failure{"start", "gateway_start_failed"}
		}
		if err := r.ready(ctx, cutover.NewID); err != nil {
			return err
		}
		r.releaseHolder()
		r.gatewayStopped = false
		return nil
	})
}

func (r *recreation) releaseHolder() {
	if r.stopHolder != nil {
		r.stopHolder()
		r.stopHolder = nil
	}
}

// fresh proves the new VM holds nothing from before and matches its declarations and seeds.
func (r *recreation) fresh(ctx context.Context) error {
	k := r.o.Kit
	request, _ := json.Marshal(map[string]any{"cpus": k.CPUs, "memory_gib": k.MemoryGiB, "config_sha256": k.ConfigSHA,
		"root_gib": k.RootGiB, "docker_gib": k.DockerGiB})
	out, err := guest.RunRequest(ctx, r.Runner, 5*time.Minute, r.Name, request, "fresh")
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	var result map[string]any
	json.Unmarshal(lastLine(out), &result)
	if err != nil || result["status"] != "passed" {
		return Failure{"fresh_checks", Plain(result["class"])}
	}
	if k.SeedCheck != nil {
		return r.kitTool(ctx, "fresh_checks", k.SeedCheck, "check", nil)
	}
	return nil
}

// kitTool runs one of the base kit's tools in the VM with the composed seeds beside it. Its
// output, which carries kit diagnostics and never a secret, goes to the private log.
func (r *recreation) kitTool(ctx context.Context, step string, tool *guest.File, action string, request []byte) error {
	name := strings.TrimPrefix(tool.Name, "kit/")
	out, err := guest.RunFiles(ctx, r.Runner, 10*time.Minute, r.Name, request, r.o.Kit.kitFiles(tool), "kit", name, action)
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	var result map[string]any
	json.Unmarshal(lastLine(out), &result)
	if log, logErr := openLog(r.Runner.Log); logErr == nil {
		fmt.Fprintf(sbx.LogWriter(log), "--- %s %s\n%v\n", name, action, result["output"])
		log.Close()
	}
	if err != nil || result["status"] != "passed" {
		return Failure{step, strings.TrimSuffix(name, ".py") + "_" + action + "_failed: " + Plain(result["class"])}
	}
	return nil
}

// slackSetup writes the token placeholders and authenticates both tokens; at a creation it also
// proves the scopes and the permission boundary. A move keeps the Slack app and tokens the
// creation proved, so its setup checks the VM only (the guest's "move").
func (r *recreation) slackSetup(ctx context.Context) error {
	tokens := map[string]string{}
	for _, svc := range r.o.Secrets.Services {
		if svc.Placeholder && (svc.Env == "SLACK_BOT_TOKEN" || svc.Env == "SLACK_APP_TOKEN") {
			tokens[svc.Env] = svc.Value
		}
	}
	if len(tokens) != 2 {
		return Failure{"slack", "slack_tokens_not_declared_in_secrets_json"}
	}
	action := "setup"
	if r.pre.Mode == migrateMode {
		action = "move"
	}
	out, err := guest.RunRequest(ctx, r.Runner, 5*time.Minute, r.Name, r.o.Kit.slackRequest(tokens), "slack", action)
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	var result map[string]any
	json.Unmarshal(lastLine(out), &result)
	if err != nil || result["status"] != "passed" {
		return Failure{"slack", Plain(result["class"])}
	}
	// A published agent's first setup learned its workspace (the bot token's): record it.
	if r.o.Kit.Slack.Team == "" {
		team, _ := result["team"].(string)
		if err := r.o.Kit.recordWorkspace(team); err != nil {
			return Failure{"slack", "slack_workspace_not_recorded"}
		}
		r.Printer.Print("slack", progress.Info, "Pinned to the Slack workspace "+team)
	}
	return nil
}

// ready proves the started gateway is this VM's, running (with Slack connected for Slack
// kits) and was not restarted while being checked.
func (r *recreation) ready(ctx context.Context, newID string) error {
	if err := r.sameVM(ctx, newID, "start"); err != nil {
		return err
	}
	if !r.active() {
		return Failure{"start", "gateway_not_active"}
	}
	signature := r.signature()
	if guard.Suspended(r.Paths, r.Name) {
		return Failure{"start", "suspended_by_a_phase_record"}
	}
	if err := r.waitReady(ctx); err != nil {
		return err
	}
	// The VM was proven above and the run holds its lock and holder; the unchanged gateway
	// signature shows nothing restarted meanwhile.
	if !r.active() || r.signature() != signature {
		return Failure{"start", "gateway_restarted_during_readiness"}
	}
	check := "gateway_readiness"
	if r.Slack {
		check = "slack_readiness"
	}
	return writeReceipt(r.t.file("ready.json"), map[string]any{"mode": mode, "new_id": newID, "status": "passed",
		"check": check, "checked_at": now()})
}

func (r *recreation) signature() string {
	out, _ := exec.Command("systemctl", "show", r.Unit(), "-p", "MainPID", "-p", "ExecMainStartTimestampMonotonic").Output()
	return string(out)
}

// resumeActivation finishes a run interrupted after the first start was attempted.
func (r *recreation) resumeActivation(ctx context.Context) error {
	var cutover, activation struct {
		NewID string `json:"new_id"`
	}
	if !readReceipt(r.t.file("cutover.json"), &cutover) || !readReceipt(r.t.file("activation-attempted.json"), &activation) ||
		activation.NewID != cutover.NewID {
		return Failure{"start", "activation_identity_mismatch"}
	}
	// Enabled again too, as the first attempt did: an operator's troupe stop in between
	// disabled it, and the finished agent must start at boot.
	return r.step(ctx, "start", func(ctx context.Context) error {
		if err := r.systemctl("enable", r.Unit()); err != nil {
			return Failure{"start", "gateway_enable_failed"}
		}
		if err := r.systemctl("start", r.Unit()); err != nil {
			return Failure{"start", "gateway_start_failed"}
		}
		return r.ready(ctx, cutover.NewID)
	})
}

// finalize restores the collector timer and files the receipts under completed/.
func (r *recreation) finalize(ctx context.Context) error {
	if err := r.restoreCollector(); err != nil {
		return Failure{r.label, "collector_restore_failed"}
	}
	var cutover struct {
		NewID string `json:"new_id"`
	}
	readReceipt(r.t.file("cutover.json"), &cutover)
	at := now()
	if err := writeReceipt(r.t.file("finalized.json"), map[string]any{"mode": mode, "new_id": cutover.NewID,
		"finalized_at": at, "restored_guest_state": false, "backed_up_guest_state": false, "slack_ready": r.Slack}); err != nil {
		return err
	}
	r.removeReplaced(ctx)
	if err := r.moveToCompleted(at); err != nil {
		return err
	}
	r.Printer.Print(r.label, progress.OK, "Done. Receipts: "+filepath.Join(r.t.completed, at))
	return nil
}

// finish runs on every exit: it stops the holder, starts the old gateway again when the run
// stopped it and failed before anything was removed, and restores the collector timer.
func (r *recreation) finish(err *error) {
	r.releaseHolder()
	quiet := *r.Agent
	quiet.Runner.Detached = true
	if *err != nil && r.gatewayStopped && r.gatewayWasUp && !r.t.has("removed.json") && !r.t.has("cutover.json") {
		records, listErr := quiet.Runner.Inventory(context.Background())
		if listErr == nil && r.pre.OldID != "" && records[r.Name].ID == r.pre.OldID && !guard.Suspended(r.Paths, r.Name) {
			r.quietCancel()
			if quiet.systemctl("start", r.Unit()) == nil {
				r.Printer.Print("recovery", progress.Info, "The old gateway is running again; nothing was removed")
			}
		}
	}
	if *err != nil && r.paused {
		if restoreErr := r.restoreCollector(); restoreErr != nil {
			r.Printer.Print("recovery", progress.Info, "Starting the collector timer again is pending: the next troupe reset does it")
		}
	}
}

// Existed reports whether this machine holds any transaction record of an agent with this name
// (a create, recreate or migrate ever started): a new agent may not take the name while they remain.
func Existed(name string) bool {
	_, err := os.Lstat(filepath.Join((&Agent{}).stateRoot(), name))
	return err == nil
}

// Created reports whether a create or recreate of this agent ever completed.
func Created(name string) bool {
	entries, err := os.ReadDir(filepath.Join((&Agent{}).stateRoot(), name, "reseed", "completed"))
	return err == nil && len(entries) > 0
}

// Open reports whether a transaction of this agent (a migrate, or a create, recreate or reset)
// stopped partway and waits to be resumed.
func Open(name string) bool {
	for _, current := range []string{"current", filepath.Join("reseed", "current")} {
		if _, err := os.Lstat(filepath.Join((&Agent{}).stateRoot(), name, current)); err == nil {
			return true
		}
	}
	return false
}
