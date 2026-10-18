package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe/internal/guard"
	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/sbx"
	"github.com/starknet-innovation/troupe/internal/secret"
)

// assembled is the prefix of the image sbx builds for one sandbox from its kits; only such an
// image is the agent's own to remove.
const assembled = "sandbox-kit-assembled:"

// Delete removes the agent entirely: it stops its gateway and VM without waiting for its work
// (that work is deleted too), removes the VM (sbx also removes the secrets
// scoped to it) and the image built for that VM when no other sandbox uses it, then troupe's
// local records of it (keys, receipts, recovery backups, update receipts, logs, its phase
// record) and folder, the folder troupe wrote for a published agent ("" for a local one, whose
// folder is the maintainer's). Every step is idempotent: run again, it finishes the job.
func (a *Agent) Delete(ctx context.Context, folder string) error {
	a.Printer.Print("delete", progress.Started, "Removing "+a.Printer.Agent+" for good")
	s, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	// A copy of the agent in progress (its pair record not complete) would be orphaned.
	if guard.Suspended(a.Paths, a.Name) {
		return Failure{"delete", "a_copy_is_in_progress; finish or remove it first"}
	}
	records, err := a.Runner.Inventory(ctx)
	if err != nil {
		return Failure{"delete", "inventory_unreadable"}
	}
	if _, exists := records[a.Name]; exists {
		image := a.image(ctx, a.Name)
		// No drain: whatever the agent is doing is deleted with it. Its gateway unit is stopped
		// first, so systemd does not restart it while the VM goes.
		if err := a.systemctl("disable", "--now", a.Unit()); err != nil {
			return Failure{"delete", "gateway_stop_failed"}
		}
		a.Runner.Output(ctx, sbx.Call{Args: []string{"stop", a.Name}, Timeout: 2 * time.Minute})
		if _, err := a.Runner.Output(ctx, sbx.Call{Args: []string{"rm", a.Name, "--force"}, Timeout: 5 * time.Minute}); err != nil {
			return Failure{"delete", "sandbox_remove_failed"}
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			records, err := a.Runner.Inventory(ctx)
			if _, exists := records[a.Name]; err == nil && !exists {
				break
			}
			if time.Now().After(deadline) {
				return Failure{"delete", "sandbox_still_listed"}
			}
			if err := sleep(ctx, a.Timing.StopEvery); err != nil {
				return err
			}
		}
		a.removeImage(ctx, "delete", image)
	}
	a.removeSources(ctx)
	// The gateway unit is a template instance: disabling it leaves nothing of it behind.
	if err := a.systemctl("disable", a.Unit()); err != nil {
		return Failure{"delete", "gateway_disable_failed"}
	}
	state := filepath.Dir(a.stateRoot())
	for _, path := range []string{
		filepath.Join(secret.DefaultRoot(), a.Name),
		filepath.Join(state, a.Name+"-updates"),
		filepath.Join(state, "live-apply", a.Name),
		filepath.Join(a.Paths.Pairs, a.Name+".json"),
		filepath.Dir(LogFile(a.Name, "x")),
		filepath.Join(a.stateRoot(), a.Name), // last: it holds the agent's maintenance lock
		folder,
	} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return Failure{"delete", "local_state_remove_failed: " + path}
		}
	}
	a.Printer.Print("delete", progress.OK, a.Printer.Agent+" is gone and its name is free")
	a.Printer.Print("delete", progress.Info, "troupe cannot do this part: revoke the keys "+a.Name+
		" used at their providers, and delete its Slack app if it had one")
	return nil
}

// sources is the repository where sbx keeps a local kit's build source, tagged NAME-<12 hex>
// after the sandbox it built.
const sources = "docker.io/library/sbx-kit-src"

// removeSources removes the build sources sbx kept for this sandbox's own kit, matched by the
// exact tag shape sbx gives them (another agent's, such as NAME-2's, cannot match).
func (a *Agent) removeSources(ctx context.Context) {
	out, err := a.Runner.Output(ctx, sbx.Call{Args: []string{"template", "ls", "--json"}, Timeout: time.Minute})
	if err != nil {
		return
	}
	var templates []struct{ Repository, Tag string }
	if json.Unmarshal(out, &templates) != nil {
		return
	}
	own := regexp.MustCompile(`^` + regexp.QuoteMeta(a.Name) + `-[0-9a-f]{12}$`)
	for _, t := range templates {
		if t.Repository == sources && own.MatchString(t.Tag) {
			a.Runner.Output(ctx, sbx.Call{Args: []string{"template", "rm", "--force", sources + ":" + t.Tag}, Timeout: time.Minute})
		}
	}
}

// image is the image sbx built a sandbox from, or "" when it cannot tell. Docker Hub's default
// prefix is dropped, so one image compares equal however sbx writes its name.
func (a *Agent) image(ctx context.Context, name string) string {
	out, err := a.Runner.Output(ctx, sbx.Call{Args: []string{"inspect", name, "--json"}, Timeout: time.Minute})
	if err != nil {
		return ""
	}
	var one struct {
		Image string `json:"image"`
	}
	var many []struct {
		Image string `json:"image"`
	}
	if json.Unmarshal(out, &many) == nil && len(many) == 1 {
		return strings.TrimPrefix(many[0].Image, "docker.io/library/")
	}
	if json.Unmarshal(out, &one) == nil {
		return strings.TrimPrefix(one.Image, "docker.io/library/")
	}
	return ""
}

// removeImage removes the image built for a VM that is gone, by its exact name, when it is an
// assembled one and no remaining sandbox uses it; anything uncertain keeps it. It reports
// whether the image was removed.
func (a *Agent) removeImage(ctx context.Context, step, image string) bool {
	if !strings.HasPrefix(image, assembled) {
		return false
	}
	records, err := a.Runner.Inventory(ctx)
	if err != nil {
		return false
	}
	for other := range records {
		if used := a.image(ctx, other); used == "" || used == image {
			return false
		}
	}
	if _, err := a.Runner.Output(ctx, sbx.Call{Args: []string{"template", "rm", "--force", image}, Timeout: 5 * time.Minute}); err != nil {
		if ctx.Err() == nil { // an interrupted removal is retried by the agent's next reset; after a resize the receipts are already filed, so troupe delete removes a leftover
			a.Printer.Print(step, progress.Info, fmt.Sprintf("The VM's image %s stays: sbx would not remove it", image))
		}
		return false
	}
	return true
}

// removeReplaced removes the image of the VM this transaction replaced, recorded before that
// VM's removal, once the new VM runs, under removeImage's rule: an image any sandbox runs, the
// new VM included, stays. Best effort: it never fails the transaction.
func (r *recreation) removeReplaced(ctx context.Context) {
	var authorized struct {
		OldImage string `json:"old_image"`
	}
	readReceipt(r.t.file("removal-authorized.json"), &authorized)
	if authorized.OldImage == "" {
		return
	}
	if r.removeImage(ctx, r.label, authorized.OldImage) {
		r.Printer.Print(r.label, progress.Info, "Removed the replaced VM's image "+authorized.OldImage)
	}
}
