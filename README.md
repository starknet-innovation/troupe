# troupe

Manage a troupe of Hermes agents, each in its own Docker Sandbox.

**In development.** Commands land one milestone at a time; today troupe offers `chat`,
`config`, `create`, `delete`, `doctor`, `guard`, `reset`, `resize`, `restart`, `secret`,
`start`, `stop` and `version`. Run `troupe --help` for the current list. Building troupe itself
and maintaining agent kits: [docs/development.md](docs/development.md).

## Install

troupe is not published yet. Once it is, one command installs it:

```bash
curl -fsSL https://raw.githubusercontent.com/starknet-innovation/troupe/main/install.sh | sh
```

Until then, install a troupe you built ([Build](#build)) the same way, from this checkout:

```bash
TROUPE_BINARY=/path/to/troupe sh install.sh
```

Run it as the user who will own the agents, not root. The installer puts troupe in
`~/.local/bin`, runs `troupe doctor`, and installs troupe's two systemd units
(`sbx-daemon.service`, which runs the sandbox daemon, and `hermes-gateway@.service`, each agent's
gateway), asking for sudo only when they are missing or changed, or the daemon is not running
under its unit. It refuses while a sandbox daemon runs outside that unit (stop it with `sbx daemon
stop` first), and leaves the units out until sbx is installed (run it again then). A machine that
fails the doctor, or any failure or interruption, leaves the troupe binary that was there before;
units already written stay, and a later run finishes them. A downloaded release is checked against the release's `SHA256SUMS`, which
catches a corrupted download; it comes from the same place as the binary.

`troupe doctor` checks this machine, one line per requirement; `create` runs the same checks
first and refuses on any failure or requested action (a note, such as an older systemd, never
stops it):

| Kind | Requirement | If missing |
|---|---|---|
| Hardware | an x86_64 CPU | fails |
| Hardware | KVM (`/dev/kvm`) usable by you | fails |
| Hardware | room for a coder agent at its default size (8 CPUs, 12 GiB memory, 120 GiB disk) beyond the reserves | noted; `create` checks each agent's own sizes, so a smaller one may still fit |
| Keys | how keys will be kept, strongest available ([key protection](#key-protection)) | noted below a TPM; `create` asks |
| Software | systemd (one older than 259, the version troupe is tested on, is noted) | asks for your action |
| Software | Docker with buildx | asks for your action |
| Software | sbx 0.48 or newer | asks for your action |
| Software | sudo | asks for your action |
| Software | troupe's systemd units installed and current, running the sandbox daemon | asks for your action (run the installer again) |

## Creating an agent from a published kit

```bash
troupe create hermes --name jane
```

creates a new agent from the latest published kit of an agent type, with no kits checkout.
The types are `coder` and `hermes`; `troupe create TYPE --help` shows each one's sizes and what it asks for.
troupe pulls `REGISTRY/TYPE:latest` (the `registry` machine setting) and pins the version it
names: the agent is composed from `REGISTRY/TYPE:VERSION`, whose tag never moves, never from
`latest`, so a later version changes it only when you run `troupe upgrade`. It keeps that kit's `/usr/share/troupe/` files (the services the type
needs, its default sizes and SOUL, its tools and a copy of its seeds) in troupe's data folder,
asks for each service's key in the kit's order, after the kit's line on what the key is and
where to get it (hidden; Enter skips an optional one), and stores
it as strongly as the machine allows ([key protection](#key-protection)), writes the agent's environment to `~/.local/share/troupe/agents/NAME/`
(`sbxenv.yaml`, its mixin, `storage.env`, `source.json`), then runs the same create as below.
`--cpu`, `--memory` and `--storage` change the kit's default sizes; `--soul FILE` replaces its
default SOUL. Every `create` ends with a self-test: one message through the agent's own Hermes
and model provider, judged on transport only (the configured model completed a turn), with the
reply shown. A failure names the likely cause (the key, the network, the provider); the agent
stays created.

**Names.** `--name` is the name people read, kept as typed ("Mr. Doyle"): troupe uses it in the
agent's SOUL, its Slack app and its progress lines, and records it in `source.json`. Every
command takes the ID troupe derives from it: lowercase ASCII letters and digits, accents folded,
every other run of characters one hyphen ("Mr. Doyle" is `mr-doyle`). A name holds at most 35
characters, Slack's limit for an app.

**Slack.** A type whose kit ships a Slack app manifest (`coder`) sets Slack up at `create`: troupe
prints a link that opens Slack's create-from-manifest flow pre-filled with the whole manifest
(the whole address, which every terminal shows; no file is written), with the steps after it, then asks for the app-level and bot tokens (hidden) and the
member ID of the agent's manager (usually yours). The manager starts as the only person the
agent talks to, and once they DM it, its notices go to that DM (the bot cannot open a DM
itself). From then on the manager runs it from
Slack: they admit other people (who may give it work, not change its configuration), point it
at channels, and hand it over to a new manager by telling it so, with no command on this
machine. troupe checks only the shape of these live settings: a manager on an allowlist of
member IDs, and a home that is never a boundary channel. A `reset` starts the agent from what
`create` was given, so the manager then tells it again; `resize` keeps them. The agent keeps its own `slack.env`
in its folder, holding the workspace its bot token belongs to, learned and pinned at its first
Slack setup. A `create` or `reset` also checks this machine's boundary channels, if set (`troupe config
set slack-boundary-public C…` and `slack-boundary-private`), which the agent must be unable to
read or post in; unset, no boundary is checked. A `resize` keeps the Slack app and tokens, so it
checks only the new VM's side and does not probe them again: run `troupe reset` to apply a boundary
set later. A kit's `env` file (`coder`'s) starts the agent's
`.env`.

**Kit settings.** A kit may also declare settings (`settings.json`): values that are not keys,
which `create` asks for before the keys, shown as typed, each after the kit's line on what it is.
Every one is optional (Enter leaves it empty), and an answer must have the form the kit declares.
Each answer becomes one line of the agent's `.env`, after the Slack lines. Like the Slack
settings, a `reset` starts the agent from what `create` was given, `resize` and `upgrade` keep
what the agent holds then, and a setting a newer kit version adds is never asked by `upgrade`:
the kit decides what an agent without it does.

**Optional keys.** Only the placeholder services an agent was given keys for are recorded (in
`source.json`) and wired; `troupe secret set ID SERVICE` records one the kit declares later.

**`--avatar PATH|URL`** gives the agent a profile picture: a file, or an `https` address fetched
once (no plain http, even by redirect), a PNG or JPEG of at most 5 MiB, checked by its first
bytes and never decoded on the host. It becomes the agent's seed `assets/profile-picture.png`
(or `.jpg`), its SOUL says where it is so the agent can look at it. A manifest cannot carry the Slack app's
icon, so the Slack steps say to upload the same picture by hand (download it first if you gave an
address); nothing is copied to your folder.

Day to day:

| Command | What it does |
|---|---|
| `troupe chat NAME` | Talks to a running agent from this terminal: Hermes's own chat in its VM. It works with or without Slack. |
| `troupe reset NAME [--check]` | Factory reset after you confirm: the agent starts fresh from the kit version it is pinned to (the one it was created from, or upgraded to), keeping itself and its keys. `--check` only proves a reset would pass its preflight. The old VM's image is removed once the new VM runs. |
| `troupe delete NAME` | Removes the agent after you type its name as shown ("Mr. Doyle", not `mr-doyle`): its VM and the VM's image, its keys, receipts, recovery backups, logs and the folder troupe wrote for it. The name is then free. It reminds you to revoke its keys at their providers and delete its Slack app. A sandbox troupe has no record of (a Lab, another tool's) is refused. |
| `troupe upgrade NAME` | Moves an agent created from a published kit onto the newest version of its kit (same MAJOR), keeping everything it has, then makes its seeded skills and scripts match that version; each edit of the agent's own that a seed replaces is kept in `~/.local/state/live-apply/NAME/`, so harvest its lessons into the kit before publishing that version. An agent already on the newest version is left as it is. Asks for any key the new version requires, and refuses a changed Slack app manifest or a new MAJOR (recreate the agent then). |
| `troupe resize NAME [--cpu N] [--memory GiB] [--storage GiB]` | Changes an agent's sizes, keeping everything it has: troupe records them and moves the agent into a new VM at them. The root disk can only grow. With no sizes it is a plain move into a fresh VM. If the move fails before it begins, the old sizes stay. The old VM's image is removed once the new VM runs. |

`reset` and `delete` ask you to confirm, then for sudo before any work; `chat` needs no sudo.

### The resource budget

`create`, `start`, `resize` and `reset` first compare what the agent
needs with what the machine has, counting every sandbox sbx runs, troupe's or not:

| Resource | Counted | Available |
|---|---|---|
| Memory | every running VM's declared memory, plus 1.25 GiB each (a full VM's host memory peaks about 1 GiB above its guest size, and its `sbx exec` process holds about 100 MiB) | the machine's memory, minus the sandbox daemon's own and a 2 GiB reserve |
| CPUs | every running VM's vCPUs | the machine's CPU threads (`nproc`) and half again: VMs idle between bursts, so shared CPUs only slow a burst down (memory is never overcommitted) |
| Disk | what every VM disk may still grow (they are sparse), plus a new agent's disks | the free space on their filesystem, minus 10% of it |

A gateway unit that is up counts as running, since it boots its VM again within seconds. Over
budget, troupe shows by how much and asks you to confirm; `--force` confirms, for scripts, and
`reset --check` reports it as a failure without asking. It refuses what cannot work at all:
one agent wanting more memory or CPUs than the machine has, and a running sandbox whose size sbx
does not report. A host figure it cannot read (memory, the disks' filesystem) is noted and not
checked.

The registry needs one token that can read the kits (on GitHub, a classic token with
`read:packages`). sbx pulls the VM's image only with its own registry credential, and troupe
reads the kit through Docker, so the first `create` asks for the token once and stores it in
both (`sbx secret set --registry`, `docker login`), each on stdin.

## Resetting and resizing

`troupe reset NAME` lets the agent finish its current work, deletes its VM and creates a fresh
one from its kit and seeds, wires its keys again, sets up Slack and its kit, and starts it.
`troupe resize NAME` moves the agent into a new VM and keeps its state: a quiesced Hermes backup
and a workspace archive of the old VM, its removal (authorized only by that backup), the new
VM, the state and workspace restored, keys and Slack wired (the Slack app's scopes and boundary
were proven at create, so a move checks only the VM's side), then one model turn through the
moved agent proves it works; nothing asks you after sudo. Hermes is updated in the new VM only
when its image lacks the agent's commit. The quiesced pre-move backup stays with the receipts
as the restore point. Each step of either leaves a receipt, so an interrupted run resumes when
run again.

`troupe reset NAME --check` proves the reset's preflight would pass (sandboxd, the keys, the
units, the machine's budget and the kit) and changes nothing but the host's build cache: no
prompt, no sudo, no exec into the VM. `--yes` answers reset's question, for scripts; `--force`,
as in every command, only confirms an agent over the machine's budget.

## Secrets

`troupe secret set AGENT SERVICE` asks for a key without showing it, stores it at the agent's
[key protection](#key-protection) and gives the agent's sandbox access to it; `troupe secret
check AGENT` proves every key stored, readable by sandboxd and wired, without reading one. An agent's
services are those its kit declares (`services.json`): native ones, which sbx injects for the
hosts named, and placeholders, which the agent sees as a stand-in value that sbx replaces on
the way out.

### Key protection

Keys live under `~/.local/state/credentials/NAME/`, in private folders. `create` keeps each new
agent's keys the strongest way the machine offers, records it in that folder's `tier` file
(none means TPM: agents created before tiers), and keeps it for the agent's life:

| Tier | When | Key file | sandboxd reads it with | A copy of the disk |
|---|---|---|---|---|
| TPM | `has-tpm2` reports full support (`systemd-analyze`, or `systemd-creds` on 256), systemd 256+ | `SERVICE.cred`, sealed by the TPM and the host key | `systemd-creds --user decrypt` | cannot read it |
| Host key | `systemd-creds` without a usable TPM, systemd 256+ | `SERVICE.cred`, encrypted with the root-only host key | `systemd-creds --user decrypt` | can read it (the host key is on it) |
| File | neither | `SERVICE.key`, plain, read only by you (`0400`) | `cat` | can read it |

Below the TPM, `create` says what the keys will get and asks, before sudo or any other setup;
anything but yes, or no terminal to ask on, stops it with nothing done. No flag skips that
question. Each tier accepts only its own file format, so a weaker file never stands in for a
stronger agent's key. In every tier, anything running as your user, or as root, can read the
keys: the TPM protects against a copied disk, not against this machine's own accounts.

## Security model

Each agent fully controls its own VM, so everything that comes out of one is untrusted:
answers, files, logs, names. troupe holds these lines:

- A compromised agent cannot harm the host, other agents or the operator's data. A
  compromised agent breaking only its own `reset` or `resize` is accepted: it can already
  break itself.
- The host never parses VM data beyond small, size-capped JSON answers, decoded into typed
  values with counted lists. Archives from a VM are only hashed and stored on the host. They
  are checked and unpacked inside a VM, with the same parser the restore uses.
- Every stream from a VM is size-capped and never writes into a free-space reserve. Every
  wait has a deadline.
- VM text reaches a terminal or a log only escaped. Secrets travel only on stdin, or in the
  environment of the one host tool that uses them, never in arguments, logs or output.
- `troupe chat` is the one exception to escaping: it is the operator's own session with the
  agent, like `sbx exec -it`, so the chat's bytes reach the terminal raw. It runs only for an
  agent that is already running.
- A published kit is the publisher's, trusted because the operator chose the
  registry. Its files still reach the host only as a size-capped stream of plain files and
  folders, none outside troupe's folder for that kit, and an agent is pinned to the version it
  was created from until you run `troupe upgrade`.

## Build

troupe is one static binary with no runtime dependencies. Building needs Go 1.26 or newer:

```bash
CGO_ENABLED=0 go build -o ~/.local/bin/troupe ./cmd/troupe
```

## Where troupe keeps things

An agent's environment lives in troupe's data folder (`$XDG_DATA_HOME/troupe`, else
`~/.local/share/troupe`): `agents/NAME/` composes its published kit, pinned by version, and
itself; troupe keeps that kit's files in `kits/TYPE-VERSION/`. Machine settings (`timezone`,
`collector-timer`, `registry`, `slack-boundary-public`, `slack-boundary-private`) live in
`~/.config/troupe/config.json`; change them with `troupe config`. Keys are kept under
`~/.local/state/credentials/NAME/` ([key protection](#key-protection)).

## Host and guest

On the host troupe needs only Linux with systemd, `sbx` and sudo (`start` and `stop` ask for it
once), ideally `systemd-creds` with a TPM for keys ([key protection](#key-protection)), and the docker CLI with buildx for `create`,
`reset` and `resize` (`create` pulls the agent's published kit to read it; `reset` and `resize`
also build the agent's own small kit into the host's build cache, the one `sbx env create` uses,
before the old VM is removed);
it never runs Python there. Inside each VM it
installs its guest jobs (`guest/`) and runs them on Hermes's own Python, because they call
Hermes's functions.
