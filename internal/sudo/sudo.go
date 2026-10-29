// Package sudo asks for sudo once, before a command's privileged work.
package sudo

import (
	"context"
	"io"
	"os"
	"os/exec"
)

// Ask makes sudo usable without a prompt for what follows. A sudo that already runs commands
// without a password (NOPASSWD, or a credential still cached) asks nothing; otherwise sudo -v
// asks on stdin. sudo-rs, Ubuntu 26.04's sudo, asks for a password on -v even under NOPASSWD,
// which a user with no password (EC2's ubuntu) can never give, so -v comes only second. A cached
// credential is still refreshed, without a prompt, as -v did.
func Ask(ctx context.Context, stdin io.Reader) error {
	if exec.CommandContext(ctx, "sudo", "-n", "true").Run() == nil {
		exec.CommandContext(ctx, "sudo", "-n", "-v").Run() // NOPASSWD under sudo-rs refuses it: harmless
		return nil
	}
	verify := exec.CommandContext(ctx, "sudo", "-v")
	verify.Stdin, verify.Stdout, verify.Stderr = stdin, os.Stderr, os.Stderr
	return verify.Run()
}
