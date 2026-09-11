#!/usr/bin/env bash
#
# install.sh — put sion-backup on a Linux machine, including one that is
# already backing up with the old scripts.
#
# Run it as the person whose files are being backed up, from the directory
# the release was unpacked into:
#
#     ./install.sh                 # look, report, install, stop before enrolling
#     ./install.sh --yes           # do not ask
#     ./install.sh --recon-only    # look and report, change nothing
#     ./install.sh --legacy-dir /srv/backup   # the old install is somewhere odd
#
# WHAT IT DOES NOT DO
#
# It does not enrol the machine: that needs a code from Eumaeus that is good
# for fifteen minutes, and it belongs to whoever is standing here.
#
# It does not disable the legacy backup. That happens last, after the new
# install has taken one verified backup, and --disable-legacy is how you say
# so once it has. Two backup systems for one night is untidy; none is worse.
#
# It does not touch the legacy bucket, credentials, or history.

set -euo pipefail

BINARY="./sion-backup-linux-amd64"
PREFIX="${HOME}/.local/bin"
ASSUME_YES=0
RECON_ONLY=0
DISABLE_LEGACY=0
NO_SERVICE=0
LEGACY_DIR=""

# INSTALL_ID ties every report from this run of the installer together. It is
# generated here rather than by the binary because the failures worth hearing
# about include the ones from before the binary was in place.
INSTALL_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || date +%s-$$)"

# STEP is what a failure gets reported as. Set it before anything that can
# fail; the trap sends it.
STEP="starting"

while [ $# -gt 0 ]; do
  case "$1" in
    --binary)         BINARY="$2"; shift 2 ;;
    --prefix)         PREFIX="$2"; shift 2 ;;
    --yes|-y)         ASSUME_YES=1; shift ;;
    --recon-only)     RECON_ONLY=1; shift ;;
    --disable-legacy) DISABLE_LEGACY=1; shift ;;
    --legacy-dir)     LEGACY_DIR="$2"; shift 2 ;;
    --no-service)     NO_SERVICE=1; shift ;;
    -h|--help)        sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)                echo "install.sh: unknown option $1" >&2; exit 2 ;;
  esac
done

INSTALLED="${PREFIX}/sion-backup"

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
warn() { printf '  \033[33m%s\033[0m\n' "$*" >&2; }

# report tells Eumaeus that this install did not finish.
#
# Best-effort by construction: it needs the binary to have been installed,
# which the earliest failures precede. Nothing here may fail the trap it runs
# from, hence the || true — an installer that dies while reporting that it
# died helps nobody.
report_failure() {
  local status=$?

  if [ "$status" -eq 0 ]; then
    return 0
  fi

  warn "install failed at step: ${STEP}"

  if [ -x "$INSTALLED" ]; then
    "$INSTALLED" report \
      --kind install-failed \
      --step "$STEP" \
      --install-id "$INSTALL_ID" \
      --prior-version "${PRIOR_VERSION:-}" \
      --detail "install.sh exited ${status} at ${STEP}" >/dev/null 2>&1 || true

    note "reported it (or queued the report) — see: $INSTALLED doctor"
  fi

  return 0
}

trap report_failure EXIT

# ---------------------------------------------------------------------------
# 1. What is already here
# ---------------------------------------------------------------------------

STEP="install-binary"

if [ ! -f "$BINARY" ]; then
  echo "install.sh: cannot find $BINARY — pass --binary /path/to/it" >&2
  exit 1
fi

mkdir -p "$PREFIX"
install -m 0755 "$BINARY" "$INSTALLED"

say "Installed $("$INSTALLED" version)"
note "$INSTALLED"

STEP="recon"

RECON_ARGS=""
if [ -n "$LEGACY_DIR" ]; then
  RECON_ARGS="--legacy-dir $LEGACY_DIR"
fi

say "Looking at this machine"
# shellcheck disable=SC2086  # RECON_ARGS is a flag pair or empty, by construction
"$INSTALLED" recon $RECON_ARGS

# Once, and reused. recon reaches the network and runs restic, and doing that
# three times to answer three questions about one machine is the kind of
# thing somebody notices on hotel wifi.
# shellcheck disable=SC2086
RECON_JSON="$("$INSTALLED" recon $RECON_ARGS --json 2>/dev/null || true)"

# PRIOR_VERSION goes on any failure report, so a migration that breaks on one
# vintage of the old scripts and not another is visible on the server.
PRIOR_VERSION="$(
  printf '%s' "$RECON_JSON" |
    sed -n 's/.*"layout": "\([a-z]*\)".*/legacy-\1/p' | head -1
)"

