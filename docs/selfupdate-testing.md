# Testing self-update before it reaches the fleet

Self-update is the only feature in this program that can break every machine
at once, and the only one whose failure mode removes the mechanism that would
fix it. A machine that stops backing up is visible on the dashboard and can be
updated. A machine whose service will not start is not running the code that
reports to the dashboard, and is not running the code that could replace
itself either. Somebody has to go and stand in front of it.

Everything below is arranged around that one asymmetry.

## What actually bricks a machine

Three ways, in order of how likely they are.

**1. A build that passes every check and does not work.** This is the one.
`foundation/selfupdate` verifies the SHA-256 the release named, then runs the
downloaded binary and asks it its version. That smoke test is weaker than it
looks: `version` is handled in `cmd/sion-backup/main.go:131`, in main's
dispatch, before `wire()` runs — so it opens no database, reads no config file
and never looks for restic. A build with a broken schema migration, a config
field that no longer parses, or a nil dereference in the daemon's wiring
answers `version` perfectly and then dies on the command the service manager
actually runs.

`scripts/selfupdate-e2e/run bricked` demonstrates it rather than arguing about
it: a release that passes the hash and the smoke test, installs, and then
crash-loops under a service manager for as long as you let it.

**2. A release that is wrong on the wire.** A truncated download, a mismatched
hash, a build for the wrong architecture, a prerelease that reached `/latest`
by accident. The code handles all of these well, and the container harness
covers eleven of them. This is the risk that gets the most attention and is
the smallest.

**3. An environment the update path has never run in.** A read-only mount, an
install directory owned by an administrator and a service running as the user,
a machine whose home directory moved. These do not brick anything — they make
self-update quietly stop happening, which is worse in a different way.

## Two guards that do not exist yet

Testing addresses risk 2 well and risk 3 completely. It does not address risk
1, because risk 1 is a build that is wrong in a way nobody predicted, and a
test suite is a list of things somebody predicted. Two structural guards would,
and neither is here.

**There is no rollback.** `swap()` moves the running binary to `.old` before
putting the new one in place, so the previous version is on the disk. Nothing
ever restores it. Worse, `CleanupOld` runs at daemon startup
(`cmd/sion-backup/daemon.go:64`), immediately after `wire()` succeeds and
before the daemon has taken a single backup — so the only copy of the working
version is deleted seconds into the new one's life, while the failure modes
that matter most (restic incompatibility, a credential fetch that now 400s)
are still hours away. A machine that gets that far and then fails has nothing
left to fall back to.

The shape of the fix is a start counter: the new binary records that it
started, and on start N without a completed backup it puts `.old` back. That
turns risk 1 from "somebody drives to the building" into "the machine is a
version behind, and the dashboard says so."

**There is no staged rollout and no kill switch.** `GitHub.Latest` reads
`/releases/latest`, so every machine takes the newest published release within
an hour of it existing. A bad tag pushed at 15:00 is on the whole fleet by the
next morning, and the only way to stop it is to un-publish the release and
hope. The package comment already names Eumaeus as the intended answer and
`Source` as the seam; `docs/eumaeus-requests.md` §5.2 is the ask. Until then,
the draft-then-gate ordering in the release workflow is the substitute: a
release that fails the gate is never published, so `/latest` never returns it.

A smaller thing, worth writing down before it bites: **the nightly check sits
close to GitHub's rate limit.** `Latest` makes two unauthenticated requests
(the release JSON, then `SHA256SUMS`), GitHub allows sixty an hour per address,
and `jitter_minutes` defaults to 30 — so thirty machines behind one office
address make sixty requests inside a half-hour window. Over the line, GitHub
answers 403, `selfUpdate` records a `KindUpdateFailed` report for each machine,
and an office produces thirty update-failure reports a night while every
backup succeeds. That is precisely the noise `fleetbus` and `diagbus` are
written to avoid. At four or five clients this is nowhere near a problem; at
one client with thirty machines it is. `scripts/selfupdate-e2e/run
rate-limited` covers the client's behaviour; the fix, when it is needed, is
either a conditional request or the Eumaeus source, which removes the question.

## The dimensions worth varying

Hand-picked, not generated. The fleet is four or five clients, so a matrix
that covers "Linux" is less useful than one that covers *their* machines.

| Dimension | Values tested | Why this one |
| --- | --- | --- |
| Release integrity | good, wrong hash, truncated, byte-corrupted, no `SHA256SUMS`, no asset for the platform, draft, prerelease, 403, connection dropped mid-body, version lie | Every branch in `download`, `smokeTest` and `GitHub.Latest` that can refuse. All must leave the machine on the binary it started with, with no debris. |
| Install location and who runs it | `~/.local/bin` owned and run by the user; a root-owned directory run by the user; a root-owned directory run by root; a read-only mount | These are the fleet's three real install shapes, plus the case the code names but nothing exercised. See below — two of them mean self-update never happens. |
| Version relationship | newer, identical, older, unparseable (`dev`) | `Newer()`'s entire contract, including that an older `/latest` must not downgrade and a hand-built binary must never replace itself. |
| Baseline | the previously published release, upgraded to the candidate | The only arrangement that can catch risk 1. A candidate tested against itself proves nothing about an upgrade. |
| Service manager | a real systemd user unit, with lingering, running when the binary is replaced | The brick is the restart, not the swap. |
| Platform | linux/amd64 today; Windows and macOS are a gap | See "What is not covered". |

