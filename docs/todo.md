# Running to-do

What is known to be missing, in the order it is worth doing. Kept here rather
than in issues so that it is read by whoever is reading the code.

The README's "Known gaps" section describes gaps that are *decisions* — the
absence of pruning, the scheduled task instead of a service. This file is for
work that is intended and not yet done. When something here is finished, delete
the entry; when something here turns out to be a decision after all, move it to
the README and say why.

Last reviewed: 2026-09-10.

## Self-update: the four findings

From `docs/selfupdate-testing.md`, which has the reasoning. None are fixed.
They are ordered by what a failure costs.

### 1. No rollback — NOT STARTED

`swap()` keeps the previous binary as `.old` and nothing ever restores it.
`CleanupOld` deletes it at the next startup (`cmd/sion-backup/daemon.go:64`),
which is seconds into the new version's life and hours before the failures that
matter most — a restic incompatibility, a credential fetch that now 400s.

This is the one that turns a bad build into somebody driving to a building, and
it is the highest-value item in this file.

Shape of the fix: the new binary records that it started. On start N with no
completed backup since the swap, it puts `.old` back and reports why. The
counter has to live somewhere that survives a crash-loop, so the database is
the wrong home — a file beside the binary, or the diag store.

Blocked on nothing. `scripts/selfupdate-e2e/run bricked` already reproduces the
crash loop, so the fix has a test before it has an implementation: that case
should end with the machine back on the old version instead of looping.

### 2. No staged rollout and no kill switch — MITIGATED, NOT FIXED

Every machine takes the newest published release within an hour of it existing.
There is no way to hold a release back from part of the fleet, and no way to
recall one that is already out.

What exists now is the release gate: the release is published as a draft, the
previously published version is upgraded to it on a real machine, and it is
only un-drafted if that machine comes back. That stops a bad release being
*published*. It does nothing about one that already was.

The real fix is the Eumaeus source — `selfupdate.Source` is the seam, and
`docs/eumaeus-requests.md` §5.2 is the ask. It also removes finding 4 and the
rate-limit ceiling in one move, because Eumaeus already tells the machine its
own repository password and is therefore already trusted more than GitHub is.

Blocked on the Eumaeus side of §5.2.

### 3. The smoke test cannot catch the likeliest brick — NOT STARTED

`version` returns from main's dispatch (`cmd/sion-backup/main.go:131`) before
`wire()`, so it opens no database, reads no config and never looks for restic.
A build with a broken migration answers it perfectly and dies on `daemon`.

A `sion-backup selftest` that opens the database read-only, parses the config,
and resolves restic would catch most of it, and `smokeTest` would call that
instead. It cannot catch everything — that is what finding 1 is for — so it is
worth less than the rollback and should not be done first.

### 4. Self-update never happens on Windows or macOS — NOT STARTED

`%ProgramFiles%` with a task running as the signed-in user, and
`/usr/local/bin` with a user agent. Neither is writable by the account that
runs the service, so `writable()` refuses. That refusal is correct.

Two separate pieces of work, and they are worth doing in this order:

**Report it once, at enrolment.** Today the dashboard cannot tell a machine
that *cannot* update from one that has stopped checking in — both look like an
`Agent` version that stopped moving. Whether self-update is even possible is
one boolean, known at enrolment, and costs nothing to send. Small, and it makes
the rest of this visible.

**Then decide where the binary lives.** The options are a writable install
location, or a privileged helper that does the swap. Both are real changes to
the deployment story, and the Windows one is entangled with the dedicated
system account in `deploy/systemd/sion-backup.service`'s comment and the
machine-scope paths that needs. Do not start here.

## Gate coverage

### Windows and macOS gate scripts — NOT STARTED

The largest testing gap, and free: `windows-latest` and `macos-latest` hosted
runners are unlimited on a public repository. Same technique as the Linux gate
— a `hosts` entry and the harness CA in the machine trust store — driving a
real scheduled task and a real launchd agent.

Deliberately not written yet: untested PowerShell and launchd in a release gate
is worse than a named gap, because a gate that fails for its own reasons gets
switched off.

macOS Full Disk Access still needs a real machine with somebody clicking a
checkbox. Per the plist comment, no Mac has ever run this program at all, so
the first one is a test and not a deployment.

