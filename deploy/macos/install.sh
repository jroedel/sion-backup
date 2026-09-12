#!/usr/bin/env bash
#
# install.sh — put sion-backup on a Mac.
#
# Run it as the person whose files are being backed up, from the directory
# the release was unpacked into:
#
#     ./install.sh                 # install, stop before enrolling
#     ./install.sh --recon-only    # look and report, change nothing
#     ./install.sh --no-service    # install the binary, register nothing
#
# WHAT IT DOES NOT DO
#
# It does not enrol the machine: that needs a code from Eumaeus that is good
# for fifteen minutes, and it belongs to whoever is standing here.
#
# It cannot grant Full Disk Access. Nothing can — it is a checkbox a human
# being clicks, by design, and it is the difference between a backup of
# somebody's work and a backup of an empty-looking home directory. The script
# prints the steps and refuses to pretend the install is finished without it.
#
# WHY THE BINARY GOES IN $HOME AND NOT /usr/local/bin
#
# Because this is a launchd USER AGENT, and Full Disk Access is granted to a
# path by a person for their own account. Keeping the binary, the agent and
# the grant all inside one account means the install has no part that belongs
# to root, and therefore no part that sudo can put in the wrong place.

set -euo pipefail

PREFIX="${HOME}/.local/bin"
BINARY=""
RECON_ONLY=0
NO_SERVICE=0

LABEL="us.schoenstatt.sion-backup"
PLIST_SRC="./deploy/launchd/${LABEL}.plist"
AGENT_DIR="${HOME}/Library/LaunchAgents"

INSTALL_ID="$(uuidgen 2>/dev/null || date +%s-$$)"
STEP="starting"

while [ $# -gt 0 ]; do
  case "$1" in
    --binary)     BINARY="$2"; shift 2 ;;
    --prefix)     PREFIX="$2"; shift 2 ;;
    --plist)      PLIST_SRC="$2"; shift 2 ;;
    --recon-only) RECON_ONLY=1; shift ;;
    --no-service) NO_SERVICE=1; shift ;;
    -h|--help)    sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)            echo "install.sh: unknown option $1" >&2; exit 2 ;;
  esac
done

INSTALLED="${PREFIX}/sion-backup"

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
warn() { printf '  \033[33m%s\033[0m\n' "$*" >&2; }

# report_failure tells Eumaeus that this install did not finish. Best effort
# by construction: it needs the binary to be in place, which the earliest
# failures precede.
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
      --detail "macos/install.sh exited ${status} at ${STEP}" >/dev/null 2>&1 || true

    note "reported it (or queued the report) — see: $INSTALLED doctor"
  fi

  return 0
}

# ---------------------------------------------------------------------------
# 0. Whose machine is this?
# ---------------------------------------------------------------------------
#
# Same check the Linux installer makes and the binary makes before enrolling,
# and it matters more here: a LaunchAgent installed into root's home never
# runs for the person sitting at the machine, and the Full Disk Access grant
# would be attached to the wrong account.

if [ -n "${SUDO_USER:-}" ]; then
  warn "this installs into \$HOME, and sudo makes that root's."
  printf '\n'
  note "Everything here belongs to the person whose files are backed up: the"
  note "binary, the data directory, the LaunchAgent, and the Full Disk Access"
  note "grant. Run it as ${SUDO_USER}, without sudo:"
  printf '\n'
  note "    ./install.sh"
  printf '\n'
  exit 2
fi

if [ "$(uname -s)" != "Darwin" ]; then
  echo "install.sh: this is the macOS installer; see deploy/linux/install.sh" >&2
  exit 2
fi

trap report_failure EXIT

# ---------------------------------------------------------------------------
# 1. The binary
# ---------------------------------------------------------------------------

STEP="install-binary"

# Picked from the machine rather than left to whoever unpacked the release.
# An Apple Silicon Mac runs the amd64 build under Rosetta, slowly and only if
# Rosetta happens to be installed, and the failure when it is not is a line
# about a bad CPU type that tells nobody what to do about it.
if [ -z "$BINARY" ]; then
  case "$(uname -m)" in
    arm64)  BINARY="./sion-backup-darwin-arm64" ;;
    x86_64) BINARY="./sion-backup-darwin-amd64" ;;
    *)      echo "install.sh: unknown architecture $(uname -m)" >&2; exit 1 ;;
  esac
fi

