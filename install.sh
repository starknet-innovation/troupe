#!/bin/sh
# troupe's installer: puts troupe in ~/.local/bin, checks this machine with troupe doctor, and
# installs troupe's systemd units, which run the sandbox daemon (once sbx is installed). Refuses a
# machine that cannot host agents; on any failure or interruption the troupe binary that was there
# before is put back (units already written stay, and a later run finishes them).
#
#   sh install.sh                          the latest release (once troupe is published)
#   TROUPE_BINARY=PATH sh install.sh       a troupe binary built here
#
# TROUPE_RELEASE overrides where releases are downloaded from (troupe-linux-amd64 and SHA256SUMS).
# The whole script is one function, called on its last line: a download cut short runs nothing.
set -eu

main() {
  release=${TROUPE_RELEASE:-https://github.com/starknet-innovation/troupe/releases/latest/download}
  bin_dir="$HOME/.local/bin"
  target="$bin_dir/troupe"
  unit_dir=${TROUPE_UNIT_DIR:-/etc/systemd/system}
  sandboxd=${TROUPE_SANDBOXD_DIR:-$HOME/.local/state/sandboxes/sandboxes/sandboxd}
  sbx=${TROUPE_SBX:-/usr/bin/sbx} # what sbx-daemon.service runs

  # 1. A machine and a user troupe can run as, before anything is downloaded.
  [ "$(id -u)" -ne 0 ] || die "run the installer as the user who will own the agents, not root; it asks for sudo when it needs it"
  [ "$(uname -s)" = Linux ] || die "troupe runs on Linux; this is $(uname -s)"
  case "$(uname -m)" in
    x86_64 | amd64) ;;
    *) die "troupe's agents need an x86_64 CPU; this is $(uname -m)" ;;
  esac

  work=$(mktemp -d)
  replaced="" previous="" finished="" was=""
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM HUP

  # 2. The binary: one built here, or the release's, checked against the release's checksum
  #    (which catches a corrupted download; it comes from the same place as the binary).
  if [ -n "${TROUPE_BINARY:-}" ]; then
    [ -f "$TROUPE_BINARY" ] || die "TROUPE_BINARY is not a file: $TROUPE_BINARY"
    cp "$TROUPE_BINARY" "$work/troupe"
  else
    command -v curl >/dev/null 2>&1 || die "curl is needed to download troupe"
    curl -fsSL "$release/troupe-linux-amd64" -o "$work/troupe" || die "could not download $release/troupe-linux-amd64"
    curl -fsSL "$release/SHA256SUMS" -o "$work/SHA256SUMS" || die "could not download $release/SHA256SUMS"
    want=$(awk '$2 == "troupe-linux-amd64" || $2 == "*troupe-linux-amd64" { print $1 }' "$work/SHA256SUMS")
    got=$(sha256sum "$work/troupe" | awk '{ print $1 }')
    [ -n "$want" ] && [ "$want" = "$got" ] || die "the download does not match the release's checksum; nothing was installed"
  fi
  chmod 755 "$work/troupe"

  mkdir -p "$bin_dir"
  if [ -e "$target" ]; then
    was=$("$target" version 2>/dev/null) || was="" # an update says what it replaced
    previous="$work/previous"
    cp -p "$target" "$previous"
  fi
  # Recorded before the move: an interruption right after it still puts the old troupe back.
  replaced=1
  cp "$work/troupe" "$bin_dir/.troupe.new"
  mv "$bin_dir/.troupe.new" "$target"
  say "installed $("$target" version) in $target"

  # 3. This machine: hardware troupe cannot do without fails, and the install is undone.
  if ! "$target" doctor > "$work/doctor" 2>&1; then
    cat "$work/doctor" >&2
    die "this machine cannot host agents (FAILED above)"
  fi

  # 4. troupe's systemd units, written when missing or changed, and the sandbox daemon run by
  #    sbx-daemon.service, enabled at boot. sudo is asked for only when something must change.
  #    Without sbx the unit could only fail, again and again: the units wait for it.
  if [ ! -x "$sbx" ]; then
    finished=1
    grep 'ACTION' "$work/doctor" || true
    say "installed troupe without its systemd units: install docker-sbx, then run the installer again"
    return 0
  fi
  # Signed out, sbx refuses every sandbox command and sandboxd answers the unit's probe with a
  # 401: the daemon could only fail to start. The units wait for sbx login, as they wait for sbx.
  if grep -q ' sign-in: ACTION' "$work/doctor"; then
    finished=1
    grep 'ACTION' "$work/doctor" || true
    say "installed troupe without its systemd units: sign sbx in to Docker (sbx login), then run the installer again"
    return 0
  fi
  changed=""
  for unit in sbx-daemon.service hermes-gateway@.service; do
    "$target" units render "$unit" > "$work/$unit"
    cmp -s "$work/$unit" "$unit_dir/$unit" || changed="$changed $unit"
  done
  activate=""
  if [ -n "$changed" ] || [ "$(systemctl show sbx-daemon.service -p UnitFileState --value 2>/dev/null)" != enabled ] ||
    ! systemctl is-active --quiet sbx-daemon.service; then
    activate=1
  fi
  if [ -n "$activate" ]; then
    # A sandbox daemon some sbx command started on its own would meet the unit's: two daemons
    # on one state folder. It is stopped first, by the user.
    if [ -f "$sandboxd/sandboxd.pid" ]; then
      pid=$(cat "$sandboxd/sandboxd.pid")
      if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null &&
        [ "$pid" != "$(systemctl show sbx-daemon.service -p MainPID --value 2>/dev/null)" ]; then
        die "a sandbox daemon (pid $pid) runs outside sbx-daemon.service; stop it (sbx daemon stop), then run the installer again"
      fi
    fi
    say "installing troupe's systemd units and starting sbx-daemon.service (sudo)"
    # A passwordless sudo is asked nothing: sudo-rs (Ubuntu 26.04's) prompts on -v even under
    # NOPASSWD, for a password a cloud user (EC2's ubuntu) does not have.
    sudo -n true 2>/dev/null || sudo -v || die "sudo is needed to install troupe's systemd units"
    for unit in $changed; do
      sudo install -m 644 "$work/$unit" "$unit_dir/$unit"
    done
    sudo systemctl daemon-reload
    if ! sudo systemctl enable --now sbx-daemon.service; then
      grep 'ACTION' "$work/doctor" >&2 || true
      die "sbx-daemon.service did not start (systemctl status sbx-daemon.service says why)"
    fi
    "$target" doctor > "$work/doctor" 2>&1 || true
  fi
  # sbx creates no sandbox until the machine's global network policy is set: a new machine gets
  # balanced (typical development traffic). init refuses on a machine that has one, so a policy
  # is never replaced. sbx runs only after the probe, so it meets the unit's daemon.
  "$target" guard probe --wait 75s > "$work/probe" 2>&1 || {
    cat "$work/probe" >&2
    die "the sandbox daemon does not answer troupe's probe"
  }
  if "$sbx" policy init balanced < /dev/null > "$work/policy" 2>&1; then
    say "set this machine's sandbox network policy to balanced"
  elif ! grep -q 'already initialized' "$work/policy"; then
    cat "$work/policy" >&2
    die "could not set the sandbox network policy (sbx policy init balanced)"
  fi
  # sbx policy reset replaces the unit's daemon with its own (sbx 0.50.1): init must not have.
  "$target" guard probe > "$work/probe" 2>&1 || {
    cat "$work/probe" >&2
    die "the sandbox daemon no longer answers troupe's probe after sbx policy init"
  }
  finished=1

  # 5. What is left for the user, and the next step.
  grep 'ACTION' "$work/doctor" || true
  case ":$PATH:" in
    *":$bin_dir:"*) ;;
    *) say "add $bin_dir to your PATH (for example in ~/.profile) to run troupe by name" ;;
  esac
  if [ -z "$previous" ]; then
    say "done. Next: troupe create coder --name NAME"
  else
    say "done. $("$target" version) replaced ${was:-the previous troupe}"
  fi
}

say() { printf 'troupe install: %s\n' "$*"; }
die() {
  printf 'troupe install: %s\n' "$*" >&2
  exit 1
}

# cleanup runs on every exit: an install that did not finish puts back the troupe that was there
# before (or removes the new one).
cleanup() {
  if [ -n "${replaced:-}" ] && [ -z "${finished:-}" ]; then
    if [ -n "${previous:-}" ]; then
      mv "$previous" "$target"
      say "put back the troupe that was here before" >&2
    else
      rm -f "$target"
      say "troupe was not installed" >&2
    fi
  fi
  rm -rf "${work:-}"
}

main "$@"
