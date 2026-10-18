//go:build dev

package troupe

import "embed"

// GuestDev holds the dev build's in-VM job: the skill harvest.
//
//go:embed guest/harvest.py
var GuestDev embed.FS
