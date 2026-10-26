# Design note: `troupe create` from published kits

Status: step 1 done; step 2a (`create TYPE --name` from a published kit, proven with `hermes`) done; step 2b (`reset`, `delete`, `chat`, the self-test) done; step 2c (the resource budget, `resize`) done; step 2d (the coder type for end users: GitHub and Slack, names, `--avatar`) built (2026-10-14).

## Goal

An end user with Docker, sbx, a host (ideally with a TPM) and a GitHub token that can read the kits creates an agent with one command and answers a few prompts:

```
troupe create coder --name jane
```

Nothing to clone, no files to write. Today the same result takes a `hermes-agents` checkout and six hand-written files per agent. The maintainer workflow (kits from a local checkout) keeps working unchanged.

## Commands

The end user's whole surface:

| Command | When |
|---|---|
| `troupe create TYPE --name NAME [--cpu N] [--memory GiB] [--storage GiB] [--soul FILE]` | A new agent, always from the latest published `TYPE` kit ([how `latest` is resolved](#the-latest-tag)). It writes the agent's environment, asks for its keys (hidden, kept as strongly as the machine allows: TPM sealed, else after the user's yes, host key only or a plain file), creates the VM and starts it. It ends with a self-test: one message through the agent's model provider, with the reply shown. If the turn fails, it says exactly what's broken (key, network, provider), using the same deterministic check as the Labs' `check.sh`. |
| `troupe chat NAME` | Talk to the agent from the terminal: Hermes's own chat UI inside the agent's VM (`sbx exec -it NAME hermes --tui` underneath). It works with or without Slack, and on EC2 over SSH. |
| `troupe resize NAME [--cpu N] [--memory GiB] [--storage GiB]` | Change an agent's resources, keeping everything it has. sbx can't resize a VM, so this records the new sizes (only the flags given change) and moves the agent into a new VM on the same kit version. That's a new VM with the agent's memories, sessions, workspace and learned skills moved in, so no harvest is needed. It refuses a size the machine can't hold, or a disk smaller than the agent's data. |
| `troupe upgrade NAME` | Move the agent onto the newest version of its kit, keeping everything it has (see below). |
| `troupe start` / `stop` / `restart NAME` | Day to day, as today. |
| `troupe secret set NAME SERVICE` | Rotate a key, as today. |
| `troupe reset NAME` | Rare factory reset, which replaced `recreate`. It keeps the agent and its keys, but first warns that it erases the agent's memory, sessions, workspace and learned skills, and asks to confirm. |
| `troupe delete NAME` | Remove the agent entirely. You confirm by typing its name as shown. It stops the agent and removes its VM and disks, then its local state: keys, generated folder, troupe's records and its start-at-boot unit. It then frees the name for a new `create`, and lists what troupe can't do for you: revoke the tokens it used, at their providers, and delete its Slack app. |

`upgrade` (decided 2026-10-17, replacing an earlier "no upgrade command": skills harvested from agents into the kit must reach existing agents, not only new ones). An agent still keeps its own Hermes current inside its VM; what only an upgrade brings is a newer kit version's image, tools and seeds:
- **Only a newer version of the same MAJOR.** Equal leaves the agent as it is (its lessons cannot be in a version yet), unless the last upgrade stopped before its seed step, which it then finishes; older is refused; a new MAJOR (agents built on it break) means recreating the agent.
- **Keys first.** A required placeholder key the agent lacks is asked for and stored before anything moves; a missing native key or Slack token is refused, since a move cannot add a binding or a Slack app. A changed Slack app manifest is refused too: new scopes or events need the app changed in Slack.
- **Then the move and the seeds.** troupe re-pins the agent's folder (its `sbxenv.yaml` kits line and `source.json` reference and version), moves it into a new VM with resize's transaction (labeled `upgrade`), and, because that move restores the agent's own home over the new image's seeds, then applies the new version's skills and scripts with update's seed apply, keeping each replaced edit of the agent's own in that change's receipt. SOUL.md is the agent's own folder's and stays.
- **Seeds win.** The agent's seeded skills and scripts become the new version's. Its own lessons are meant to be in that version already: a maintainer harvests them (`troupe harvest`, dev build) into the kit before publishing it. Every replaced file is kept in the receipt.
- **Failure.** A move that never began puts the old pin back; one that began is finished by `troupe resize NAME`, and `troupe upgrade NAME` then makes the seeds match. If troupe is killed outright between the pin and the move, `troupe resize NAME` moves the agent onto the pinned version. The old version's kit files stay (another agent may use them).

`resize` and `upgrade` are the end-user reasons to move an agent to a new VM.

Today's rule that `create` refuses any name that ever existed changes: after `delete`, the name is free again.

### The release is end-user only; development commands live in the dev build

Decided 2026-10-14, replacing an earlier plan to port the development commands to Python scripts in hermes-agents (that would have duplicated troupe's kit composition and receipt reading in a second language).

One source tree, two builds:
- **The release** (`go build`) has only the end-user commands above, plus `config`, `guard probe` (which hermes-agents' scripts call before every sbx command) and later `doctor`. It has no kits root and carries no maintainer code: those files are `//go:build dev`, and the in-VM jobs only the dev build uses are embedded only there.
- **The dev build** (`go build -tags dev`, [docs/development.md](development.md)) is the same `troupe` with the maintainer commands and a kits root (`--kits`, else the `kits` setting). It's built from the checkout on a maintainer's machine and never released; hermes-agents' scripts require it.

| Before | After |
|---|---|
| `troupe recreate AGENT` | `troupe reset AGENT` in both builds; it works for local agents too, `--check` proves its preflight, `--yes` answers its confirmation for scripts (`--force` still only confirms the budget). |
| `troupe migrate AGENT` | `troupe resize AGENT` with no sizes: the same move into a fresh VM, keeping state, for local agents too (whose sizes are their own folder's files). |
| `troupe update`, `troupe agents`, `troupe storage check` | unchanged, in the dev build only |
| `troupe-dev harvest AGENT` | `troupe harvest AGENT`, in the dev build only; troupe-dev is gone |
| `--kits`, `TROUPE_KITS`, `troupe.json`, the `kits` setting | `--kits` and the `kits` setting, in the dev build only |
| `troupe units` | hidden: the installer installs the units with `troupe units render` |
| `troupe secret check AGENT` | kept: an end user verifying their keys |
| `max-running-agents` setting | retired: the resource budget replaces it (step 2c) |

### Defaults

Every flag is optional:

| Flag | Default | Source |
|---|---|---|
| `--cpu` | 8 (`hermes`: 4) | kit |
| `--memory` | 12 GiB (`hermes`: 6 GiB) | kit |
| `--storage` | 100 GiB root disk, plus 20 GiB for Docker | kit |
| `--soul` | the type's default SOUL | kit |

### Resource budget, not an agent count

`create`, `resize` and `start` compare what the agents would need against what the machine has, after a reserve for the host and the microVMs' own overhead:

| Resource | Counted | Reserve kept back | Why |
|---|---|---|---|
| Memory | sum of the running agents' declared memory | 2 GiB and the sandbox daemon's own memory, plus a per-VM overhead of 1.25 GiB | overcommitted memory ends with the host killing processes, possibly a VM |
| CPU | sum of the running agents' vCPUs | 1 core; it warns only above twice the cores | shared CPUs are only slower, never fatal |
| Disk | what every VM disk may still grow (declared size minus what it holds; they are sparse), stopped ones included, plus a new agent's disks | 10% of the disk | VM disks grow as they fill, so overcommit shows up later as "disk full" inside an agent |

Behaviour:
- **Over budget:** troupe prints what's committed, what's available and by how much it's over, then asks to confirm. `--force` stands in for the confirmation in non-interactive runs.
- **Physically impossible:** troupe refuses only what can't work at all, a single agent asking for more memory or CPUs than the machine has.

`max-running-agents` is retired: the budget counts every running VM, so it covers what the count was for. The per-VM overhead is 1.25 GiB, measured on this host: a full 8 GiB guest's VM process peaked at 9.03 GiB during a cold Rust build (its anonymous memory within the declared size, the rest the kit image's layers mapped in), and its `sbx exec` process settles to about 100 MiB.

## What `create` asks

Only what's truly per agent, in order. Pressing Enter skips an optional item, and every key is typed hidden.

| Key or field | Required? | Notes |
|---|---|---|
| Venice API key | Required | The model provider. |
| GitHub token | Required for `coder` | The agent finds its own login and ID inside its VM (`gh api user` through the proxy), so there's nothing more to ask and troupe never calls GitHub from the host. |
| Starkscan key | Required for `coder` (decided 2026-10-16): its `starkscan` skill assumes the key | |
| Slack app-level and bot tokens | Required for `coder` (decided 2026-10-14, to revisit); a type without a Slack app manifest (`hermes`) never asks | See [Slack](#slack) below. |

### Slack

Slack is part of the `coder` type: a coder agent maintains GitHub projects and talks with its team in Slack, so `create` sets Slack up for every coder agent (decided 2026-10-14, to revisit). Making it optional would mean changing the coder type's watcher, its always-loaded `maintainer` skill and adding Slack later; a plain terminal assistant is the `hermes` type, which has no Slack. `troupe chat` works beside Slack for every agent. A type sets Slack up only when its kit ships a Slack app manifest. The user never writes or edits a manifest. For Slack, troupe:

1. **Gives a one-click link** that opens Slack's "create app from manifest" flow, pre-filled with the kit's manifest and the agent's name: `https://api.slack.com/apps?new_app=1&manifest_json=<URL-encoded manifest>` (a link Slack documents). It writes no file: the link carries the whole manifest, so nothing is left in the operator's folder (decided 2026-10-16).
2. **Prints the remaining steps:**
   - pick the workspace, review, and **Create**;
   - under **Basic Information**, create an **App-Level Token** with `connections:write` and copy it (`xapp-…`);
   - **Install to Workspace**, then copy the **Bot token** (`xoxb-…`);
   - optionally, with `--avatar`, upload the same picture as the **App icon** (a manifest cannot carry one).
3. **Asks for** the two tokens (hidden) and the manager's member ID (usually yours), validates them against Slack, and wires them in. The manager starts as the only person the agent talks to, and their first DM becomes its home; they admit others, set channels and hand the agent over by telling it in Slack.

A finished `create` ends like this:

```
jane answered: "…"
Talk to her: troupe chat jane
```

The services and their order come from the kit, so another type asks for its own.

## Requirements, checked, never assumed

| Kind | Requirement | Why | If missing |
|---|---|---|---|
| Hardware | x86_64 CPU | the `coder` kit installs x86_64 builds | **fail** |
| Hardware | KVM (`/dev/kvm` usable by the user) | sbx's microVMs | **fail** |
| Keys | TPM 2.0, else `systemd-creds`, else a plain file | how keys will be kept ([README](../README.md#key-protection)) | note below a TPM; `create` asks |
| Hardware | 8 CPUs, 12 GiB memory, 120 GiB free disk, beyond the reserves above | a coder agent at its default size | note; `create` checks each agent's own sizes (a machine that cannot be measured fails) |
| Software | Linux with systemd (older than 259, the tested version, is noted) | gateway units | warn |
| Software | Docker | troupe reads the kit's files from the pulled image | warn |
| Software | a token that reads the kits, in both sbx (`sbx secret set --registry`) and Docker (`docker login`) | sbx pulls the VM image only with its own credential; troupe reads the kit through Docker | warn; `create` asks for the token once (hidden) and stores it in both |
| Software | sbx 0.48 or newer | the sandboxes | warn |
| Software | sudo | one-time key enrollment | warn |

`troupe doctor` runs these checks and prints one line per requirement: ok, warning or failure, with the fix for each problem. It exits non-zero on any hardware failure. It is the single implementation, used by:
- **the installer**, which runs it before finishing and refuses to install on any hardware failure (the binary is removed again); software problems are listed as warnings and the install completes, so the user can fix them afterwards;
- **`troupe create`**, which runs it first and refuses on any failure, including software problems that would break the creation, before touching anything.

## Install

One command for end users:

```
curl -fsSL https://raw.githubusercontent.com/starknet-innovation/troupe/main/install.sh | sh
```

`install.sh` does five things:
1. downloads the release binary into `~/.local/bin/troupe`, checked against the release's `SHA256SUMS` (or, with `TROUPE_BINARY=PATH`, installs one built locally);
2. runs `troupe doctor`;
3. on a hardware failure, removes the binary (restoring any troupe that was there) and exits non-zero, naming what's missing;
4. installs troupe's two systemd units when they're missing or changed, asking for sudo only then;
5. prints the software warnings and the next step, `troupe create coder --name NAME`.

No tagged release exists yet; until one does, the installer runs with a local binary. The recipe is in [development.md](development.md#releasing).

## EC2

EC2 works with the same code and the same TPM path, given the right instance and image:

- **Instance:** an Intel 8th-generation type with nested virtualization, launched with `--cpu-options "NestedVirtualization=enabled"`. AWS lists C8i, M8i and R8i (some sources also list 7th-generation Intel types). AMD and Graviton are not supported; `.metal` instances also work, at a higher price.
- **Image:** Ubuntu 26.04 (the release troupe is built and tested on), registered once as your own AMI from Ubuntu's snapshot with `--boot-mode uefi --tpm-support v2.0`, so the instance gets AWS's NitroTPM (a virtual TPM 2.0). TPM support is an AMI property and can't be set at launch. If a TPM-enabled Ubuntu AMI turns out to exist already, this step disappears.
- **Then the normal install**, and `troupe doctor` confirms KVM and that keys are sealed by the TPM. Without NitroTPM, keys fall back to the host key only, which a snapshot of the volume can read; `create` asks before using that.

The commands below are unverified until step 6 of the work runs them; the Ubuntu image name pattern and the CPU option syntax in particular are taken from documentation, not yet from a run.

```bash
# 1. Once per region: the latest Ubuntu 26.04 x86_64 image and its root snapshot (Canonical's account).
aws ec2 describe-images --owners 099720109477 \
  --filters 'Name=name,Values=ubuntu/images/*ubuntu-*-26.04-amd64-server-*' \
  --query 'sort_by(Images,&CreationDate)[-1].[ImageId,BlockDeviceMappings[0].Ebs.SnapshotId]' --output text

# 2. Register your own AMI from that snapshot, with UEFI and NitroTPM.
aws ec2 register-image --name ubuntu-26.04-tpm --architecture x86_64 --virtualization-type hvm --ena-support \
  --root-device-name /dev/sda1 \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={SnapshotId=SNAPSHOT,VolumeType=gp3}' \
  --boot-mode uefi --tpm-support v2.0

# 3. Launch an Intel 8th-generation instance with nested virtualization and room for agents.
aws ec2 run-instances --image-id AMI --instance-type m8i.2xlarge --key-name KEY \
  --cpu-options 'NestedVirtualization=enabled' \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=250,VolumeType=gp3}'

# 4. On the instance: Docker, sbx, then troupe.
curl -fsSL https://raw.githubusercontent.com/starknet-innovation/troupe/main/install.sh | sh
```


KMS instead of the TPM was considered and rejected for now:
- every key use would become an AWS call;
- a guest reaching the instance metadata endpoint could decrypt every agent's keys, which is unmeasured;
- it would need a second secret system.

## Where things live

- **Kits:** `ghcr.io/starknet-innovation/agents/TYPE:VERSION` (once published there, see below; GitHub lists them as `agents/TYPE`), plus `TYPE:latest` (the `registry` machine setting). Access is one GitHub token that can read packages: sbx pulls the VM image only with its own registry credential, and troupe reads the kit through Docker, so `create` asks for the token once (hidden) when either is missing and stores it in both.
- **Agents:** troupe generates each agent's environment under `~/.local/share/troupe/agents/NAME/`:
  - `sbxenv.yaml`, naming the registry kit and the agent's own mixin;
  - the mixin (`NAME.yaml`, `NAME.dockerfile`, `files/home/.hermes/` with the SOUL and `.env`);
  - `storage.env`;
  - `source.json`, the kit version it is pinned to: the one it was created from, or the one `upgrade` moved it to. `reset` rebuilds from exactly that.

  Users never edit these.
- **Keys:** under `~/.local/state/credentials/NAME/`, at the strongest [tier](../README.md#key-protection) the machine offers.
- **Maintainer mode:** the dev build, with `troupe config set kits DIR` or `--kits`, uses a local kits root. Agents found there are local agents.

## What the published kit must carry

troupe needs some type-level files on the host that today come from the kit folder. They move into the kit image at a fixed path, `/usr/share/troupe/`:
- `services.json`: every service an agent of the type needs, in prompt order, each with its prompt, a short help (what the key is and where to get it), whether it's required, and how sbx delivers it (native, with its domains, or a placeholder, with its environment variable, hosts and placeholder; today's `secrets.json` plus the agents' bindings);
- `defaults.json`: the default CPUs, memory, root disk and Docker disk;
- `settings.json`, optional: the settings `create` asks for before the keys, in prompt order, each with the `.env` variable it sets, its prompt, a short help, and the regular expression a non-empty answer must match whole (`{"settings":[{"env":"TEAM_NAME","prompt":"Team name","help":"…","pattern":"[a-z]+"}]}`). None may be a `SLACK_` variable, a line of the kit's `env` template or a placeholder service's variable;
- `SOUL.md`: the type's default SOUL, with `{agent}` for the name (`hermes` has none: Hermes writes its own);
- `slack-app-manifest.json`, only in kits whose type offers Slack;
- `tools/`: the type's setup and seed-check tools;
- `seeds/`: a copy of the type's seeds (from 1.2.0). The image's `~/.hermes` mixes them with Hermes's own skills and logs, so they can't be read from there.

None of them is private or per user. troupe reads them with `docker create` plus `docker cp` from the pulled kit, as a capped stream of plain files, and keeps them in `~/.local/share/troupe/kits/TYPE-VERSION/`, laid out like a local kit folder.

One file doesn't move: `slack.env`, the Slack workspace and boundary channels. It's the user's own setting, not the type's. A published agent's workspace is its own: learned from its bot token at its first Slack setup and pinned in the agent's folder (`slack.env`). The boundary channels are optional machine settings (`slack-boundary-public`, `slack-boundary-private`), read at every create and reset, so one set later applies at the next reset (a resize does not probe Slack again); unset, no boundary is checked.

## The `latest` tag

Neither Docker nor sbx lists a registry's tags, so without a moving tag troupe would need its own registry client to find the newest version. `kit push` (in hermes-agents) points `TYPE:latest` at each version it publishes, after verifying it, unless `latest` already holds a newer version, and proves both tags serve the same digest. Version tags never move.

`create` resolves `latest` once:
1. pulls `TYPE:latest`;
2. reads which version that is from the image's `org.opencontainers.image.version` label;
3. records the version in `source.json`, and pins the agent to that version's tag (`REGISTRY/TYPE:VERSION`, which never moves), so `reset` never follows a later `latest`.

## Work, in order (each step reviewed before the next)

1. **hermes-agents:**
   - kits gain `/usr/share/troupe/` with the files above;
   - `kit push` moves `latest`;
   - publish `coder` and `hermes` 1.1.0 (MINOR: new content, nothing breaks).
2. **troupe:**
   - the `registry` setting and registry agents in discovery;
   - `create TYPE --name` with defaults and prompts, and `source.json` (2a, proven with `hermes`);
   - prebuild becomes `docker pull` for registry kits;
   - seeds, tools and services are read from the pulled image;
   - the coder type made end-user ready (2d): GitHub and Slack required, the agent's GitHub identity found in its VM, an optional profile picture (`create --avatar PATH|URL`), each agent pinned to its bot token's Slack workspace, and the boundary channels as optional machine settings;
   - `reset` as the end-user name for recreate, with its confirmation;
   - the resource budget in `create`, `resize` and `start` (warn and confirm, `--force`, refuse only the impossible), after measuring the per-VM overhead on this host;
   - `resize` on top of `migrate`, with the new sizes recorded;
   - `delete`, with its typed confirmation, the full local cleanup and the list of provider-side steps;
   - `chat`, and the self-test that ends `create`.
3. **Development commands only in the dev build** (done 2026-10-14, [above](#the-release-is-end-user-only-development-commands-live-in-the-dev-build)):
   - `update`, `agents`, `storage check` and `harvest` (troupe-dev folded in) compile only with `-tags dev`, with the kits root;
   - `recreate` and `migrate` are gone: `reset` and `resize` do their jobs, for local agents too;
   - the hermes-agents scripts, docs and skills point at the new names and require a dev build.
4. **troupe doctor and the installer** (done 2026-10-14, publishing deferred):
   - `troupe doctor` with the checks above (the registry token is left to `create`, which asks for it), run first by `create`;
   - `install.sh`, which also installs the units;
   - the release recipe, documented; the tagged release waits for publishing;
   - tests that make each hardware check fail (no KVM, wrong architecture, too little memory or disk) and each software check warn, using fake system files.
5. **Proof on this host,** with a new throwaway agent (done 2026-10-14 with the release build, installed by `install.sh` from a local binary since publishing is deferred; it passed, see the end of this list):
   1. Install with the one-line command.
   2. `troupe create coder --name jane-test --avatar URL` and the prompts, creating the Slack app through the one-click link: it must open pre-filled. The self-test must pass, the agent must answer on Slack and describe its picture, and `troupe chat jane-test` must hold a conversation while the gateway serves Slack.
   3. `troupe resize jane-test --memory 12`. The agent must come back with the new memory and the same memories and skills, proven by a marker file it wrote before.
   4. `troupe delete jane-test`. Nothing of it may remain: no sandbox, keys, records or unit. Then `create` must accept the name again.
   Step 5's result: the release build installed with no sudo prompt and the doctor passed; `create` pinned the agent to the `coder:1.3.2` tag, recorded its Slack workspace, and the agent found its GitHub identity in its VM; it answered in Slack and in `troupe chat` and described its picture; `resize --memory 12` brought it back at 12 GiB with its marker file unchanged byte for byte and its state database kept; `delete` left nothing of it, and the machine matched its state from before the test once the replaced VM's image (the finding above) and the kit cache were removed.
6. **Proof on EC2:**
   - register the AMI;
   - launch a nested-virtualization instance;
   - run the one-line install and `troupe doctor` (all hardware checks ok);
   - create a throwaway agent, then terminate the instance.

   A negative control: the same instance type launched without nested virtualization must make the installer refuse.

## Open points for your decision

- The `coder` default sizes are 8 CPUs, 12 GiB memory (your call, for language servers, the browser and builds; raised from 6 and 8 on 2026-10-25), 100 GiB root disk and 20 GiB Docker disk. `hermes` uses 6 GiB memory and 50 GiB and 10 GiB disks, like Hermes Lab.
- Decided (2026-10-14, to revisit): Slack and GitHub are required for `coder`; `hermes` is the type without them. `troupe chat` and the end-of-create self-test work for every agent.
- Measured in step 5 (2026-10-14): a terminal chat runs while the gateway serves Slack in the same VM, and the Slack link, printed whole, creates the app pre-filled. A clickable label alone (OSC 8) showed as plain text with no address in the owner's terminal, so the link is now always printed whole.
- Step 5 also found that `reset` and `resize` left the replaced VM's assembled image behind (about 3 GB each). Fixed the same day: both remove it once the new VM runs, under `delete`'s rule (never an image any sandbox runs), and a run interrupted before that has it removed by the agent's next reset or resize.
- The kits move to `ghcr.io/starknet-innovation/agents`, the default registry, before the first release; until then a machine sets `registry` to the staging location.
