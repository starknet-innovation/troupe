# Developing troupe and agent kits

troupe has two builds from one source tree:

- **The release** (`go build`): the end-user commands in the [README](../README.md), and nothing
  else. A release has no kits root and no maintainer command.
- **The dev build** (`go build -tags dev`): the same `troupe` with a maintainer's commands and a
  kits root of local agent kits. It is built from the checkout and never released; `troupe
  version` ends with "(dev build)".

```bash
CGO_ENABLED=0 go build -tags dev -o ~/.local/bin/troupe ./cmd/troupe
```

Files that only the dev build compiles start with `//go:build dev`; the in-VM jobs only it ships
are listed in `embed_dev.go` (the rest in `embed.go`).

## Maintainer commands

| Command | What it does |
|---|---|
| `troupe agents list\|show\|materialize\|slack-manifest` | Lists the agents of the kits root and published agents, shows an agent's kits and seeds, writes its composed seed home, prints its Slack app manifest. |
| `troupe update skills\|scripts\|soul AGENT` | Puts seed changes into a running agent without a reset. Your seeds win: it overwrites the agent's own edits to those files and keeps each overwritten copy in its receipt. Needs a finished reset or create. |
| `troupe harvest AGENT` | Read only collection of the agent's own skill edits, before a reset, update or upgrade replaces them; it prints where they are and the next step (blend them into the kit by hand or with an LLM, then update or upgrade); any agent, one created from a published kit included (its seeds are the pinned kit's). |
| `troupe storage check AGENT` | Checks a sandbox's disks and mounts against its declared storage. |
| `troupe budget check --name NAME --cpu N --memory GiB --disk GiB [--count N]` | Whether this machine has room for a new sandbox (or `--count` of them at once), counted as `create` counts a new agent: exit 0 when it fits, 3 when over budget (with the figures; the caller decides), 1 when it can never fit or the check could not run. Never asks; other tools (such as a Lab's) call it before creating a sandbox. |

A local kits root is trusted: it is the maintainer's own checkout, read straight from disk. Its
tools still run only inside the agent's VM, never on the host.

`reset` and `resize` work on local agents too. A local agent's sizes are its own folder's files
(CPUs and memory in `sbxenv.yaml`, the disks in `storage.env`; sbx has no disk field in
`sbxenv.yaml`): edit them, then `troupe resize AGENT` with no sizes moves it into a new VM at
them. `troupe create` makes agents only from published kits (`troupe create TYPE --name NAME`).

## Kits root

The dev build reads local agent kits from a kits root: `--kits DIR`, else the `kits` machine
setting (`troupe config set kits DIR`). Kits are Docker Sandbox v3 kits:

- An agent type is a workload kit (`kits/coder`): a folder holding a descriptor named after it
  (`coder.yaml`, `kind: workload`; sbx names a local kit after its folder), its recipe
  (`coder.dockerfile`, which names Hermes with one line `ARG HERMES_VERSION=X.Y.Z`), its
  seeds under `files/home/.hermes` and optionally `tools/seed-check.py`, `tools/setup.py`,
  `secrets.json`, `slack.env` and `slack-app-manifest.json`.
- An agent is a mixin on it plus its environment (`agents/ada`): `ada.yaml` (`kind: mixin`,
  `requires: ["coder"]`), `ada.dockerfile`, its seeds, `storage.env`, and an `sbxenv.yaml`
  whose `name:` is the folder's and whose `kits:` are the workload (a relative `../` path)
  followed by itself (`.`).
- A Lab is never an agent: it lives in a folder named after it (`labs/coder-lab`), its name
  ends in `-lab`, and it may compose the workload alone.

Agents may sit up to three levels below the root; two agents with one name are refused, as is a
name used by both a local and a published agent. [`examples/kit`](../examples/kit) holds a
workload kit, two agents and a Lab:

```bash
troupe --kits examples/kit agents list
```

A local agent's placeholder services are those its base kit declares in `secrets.json`:

```json
{"placeholders": [{"service": "slack-bot", "env": "SLACK_BOT_TOKEN",
  "hosts": ["slack.com"], "placeholder": "sbx-cs-{agent}-slack-bot"}]}
```

## A kit's own steps

A base kit may add its own steps to `create` and `reset`, which troupe runs inside the VM on
Hermes's Python with the agent's composed seeds beside them:

- `tools/seed-check.py validate|check`: the kit's seed rules, run in the current VM before any
  downtime when it is running (validate, which only catches honest mistakes: the old VM is the
  agent's) and in the new VM before the first start (check, the one that counts);
- `tools/setup.py run`: setup before the first start, such as scheduled jobs.

Both kits are built into the host's build cache while the old VM still runs, so a kit that does
not build stops a reset before anything is removed. A resumed reset enables the gateway again
before starting it, so a `troupe stop` between attempts never leaves a finished agent off at
boot.

## Releasing

The recipe (first release: v0.1.0):

```bash
git tag vX.Y.Z
CGO_ENABLED=0 go build -trimpath -o troupe-linux-amd64 ./cmd/troupe   # the release: no -tags dev
sha256sum troupe-linux-amd64 > SHA256SUMS
```

then a GitHub release for the tag with both files, which `install.sh` downloads from
`releases/latest/download`. A binary built at a clean tag says `troupe X.Y.Z`; one built
anywhere else says its commit.

`troupe units render NAME` (hidden) prints a unit with this host's values; the installer and
a host's own upgrade tools can use it.

## Tests

Run both builds' tests; the dev build's cover the maintainer commands and the code they share
with the release.

```bash
go vet ./... && go test ./...
go vet -tags dev ./... && go test -tags dev ./...
python3 -m unittest discover -s guest/tests
```