if [ ! -f "$BINARY" ]; then
  echo "install.sh: cannot find $BINARY — pass --binary /path/to/it" >&2
  exit 1
fi

mkdir -p "$PREFIX"
install -m 0755 "$BINARY" "$INSTALLED"

# The release binaries are not signed with an Apple Developer ID and are not
# notarized, so anything downloaded through a browser carries the quarantine
# flag and Gatekeeper refuses to run it — with a dialog that offers "Move to
# Trash" as one of its two buttons. Clearing it here is the same decision the
# person made by choosing to run this installer, taken once instead of per
# binary, and it is why this script exists rather than a paragraph in a README.
if xattr -p com.apple.quarantine "$INSTALLED" >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$INSTALLED"
  note "cleared the download quarantine flag"
fi

say "Installed $("$INSTALLED" version)"
note "$INSTALLED"

if ! printf '%s' "$PATH" | tr ':' '\n' | grep -qx "$PREFIX"; then
  note "note: $PREFIX is not on your PATH; the commands below use the full path"
fi

STEP="recon"

say "Looking at this machine"
"$INSTALLED" recon

if [ "$RECON_ONLY" -eq 1 ]; then
  say "Nothing else was changed (--recon-only)."
  exit 0
fi

# ---------------------------------------------------------------------------
# 2. restic
# ---------------------------------------------------------------------------

STEP="fetch-restic"

# Done now rather than at two in the morning, and as the person whose files
# are backed up, so it lands in their own data directory.
say "restic"
"$INSTALLED" restic

# ---------------------------------------------------------------------------
# 3. The LaunchAgent
# ---------------------------------------------------------------------------

if [ "$NO_SERVICE" -eq 0 ]; then
  STEP="install-service"

  if [ ! -f "$PLIST_SRC" ]; then
    warn "$PLIST_SRC is not here; skipping the service"
  else
    say "Installing the launch agent"

    mkdir -p "$AGENT_DIR"

    # Generated rather than copied, because the shipped plist names
    # /usr/local/bin and this installs into $HOME. A copied file would point
    # at a binary that is not there, and launchd reports that as a service
    # which loads and immediately dies — which looks exactly like a crash.
    sed "s|<string>/usr/local/bin/sion-backup</string>|<string>${INSTALLED}</string>|" \
      "$PLIST_SRC" > "${AGENT_DIR}/${LABEL}.plist"

    if ! grep -q "<string>${INSTALLED}</string>" "${AGENT_DIR}/${LABEL}.plist"; then
      echo "install.sh: could not point the agent at ${INSTALLED}" >&2
      exit 1
    fi

    # Unloaded first so that re-running the installer replaces the agent
    # rather than failing with "service already bootstrapped", which is the
    # normal case on an upgrade.
    launchctl bootout "gui/$(id -u)/${LABEL}" >/dev/null 2>&1 || true
    launchctl bootstrap "gui/$(id -u)" "${AGENT_DIR}/${LABEL}.plist"

    note "registered as ${LABEL}"
    note "it starts at login, and restarts if it stops"
  fi
fi

# ---------------------------------------------------------------------------
# 4. Full Disk Access — the step no script can take
# ---------------------------------------------------------------------------

STEP="done"

say "Full Disk Access — do this now, it is not optional"
note "On a Mac, ~/Documents, ~/Desktop and Mail are protected by TCC rather"
note "than by file permissions. Without this grant the backup runs, succeeds,"
note "and quietly contains none of them: the folders look empty to it."
printf '\n'
note "  1.  → System Settings → Privacy & Security → Full Disk Access"
note "  2. Click +, then press ⌘⇧G"
note "  3. Paste this path and choose the file:"
note "       ${INSTALLED}"
note "  4. Make sure its switch is on"
printf '\n'
note "Then restart the agent so it picks the grant up:"
note "    launchctl kickstart -k gui/$(id -u)/${LABEL}"

say "Next"
note "1. In Eumaeus: choose the owner and the bucket, and issue a code."
note "2. Enrol, print the restore card, and give it to the owner:"
note "     $INSTALLED enroll --code XXXX-XXXX"
note "3. Enrolling starts the service and opens the set-up page. NOTHING is"
note "   backed up until somebody at this computer answers it — that is where"
note "   the folders, the schedule and the first backup are chosen:"
note "     http://127.0.0.1:7391/setup"

echo