LEGACY_FOUND=0
if printf '%s' "$RECON_JSON" | grep -q '"legacy"'; then
  LEGACY_FOUND=1
fi

if [ "$RECON_ONLY" -eq 1 ]; then
  say "Nothing was changed (--recon-only)."
  exit 0
fi

if [ "$LEGACY_FOUND" -eq 1 ] && [ "$ASSUME_YES" -eq 0 ]; then
  say "This machine is already backing up with the old scripts."
  note "Read the plan above. Nothing below touches the legacy install, its"
  note "bucket or its credentials — but decide the bucket question BEFORE"
  note "enrolling, because enrolling is what fixes the answer."
  printf '\n  Continue? [y/N] '
  read -r answer

  case "$answer" in
    y|Y|yes|YES) ;;
    *) say "Stopped. The binary is installed and nothing else was changed."; exit 0 ;;
  esac
fi

# ---------------------------------------------------------------------------
# 2. restic
# ---------------------------------------------------------------------------

STEP="fetch-restic"

# The binary does this, on every platform, from a pin compiled into it. What
# used to be here was a shell reimplementation of the same download, hash
# check and extraction that install.ps1 also had a copy of -- three
# implementations of one careful thing, and the one that ran depended on
# whether ./scripts happened to be unpacked beside the release.
#
# Running it now rather than leaving it to enrolling is worth one step: this
# runs as the person whose files are backed up, so the binary lands in their
# own data directory, and a 20 MB download is better done while somebody is
# standing here than at two in the morning.
say "restic"
"$INSTALLED" restic

# ---------------------------------------------------------------------------
# 3. The service
# ---------------------------------------------------------------------------

if [ "$NO_SERVICE" -eq 0 ]; then
  STEP="install-service"

  UNIT_SRC="./deploy/systemd/sion-backup.service"
  UNIT_DIR="${HOME}/.config/systemd/user"

  if [ -f "$UNIT_SRC" ]; then
    say "Installing the user service"
    mkdir -p "$UNIT_DIR"
    install -m 0644 "$UNIT_SRC" "$UNIT_DIR/sion-backup.service"

    systemctl --user daemon-reload
    systemctl --user enable sion-backup >/dev/null

    # Without lingering, the daemon stops when the last session ends and a
    # machine that is switched on but not signed into takes no backups.
    if ! loginctl show-user "$USER" 2>/dev/null | grep -q '^Linger=yes'; then
      note "enabling lingering so it runs when nobody is signed in"
      loginctl enable-linger "$USER" || warn "could not enable lingering; ask an administrator"
    fi

    note "enabled (not started: it has nothing to do until the machine is enrolled)"
  else
    warn "deploy/systemd/sion-backup.service is not here; skipping the service"
  fi
fi

# ---------------------------------------------------------------------------
# 4. The legacy schedule, if and only if asked
# ---------------------------------------------------------------------------

if [ "$DISABLE_LEGACY" -eq 1 ]; then
  STEP="disable-legacy"

  say "Disabling the legacy schedule"

  if [ "$(id -u)" -ne 0 ]; then
    warn "this needs root: re-run just this step with sudo, or comment the line out by hand"
  else
    for account in restic backup; do
      for table in "/var/spool/cron/crontabs/${account}" "/var/spool/cron/${account}"; do
        [ -f "$table" ] || continue

        cp -a "$table" "${table}.before-sion-backup.$(date +%Y%m%d%H%M%S)"
        sed -i 's|^\([^#].*backup\.sh.*\)$|# disabled by sion-backup install.sh: \1|' "$table"

        note "commented out the backup line in $table (a copy is beside it)"
      done
    done
  fi
fi

# ---------------------------------------------------------------------------
# 5. What is left for a person
# ---------------------------------------------------------------------------

STEP="done"

say "Next"
note "1. In Eumaeus: choose the owner and the bucket, and issue a code."
if [ "$LEGACY_FOUND" -eq 1 ]; then
  note "   If this machine's existing bucket is being adopted, that has to be"
  note "   done on the server FIRST: eumaeus backup adopt -node <id> -bucket <b>"
fi
note "2. Enrol, print the restore card, and give it to the owner:"
note "     $INSTALLED enroll --code XXXX-XXXX"
note "3. Take one backup in the foreground and watch it:"
note "     $INSTALLED run"
note "4. Check it, then start the service:"
note "     $INSTALLED doctor"
note "     systemctl --user start sion-backup"
note "     http://127.0.0.1:7391/"

if [ "$LEGACY_FOUND" -eq 1 ] && [ "$DISABLE_LEGACY" -eq 0 ]; then
  note "5. ONLY after a verified backup, turn the old one off:"
  note "     sudo ./install.sh --disable-legacy"
fi

echo