### The machine-config budget: 4 of 12 used

`scripts/selfupdate-e2e/run` has 19 cases but only four distinct *machines*.
The other fifteen vary the release or the version relationship on one machine.
The cap is twelve, and new ones get added when telemetry shows a real machine
that differs — not to fill the budget.

Used:

| Config | Stands for |
| --- | --- |
| `user-local-bin` | Linux via `install.sh`; the only shape that updates today |
| `program-files` | Windows: admin-owned directory, task as the user |
| `elevated-task` | Windows with `-Elevated`: the same directory, writable |
| `readonly-mount` | A read-only mount, which `writable()` names as its own cause |

Candidates, in rough order of how likely they are to be real:

- macOS `/usr/local/bin` with a user agent. Probably identical to
  `program-files` on Linux, which is exactly why it should be confirmed rather
  than assumed.
- A home directory on NFS or a roaming profile, where the rename in `swap()`
  may not be atomic and `.new` may not land on the same filesystem.
- arm64, both Apple silicon and arm64 Linux. `AssetName` is unit-tested; the
  download and smoke test on real hardware are not.
- A machine with a month of history — a database with several schema versions
  and real backups in it. Needs a long-lived box, not a just-in-time one, and
  is the one thing worth paying for a persistent instance.
- A machine where `$HOME` moved after install, so the binary and the data
  directory disagree.

### The automatic path is untested — NOT STARTED

Both harnesses drive `sion-backup update`. The daemon reaches the same code
from `selfUpdate` after a completed backup, which needs a real Eumaeus and a
real bucket. The two differ only in the throttle and in who exits, but that
"only" is carrying weight: the exit-75-into-a-restart chain is modelled by
`scripts/selfupdate-e2e/supervise` and asserted against systemd's effective
`Restart=` policy, never actually executed by the daemon itself.

## Operations

### Set the Vultr key — READY, NEEDS A PERSON

    gh secret set VULTR_API_KEY          # the release gate
    mkdir -p ~/.config/sion-backup       # running the gate by hand
    umask 077
    printf 'VULTR_API_KEY=%s\n' "..." > ~/.config/sion-backup/vultr.env

Without the secret the gate still runs, in a privileged container on the
runner, and says so with a warning. That is weaker evidence — a container that
can do anything proves little about a laptop that cannot — but it is never
skipped, because an ungated release is the thing the gate exists to prevent.

The key is account-wide; Vultr has no per-resource scoping. Restrict it to your
own address in its access control list for local use, and if blast radius
matters, use a separate Vultr account with a small balance for CI.

### Rehearse the gate before the first tag — NOT DONE

The gate has never run inside the release workflow. Run it by hand against a
real instance first, with the version of a release that does not exist yet:

    make release GOOS=linux GOARCH=amd64
    scripts/selfupdate-gate --candidate dist/sion-backup-linux-amd64 --version v0.3.0

That exercises the Vultr path, the key, and the full upgrade from the published
release, and it publishes nothing. Tag only after it passes.

### The GitHub rate-limit ceiling — WATCH, DO NOT FIX YET

`Latest` makes two unauthenticated requests, GitHub allows sixty an hour per
address, and `jitter_minutes` defaults to 30 — so thirty machines behind one
office address make sixty requests inside a half-hour window. Past the line
every machine files a `KindUpdateFailed` report nightly while every backup
succeeds, which is exactly the noise `diagbus` exists to avoid.

At four or five clients this is nowhere near a problem. It becomes one at a
single client with roughly thirty machines. Do not add a conditional request
now: finding 2's Eumaeus source removes the question entirely, and two
mechanisms for the same problem is worse than one.

Watch for: `KindUpdateFailed` reports whose detail contains `403`.

## Reviewing this file

The machine-config list above is a set of guesses about which clients' machines
differ in ways that matter. `fleetbus.Event` already carries `Agent` and `OS`,
which is enough to check them every few weeks:

- A machine whose `Agent` has not moved across two releases while its runs keep
  succeeding is a machine where self-update is not happening. Given finding 4,
  check first whether it can write its own binary.
- Every distinct `OS` value is a row the budget above should have and does not.

With four or five clients the honest move is to enumerate their machines by
hand and make the table match, rather than to cover platforms nobody runs.
