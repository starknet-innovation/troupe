//go:build !dev

package config

// kitsSetting is nil: a release build has no kits root.
var kitsSetting *Setting
