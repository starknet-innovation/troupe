//go:build !dev

package agent

import "testing"

// checkInstalled is the dev build's: a release has no update to read the receipts.
func checkInstalled(*testing.T, string, string) {}
