//go:build dev

package agent

import "os"

// The dev build lets a test or a maintainer force the guest's install to fail after its first
// write (TROUPE_FORCE_INSTALL_FAILURE=1), to prove the rollback; a release never does.
func init() {
	forceInstallFailure = func() bool { return os.Getenv("TROUPE_FORCE_INSTALL_FAILURE") == "1" }
}
