//go:build !dev

package troupe

import "embed"

// GuestDev is empty: a release build ships no dev job.
var GuestDev embed.FS