### What the install-location dimension turned up

Two of those four values mean self-update silently never happens:

- **Windows.** `deploy/windows/install.ps1` installs to
  `%ProgramFiles%\sion-backup` and registers a scheduled task that runs as the
  signed-in user. If that user is not an administrator, the directory is not
  writable, `writable()` fails, and `selfUpdate` logs `ErrNotWritable` at Info
  once per process and deliberately never reports it. The machine backs up
  forever on the version it was installed with.
- **macOS.** `deploy/launchd/us.schoenstatt.sion-backup.plist` runs
  `/usr/local/bin/sion-backup` as a user agent. Same outcome, unless something
  has made `/usr/local/bin` user-writable.

Treating that as a deployment fact rather than a failure is the right call —
it *will* be true again in an hour, and hourly reports would bury the ones
that matter. But it has a consequence: the fleet dashboard cannot tell "this
machine cannot update itself" from "this machine has not checked in", because
both look like an `Agent` version that stops moving. If self-update is meant to
be the mechanism that keeps the fleet current, the one-line-per-machine fact of
whether it is even *possible* belongs in the enrolment report, where it is
stated once and costs nothing.

`scripts/selfupdate-e2e/run program-files readonly-mount` covers the mechanism
on Linux. Whether a real elevated Windows task can write Program Files is not
something a container can answer.

## Where each layer runs

**Every push — containers, seconds.** `scripts/selfupdate-e2e/run`, and the
`selfupdate` job in `ci.yml`.

The binary under test is the release binary, unmodified. What is faked is the
internet: a container serves the release under a certificate signed by a CA
the test machine's trust store accepts, reachable at `api.github.com` because
Docker's embedded DNS resolves a network alias before forwarding upstream. The
program does a real DNS lookup, a real TLS handshake it really verifies, and a
real download. Nothing in `foundation/selfupdate` was changed to make this
possible, and that is deliberate: a configurable update host is a configurable
place to be handed a binary, and on a Windows machine with an elevated task and
a non-administrator user it would be a privilege escalation whose hash check
verifies the attacker's own hash.

**Before publication — a real machine, four minutes.** `scripts/selfupdate-gate`,
wired into `release.yml` between creating the release as a draft and
un-drafting it.

It fetches the *previously published* release from the real GitHub, installs it
on a machine that has never seen either binary using the same `install.sh` a
client ran — including the real restic download and the systemd user unit —
starts the service, upgrades it to the candidate, and then asks the question
that matters: did the service come back, is it serving, and is it the new
version. If not, the release stays a draft forever and no machine can see it.

There is no earlier place for this. The tag push is what triggers the release
workflow, so "before tagging" is not a moment CI can act on. Publication is,
and `/releases/latest` excludes drafts.

Three backends: a Vultr instance bought for the run and destroyed after
(`scripts/vultr-testbed`), a machine you already have (`--host`), or a
privileged container with a real systemd (`--docker`). The container is what to
iterate against and what CI falls back to when no Vultr key is configured; it
is weaker evidence, because a container that can do anything proves little
about a laptop that cannot.

On leaked instances: the gate destroys its own from a trap, `reap` runs before
the gate as well as after — the run that leaked a box is not the run that can
clean up after itself — and the instance's user-data sets a shutdown timer so a
box whose driver died powers itself off. One hour of the smallest plan per
release.

## What is not covered, and what it would take

- **Windows.** A real scheduled task, `RestartCount 999` with a five-minute
  interval, and the rename-aside that exists because a running image cannot be
  overwritten. `windows-latest` on GitHub's hosted runners is free and can do
  all of it: same technique, `hosts` file plus `Import-Certificate` into the
  machine trust store. This is the largest gap and the cheapest to close.
- **macOS.** A real launchd agent, `KeepAlive`, and Full Disk Access.
  `macos-latest` covers the first two free; TCC needs a real machine with
  somebody clicking a checkbox, and per the plist comment no Mac has ever run
  this program at all.
- **arm64.** Both Apple silicon and arm64 Linux. `AssetName` is tested; the
  download and smoke-test path on a real arm64 machine is not.
- **A machine that has been running for a month.** The gate's baseline is
  installed minutes before it is upgraded, so its database has one schema
  version's worth of history and no backups in it. A real client machine has
  neither. Keeping one long-lived box that is upgraded release after release
  would cover this, and is the one thing worth paying for a persistent instance.
- **The automatic path.** The gate drives `sion-backup update`. The daemon
  reaches the same code from `selfUpdate` after a completed backup, which needs
  a real Eumaeus and a real bucket. The two differ only in the throttle and in
  who exits, but "only" is doing some work in that sentence.

## Adjusting this as telemetry arrives

The table above is a set of guesses about which clients' machines differ in
ways that matter. `fleetbus.Event` already carries `Agent` and `OS`, which is
enough to answer two questions worth asking every few weeks:

- **Which versions are actually out there?** A machine whose `Agent` has not
  moved across two releases while its runs keep succeeding is a machine where
  self-update is not happening. Given the install-location finding above, the
  first thing to check is whether it can write its own binary — and that is the
  case for reporting the answer once at enrolment instead of inferring it.
- **Which `OS` values exist?** Every distinct one is a row this matrix should
  have and does not. With four or five clients the honest move is to enumerate
  their machines by hand and make the matrix match, rather than to cover
  platforms nobody runs.

When a new value shows up, a case is one line in the `CASES` array in
`scripts/selfupdate-e2e/run`.
