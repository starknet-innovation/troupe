// Package guest runs troupe's guest jobs inside an agent's VM.
//
// The jobs are Python run on Hermes's own interpreter, because they call Hermes's own
// functions; the host never runs them. Every call sends this build's job files afresh and
// runs them from a private temporary folder that is removed afterwards: nothing persists in
// the VM, so neither the agent nor an older build can edit troupe's job files ahead of time.
//
// A job still runs as the agent's own user, on the VM's interpreter and tools, so what it
// reports is what the VM shows. That catches misconfiguration; it is no proof against a
// compromised agent, which controls the VM. Only host-side facts (sbx's inventory, systemd,
// containerd's state) are.
package guest

import (
	"archive/tar"
	"bytes"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/starknet-innovation/troupe"
	"github.com/starknet-innovation/troupe/internal/sbx"
)

// Python is the image's Debian interpreter, which asks Hermes's own package manager (pm/) for
// the environment Hermes runs on; each native update may commit a new one.
const Python = "/usr/bin/python3"

type file struct {
	name string
	data []byte
}

// File is one extra file a call sends beside the jobs, at a path relative to their folder
// (kit tools under kit/, an agent's composed seeds under seeds/).
type File struct {
	Name string
	Data []byte
}

// files are the jobs this build ships: every build's, and the dev build's.
func files() ([]file, error) {
	var found []file
	for _, jobs := range []embed.FS{troupe.Guest, troupe.GuestDev} {
		err := fs.WalkDir(jobs, ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := fs.ReadFile(jobs, name)
			if err != nil {
				return err
			}
			found = append(found, file{name: path.Base(name), data: data})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	return found, nil
}

// archive is the job files as a tar stream with fixed metadata, so the same build always
// sends the same bytes. A request, when given, travels as request.json beside the jobs, so
// its size is not limited the way a command line is; extra files travel the same way.
func archive(request []byte, extra []File) ([]byte, error) {
	all, err := files()
	if err != nil {
		return nil, err
	}
	if request != nil {
		all = append(all, file{name: "request.json", data: request})
	}
	for _, f := range extra {
		if !local(f.Name) {
			return nil, fmt.Errorf("guest file outside its folder: %q", f.Name)
		}
		all = append(all, file{name: f.Name, data: f.Data})
	}
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, f := range all {
		header := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(f.data); err != nil {
			return nil, err
		}
	}
	// Close writes the end-of-archive blocks, so it must run before the bytes are taken.
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runScript unpacks the job files from stdin into a fresh private folder, runs the job on the
// interpreter Hermes itself uses and removes the folder; the job's exit status is the
// script's. Arguments: the image's Python, Hermes's folder, then the job and its arguments.
// An answer from PM that names no interpreter fails the call.
const runScript = `set -eu
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
tar -x -C "$dir"
hermes=$2
python=$("$1" -I -c 'import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from pm.environments import project_python
print(project_python(Path(sys.argv[1])))' "$hermes" 2>/dev/null) || python=
shift 2
if [ -z "$python" ] || [ ! -f "$python" ] || [ ! -x "$python" ]; then
  echo '{"status": "failed", "class": "hermes_python_unknown"}'
  exit 1
fi
"$python" -B "$dir" "$@"`

// Hermes is Hermes's source folder in every agent VM; jobs run from it, as Hermes's own
// tools do.
const Hermes = "/opt/hermes"

// Run sends the job files and runs job in the VM in one sbx exec, returning its stdout.
func Run(ctx context.Context, runner sbx.Runner, name, job string, args ...string) ([]byte, error) {
	return RunWithin(ctx, runner, 120*time.Second, name, job, args...)
}

// RunWithin is Run with its own deadline for the whole call.
func RunWithin(ctx context.Context, runner sbx.Runner, timeout time.Duration, name, job string, args ...string) ([]byte, error) {
	return RunRequest(ctx, runner, timeout, name, nil, job, args...)
}

// RunRequest is RunWithin with a request the job reads from request.json.
func RunRequest(ctx context.Context, runner sbx.Runner, timeout time.Duration, name string, request []byte, job string, args ...string) ([]byte, error) {
	return RunFiles(ctx, runner, timeout, name, request, nil, job, args...)
}

// RunFiles is RunRequest with extra files beside the jobs.
func RunFiles(ctx context.Context, runner sbx.Runner, timeout time.Duration, name string, request []byte, extra []File, job string, args ...string) ([]byte, error) {
	stream, err := archive(request, extra)
	if err != nil {
		return nil, err
	}
	argv := append([]string{"exec", "-i", "--workdir", Hermes, name, "sh", "-c", runScript, "sh", Python, Hermes, job}, args...)
	return runner.Output(ctx, sbx.Call{Args: argv, Stdin: bytes.NewReader(stream), Timeout: timeout, Private: true,
		Limit: answerLimit(job, args)})
}

// MaxFilesAnswer caps update read's answer, which carries the agent's seeded files as base64
// (about 0.4 MiB for the coder kit today); the host decodes it as a counted stream.
const MaxFilesAnswer = 16 << 20

// fileJobs are the jobs that answer with files, and how much each may print; the dev build
// adds its own.
var fileJobs = map[string]int64{"update read": MaxFilesAnswer}

// answerLimit is how much a job may print: one short JSON line, except the jobs that return
// files.
func answerLimit(job string, args []string) int64 {
	key := job
	if len(args) > 0 {
		key += " " + args[0]
	}
	if limit, ok := fileJobs[key]; ok {
		return limit
	}
	if limit, ok := fileJobs[job]; ok {
		return limit
	}
	return sbx.MaxAnswer
}

// local is a clean relative path that stays inside the jobs' folder.
func local(name string) bool {
	return name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." &&
		!strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\x00\\")
}
