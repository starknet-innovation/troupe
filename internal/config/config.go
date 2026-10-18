// Package config reads this machine's settings and, in troupe's dev build, finds the kits root
// (the folder holding a maintainer's agent kits).
package config

import "errors"

// Kits is a resolved kits root.
type Kits struct {
	Root string
}

// ErrNoKits means no kits root is named. That is normal for an end user, whose agents all come
// from published kits, and always so in a release build, which has no kits root at all.
var ErrNoKits = errors.New("no kits root")
