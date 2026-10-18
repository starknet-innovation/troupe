//go:build dev

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// kitsSetting is the dev build's kits root: the folder holding a maintainer's agent kits.
var kitsSetting = &Setting{
	Key:    "kits",
	stored: "string",
	Help:   "the kits root (a maintainer's agent kits) to use when no --kits names one",
	parse: func(text string) (any, error) {
		root, err := filepath.Abs(text)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			return nil, errors.New("an existing folder")
		}
		return root, nil
	},
}

// Resolve picks the kits root: the --kits flag, else the machine's kits setting.
func Resolve(flagDir string) (Kits, error) {
	dir, fromSetting := flagDir, false
	if dir == "" {
		machine, err := LoadMachine()
		if err != nil {
			return Kits{}, err
		}
		dir, fromSetting = machine.Kits, machine.Kits != ""
	}
	if dir == "" {
		return Kits{}, fmt.Errorf("%w: pass --kits DIR, or run: troupe config set kits DIR", ErrNoKits)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return Kits{}, err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if fromSetting {
			return Kits{}, fmt.Errorf("kits root is not a folder: %s (the kits machine setting; run: troupe config set kits DIR)", root)
		}
		return Kits{}, fmt.Errorf("kits root is not a folder: %s", root)
	}
	return Kits{Root: root}, nil
}
