// Package troupe holds the files the binary carries: the guest jobs it installs into each
// agent's VM and the systemd unit templates it renders.
package troupe

import "embed"

// Guest holds the in-VM jobs every build runs, on Hermes's own Python; GuestDev (embed_dev.go)
// adds the dev build's.
//
//go:embed guest/__main__.py guest/align.py guest/archive.py guest/backup.py guest/drain.py guest/fresh.py guest/kit.py guest/ready.py guest/restore.py guest/selftest.py guest/slack.py guest/stop.py guest/stopped.py guest/storage.py guest/update.py guest/install.py
var Guest embed.FS

// Units holds the systemd unit templates.
//
//go:embed systemd/*.service
var Units embed.FS
