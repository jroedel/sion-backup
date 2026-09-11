# sion-backup

Keeps a work computer backed up to an S3 bucket with restic, and — this is the
part that matters — says so.

It is one binary, installed per machine, that schedules its own runs, proves
each one by restoring from it, serves a status page on `127.0.0.1:7391`, and
reports to a central dashboard. Windows, macOS and Linux.

---

## The failure this is built against

Not "the backup broke". The backup breaking is fine; it happens, and it is
fixable on the day it happens.

The failure is: **the backup broke in March and nobody noticed until the laptop
was stolen in September.**

Every design decision here points at that one sentence, and several of them
cost something to get it:

| Decision | What it costs | What it buys |
|---|---|---|
| Every run is verified by restoring a file from it | one extra round trip per run | a repository that cannot be read is discovered that night, not at the restore |
| Exit code 3 is `incomplete`, not success | some runs go amber | you find out which files are missing |
| Every run is recorded, failures included | a database instead of a log | "succeeds every fourth night" is visible; a last-success flag hides it |
| The machine reports even when it has nothing good to say | outbound traffic on every run | the dashboard can list machines that have gone *quiet*, which is the real question |
| No credential is ever stored on the machine | every backup needs Eumaeus reachable | revoking one token cuts off a stolen laptop, completely, in one click |

### What the shell scripts got wrong

This replaces a set of per-machine `backup.sh` and `backup.bat` files. They
worked, and they had four faults that are hard to fix in a shell script and
easy to fix once:

1. **Exit code 3 was treated as success.** `if [ $STATUS -eq 1 ]` on Linux and
   `IF %ERRORLEVEL% NEQ 1` on Windows. restic exits 3 when it wrote a snapshot
   but could not read some files — a locked Outlook data file, say. A backup
   missing precisely the thing the exercise was for reported green every night.

2. **Verification checked that a file existed, not what was in it.** The
   Windows script said so itself: `:: TODO actually check the contents`. A
   half-broken restore produces a zero-length file, which passes that test.

3. **Credentials were in the script**, in plaintext, on the machine, and
   `export`ed into an environment that every child process inherited. The
   repository password — the one thing that can never be rotated — was in a
   `.bat` file in the user's Documents folder.

4. **The repository password was escrowed by an unauthenticated URL.** A Google
   Apps Script endpoint, in the binary, with the bucket name and password as
   query parameters. Anybody who read the binary could read every password.

Each of those is now a test in this repository rather than a comment.

---

## How it works

```
                    ┌──────────────────────────────────────────┐
   you, once        │  sion-backup enroll --code K4TP-9QX2     │
   per machine ────▶│    ├── claims a one-time code from you   │
                    │    ├── stores ONE token, 0600            │
                    │    ├── proves the bucket opens           │
                    │    └── prints the owner's restore card   │
                    └──────────────────────────────────────────┘
                                      │
                                      ▼
   ┌────────────────────────────────────────────────────────────┐
   │  sion-backup daemon           (service / launchd / task)   │
   │                                                            │
   │   scheduler ─── due? ──▶ fetch credentials from Eumaeus    │
   │       │                      │      (used, then wiped)     │
   │       │                      ▼                             │
   │       │                 restic backup --json               │
   │       │                      │                             │
   │       │                      ▼                             │
   │       │                 restore the nonce, compare bytes   │
   │       │                      │                             │
   │       │                      ▼                             │
   │       │                 record the outcome (SQLite)        │
   │       │                      │                             │
   │       │                      ▼                             │
   │       │                 report to Eumaeus ── offline? ──┐  │
   │       │                                                 │  │
   │   status page ◀── 127.0.0.1:7391                        │  │
   │                                              flush later ◀─┘│
   └────────────────────────────────────────────────────────────┘
```

### The three secrets, and where each one lives

| Secret | Held by | On the machine | Rotatable |
|---|---|---|---|
| S3 access key ID | Eumaeus | **never** | yes |
| S3 secret access key | Eumaeus | **never** | yes |
| **restic repository password** | **Eumaeus** | **never** | **no, ever** |

One thing does sit on the machine: a machine token, in a 0600 file, which
authorises "fetch my own credentials" and nothing else. It is revocable in one
click and every use of it is audited, which is exactly what a cached
credential was not.

The third row is not really a credential. restic derives the repository's
master key from it, and there is no recovery path: lose it and every snapshot
that machine has taken becomes an inert pile of ciphertext. The machine it
protects is precisely the machine most likely to be stolen or reimaged by
somebody who did not know. So it lives in Eumaeus, and the machine borrows it
for the length of one backup.

### Nothing is cached, and that was the right trade

An earlier design kept all three in the platform keyring so a laptop could back
up while offline. That capability was never real — a backup writes to a bucket
over the internet, so a machine that cannot reach Eumaeus almost certainly
cannot reach S3 either.

Dropping it bought three things:

- **Revocation works.** One click stops a stolen laptop backing up. With cached
  credentials it stopped the laptop *reporting* while it carried on writing,
  and cutting it off properly meant rotating a password that cannot be rotated.
- **Every use is audited**, so a stolen token being used is a row on the server
  rather than an invisible local read.
- It deleted the most platform-specific code in the repository: a wrapper
  around DPAPI, the macOS Keychain and the Secret Service, plus a file fallback
  for the headless Linux case where none of them answers. About 900 lines, and
  the part hardest to test on any one machine.

It is honest to say what it does not buy. The machine token is still a secret
on a laptop, and whoever takes that file can fetch what the machine can fetch.
The gain is that it is revocable, audited, and narrow.

The cost is that **Eumaeus is on the critical path of every backup**. An outage
means nothing in the fleet backs up — which shows up as every machine going
overdue at once, and at `warn_after_hours` of 240 an outage of a day or two
passes unnoticed.

---

## Getting started

You need Go 1.26.

```sh
make build
./sion-backup restic         # download and verify the pinned restic
./sion-backup paths          # where it will keep things — nothing is in this tree
./sion-backup doctor         # what is missing
```

For a machine rather than a working copy, take the binaries from a
[release](https://github.com/jroedel/sion-backup/releases) and verify them
before running anything:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

Before installing on a machine that is already backing up with the old
scripts — which is most of them — look at what is there:

```sh
./sion-backup recon              # reads and reports; changes nothing
```

It finds the legacy install, says which bucket it has been writing to, what
it backs up, what starts it, and what an upgrade means for that particular
machine. It never prints a credential: the old scripts carry the S3 keys in
plain text, and this says which file they are in and leaves them there. If the
install is somewhere the notes never mentioned — they were done by hand —
point at it with `--legacy-dir`.

`recon` reports. `adopt-enroll` is what acts on the report:

```sh
sudo ./sion-backup adopt-enroll          # asks before it writes anything
```

It takes the plan out of the legacy install — the targets, both halves of the
exclude list, the per-machine tuning, VSS on Windows — writes it as this
machine's own, opens the old repository to count what is in it, and prints the
two Eumaeus commands to run, with the bucket name, node ID, snapshot count and
history horizon already filled in and wrapped in `ssh` so they can be run from
right there:

```sh
ssh -t root@terraboskamp.org 'sudo -u eumaeus \
        EUMAEUS_DATA_DIR=/var/lib/eumaeus \
        EUMAEUS_WASABI_ENV=/var/lib/eumaeus/.config/eumaeus/wasabi-provisioning.env \
        eumaeus backup check'
...
ssh root@terraboskamp.org 'sudo -u eumaeus … eumaeus backup code -issued-by "Your Name" dell3-backup'
```

That prefix is not decoration. `eumaeus` opens a **local** store as its service
account, and without `-u eumaeus` and `EUMAEUS_DATA_DIR` it opens root's own,
which is empty — and an empty store does not refuse, it answers every question
wrongly. `EUMAEUS_WASABI_ENV` is there for the same reason: the provisioning
key is otherwise found through `HOME`, which `sudo -u` may or may not reset.
`ssh -t` because `adopt` reads the repository password from `/dev/tty` rather
than from stdin, so it cannot be piped. `check` goes first because `adopt`
refuses without a usable key — better found before the bucket than after the
password has been typed for nothing.

`--issued-by` puts your name on the enrollment code's audit row; without it the
printed command carries a placeholder, because on the server every admin
command runs as the service account and the environment there only ever says
`eumaeus`. `--ssh` names a different target, user included; `--ssh ""` prints
the commands bare, for somebody already on the server.

These five admin commands are on their way to a web page — see
[sion-backup#34](https://github.com/jroedel/sion-backup/issues/34) — so this
form is current rather than permanent. It is written down in one place,
`adoption.adminCommands`, so that it changes all at once.

Then, with the code that last command issues:

```sh
sudo ./sion-backup adopt-enroll --code K4TP-9QX2
```

The claim tells Eumaeus which bucket this machine has actually been writing to,
and a code that enrols it against a different one is **refused without being
consumed** — adopt the right bucket and present the same code again. After a
successful claim it checks again from this end, against the repository itself.

That check is the reason the command exists. `provision` typed where `adopt`
was meant hands back a working, empty bucket: every step after it succeeds, the
dashboard goes green, and two years of history sit in a bucket nothing points
at until somebody needs a file from 2024.

The installers run it for you and stop before doing anything irreversible:

```sh
./deploy/linux/install.sh                  # or: -ReconOnly on Windows
powershell -File deploy\windows\install.ps1 -Elevated
```

Neither disables the old backup. That is the last step, after the new install
has taken one verified backup: `install.sh --disable-legacy`, or
`install.ps1 -DisableLegacyTask`. Two backup systems for one night is untidy;
none is worse.

An adopted machine gets the set-up page too. The folders came out of a script
that is on its way to being deleted, and "this is what the old backup covered,
is it still right?" is worth asking once, while somebody is standing there.

To set up a machine with no backup on it — the rare case; on one that already
backs up, `adopt-enroll` above replaces steps 1 and 2 and keeps the history:

```sh
# 1. In Eumaeus: sign in, "Enrol a computer", choose the owner and the bucket.
#    It shows a code, good for fifteen minutes, usable once.

# 2. Install the service (install.sh does this for you).
cp deploy/systemd/sion-backup.service ~/.config/systemd/user/
systemctl --user enable sion-backup
loginctl enable-linger "$USER"

# 3. At the machine. This writes the one token it will keep, proves the bucket
#    opens, prints the owner's restore card, starts the service, and opens the
#    page where the person using this computer chooses what is backed up.
#    --server is only needed to point at something other than the fleet's own
#    server, https://terraboskamp.org.
./sion-backup enroll --code K4TP-9QX2
```

Nothing is backed up until that page is answered — see
[Nothing backs up until somebody says yes](#nothing-backs-up-until-somebody-says-yes)
below. `--no-start` and `--no-open` turn off the last two steps, for an install
being driven from a script or over SSH.

macOS and Windows have their own files in `deploy/`. The Windows one is a
PowerShell script; run it elevated, with `-Elevated`, so Volume Shadow Copy is
available and open files get backed up.

---

## Commands

```
sion-backup daemon     the scheduler and the status page (what the service runs)
sion-backup run        one backup now, in the foreground
sion-backup status     the last few runs, as a table
sion-backup enroll     fetch this machine's credentials and prove they work
sion-backup adopt-enroll
                       take over the backup already running here, keeping its
                       bucket and its history
sion-backup recon      what is already on this machine, including the old scripts
sion-backup doctor     check everything a backup needs, and say what is wrong
sion-backup paths      where this program keeps its files
```

`run` exits 0 for a verified backup, 3 for one with files missing, 1 otherwise
— the same shape as restic's own, so a wrapper that understands one
understands the other. `doctor` exits non-zero if any check failed, so it can
be what a monitoring script runs.

---

## Decisions worth arguing with

### The scheduler is ours, not cron's

cron, systemd timers and Task Scheduler all answer "run at 13:00". None of them
answers the question this fleet has, which is: *the laptop was in a bag at
13:00, so when does it back up?*

`Persistent=true` and "run as soon as possible after a missed start" exist
because that is the real question, and each of the three is configured
differently. Owning the scheduling means one behaviour, described in one place,
testable without a machine that sleeps. A run is due when a scheduled time has
passed that the machine has not yet backed up for — one sentence that covers
both the ordinary case and the laptop.

Jitter is derived from the node ID and the date rather than drawn at random, so
a reboot at 13:05 does not roll a new offset and run a second backup.

### 13:00, not 02:00

A work computer is switched on, awake and on a network in the middle of the
working day. That is exactly when an overnight schedule fails. The cost is that
the backup competes with the person using the machine, and read concurrency is
the dial for that.

### One bucket per machine

Restic deduplicates within a repository, not across them, so one bucket for the
fleet would save real space. It would also mean every machine's credentials
open every machine's backups, and one compromised laptop is the whole fleet.

### Rotation is suggested, never imposed

The machine measures its own repository weekly and, when a fresh start would
reclaim something worth having, says so on the status page — with the saving
and the discarded history in the same sentence, and a note that nothing happens
unless the owner asks.

The split is deliberate: the server says whether a new bucket is *allowed*, the
machine works out whether it is *worth it*, and the owner decides. Only the
machine can do the middle part — the figures come from `restic stats` against
the repository. And an organisation that imposes a rotation day finds machines
switched off on it.

There is no "take a full backup and clean up" alternative, because restic has
no full/incremental distinction at all. Every backup uploads only blobs the
repository does not already hold, so a "full" one into an existing repository
uploads almost nothing and frees nothing. `TestMeasureAgainstRealRestic` pins
that against the real binary.

### restic is a subprocess, not a library

restic cannot be imported. Every package in its module lives under `internal/`,
which Go's module rules make unimportable from outside it; using it as a
library means forking it and carrying the fork.

That is the mechanical reason, and the weaker one. The real reason is that the
owner of each machine is handed a card telling them to download the official
restic binary and restore their own files with four environment variables — no
Eumaeus, no us. That promise only holds if their stock binary and ours are the
same program. A fork would make these repositories the output of our build,
and compatibility with the public restic something to hope for.

So restic is a subprocess, and this program installs the one it runs.

Every machine in the fleet runs **exactly** the version pinned in
`foundation/restic/pin.go` — currently 0.19.1. It is downloaded from restic's
own release, checked against a SHA-256 compiled into this binary, run once to
confirm it is what it claims, and kept in the per-user data directory. That
last part is the load-bearing detail: a binary there can be replaced by the
same account that takes the backup, so upgrading restic across thirty laptops
needs no administrator, no package manager, and no second visit. The pin
travels inside the release, so a machine that self-updates picks up a new
restic on the same channel — there is no second rollout to run and no machine
that quietly missed it.

Nothing on `PATH` is used. A machine with a three-year-old snap-installed
0.14 is a machine whose exit codes mean something different from everybody
else's — `classify` in `foundation/restic` exists only because that fleet
used to be real — and "which restic wrote this snapshot" should be a fact
rather than a guess. `sion-backup recon` reports any other restic it finds
and says plainly that it is not the one that will run.

The escape hatch is `server.restic` in the config file: it names a binary to
use as given and never to manage, for a platform the pin has no build for.
`doctor` and `recon` both say when a machine is in that state.

### The status page has no login

Anybody who can reach `127.0.0.1` on the machine is already logged in as that
user: they can read the keyring, the database and the binary. A password would
protect nothing and would guarantee a sticky note on the monitor.

"It only listens on localhost" is not by itself a security model, though, and
`app/sdk/loopback` closes the two specific holes:

- **CSRF.** Any web page the user visits can make their browser POST to
  `127.0.0.1:7391/settings`. Defended with a per-process form token, `Origin`,
  and `Sec-Fetch-Site`.
- **DNS rebinding.** A page on `evil.example` can re-resolve its own hostname
  to `127.0.0.1` and then *read* the responses, because as far as the browser
  is concerned it is same-origin. Defended by requiring the `Host` header to be
  a loopback name — the browser sends the attacker's hostname there.

The daemon refuses to bind anything but loopback, with no override flag.

### Nothing backs up until somebody says yes

Enrolment used to end with a machine that had credentials, a plan somebody else
had written, and a scheduler that would start uploading at the next slot. That
is wrong in two directions at once.

A first backup is the largest thing this program ever does — tens of gigabytes,
hours of an office uplink — and it was starting without the person whose
computer it is having seen what was in it or been told how long it would take.
And what was in it was a guess: an administrator's idea of which folders
matter, made at a desk that is not theirs.

So a plan now carries `confirmed_at`, the scheduler refuses to run against a
plan that has none, and `enroll` finishes by starting the service and opening
the page that clears it:

```
http://127.0.0.1:7391/setup
```

The page asks four questions and measures the answer to each:

- **What to back up.** The standard document folders, or the whole user
  folder — each with its real size, counted on this machine while you watch
  (`foundation/dirsize`). Or a list you write yourself.
- **What to leave out.** One checkbox for caches, downloads and disk images;
  one box for "skip files larger than N GB", which reaches restic as
  `--exclude-larger-than` and is the only exclusion that cannot be written as a
  pattern.
- **How often.** Hourly, three times a day, once a day at a time you pick, or
  your own list. The times, the jitter and the minimum interval move together:
  an hourly schedule with the default six-hour floor would silently run four
  times a day.
- **Your connection.** Measured against this machine's own bucket, with the
  first backup timed from it, and a button to measure again.

There is no third "whole machine" option, deliberately. The daemon runs as the
signed-in user and cannot read other accounts or most of the operating system,
so what that choice would actually produce is a run that reports files missing
every night forever. A work computer's operating system is reinstalled rather
than restored, and a whole-machine backup taken from inside a running system is
not a bootable one anyway. Somebody who wants `/etc` in the list can put `/etc`
in the list.

**The upgrade path is the dangerous part of this, and it is tested.** Every
machine in the fleet has a plan that predates the column. If the migration left
those unconfirmed, taking this release would stop the whole fleet backing up,
on the same day, silently. `plandb.addColumns` backfills them from
`updated_at`, and `TestAMachineThatWasAlreadyBackingUpKeepsBackingUp` is what
holds it there.

`sion-backup run` and the "Back up now" button are unaffected. Both are a
person deciding, which is the thing the gate is waiting for.

### Measuring the upload speed without writing anything

The estimate on that page needs real bytes sent to the real endpoint over the
real path, or it is measuring something else — and a browser speed test is no
use, because these connections are asymmetric and it is the upload this program
spends.

But nothing in this system may delete backup data (§5.4 of
[`docs/model.md`](docs/model.md)), so an ordinary `PUT` would leave a junk
object in somebody's bucket that no credential here could ever remove. So
`foundation/s3probe` starts a multipart upload, sends one timed part, and
aborts it:

```
POST   /bucket/key?uploads            begin
PUT    /bucket/key?partNumber=2&...   the bytes, timed
DELETE /bucket/key?uploadId=...       abort
```

An aborted multipart upload has no object at the end of it and no parts left
behind. It uses `PutObject` and `AbortMultipartUpload` and nothing else — both
of which restic itself requires, because that is how minio-go uploads a pack
file and cleans up after a failed one. The part is sized from an untimed
warm-up to take about eight seconds, clamped between 8 and 64 MiB.

The SigV4 signing is written out by hand in `foundation/s3probe/sigv4.go`
rather than pulling in an AWS SDK for three requests. It is checked against the
worked example AWS publishes — canonical request hash, signing key and
signature, all three.

### The user edits the settings, the administrator does not

Folders, excludes, times and the pause switch are on the status page, because
the person using the machine is the only one who knows that the work they care
about now lives in a folder that did not exist when it was set up.

There are two pages over the same plan, on purpose. `/setup` asks the four
questions in plain words with the sizes measured; `/settings` is the plan as it
is stored, for editing one exclude pattern or one tuning number without walking
through the questions again.

The node ID and the repository URL are *not* on that page. Getting either wrong
silently detaches a computer from its own backup history, so changing them
means re-enrolling, deliberately.

### `config.toml` is a seed, not a source of truth

The plan half of it is read **once**, on a machine that has no plan yet, and
lives in the database from then on. The deployment half — `[eumaeus]` and
`[server]` — is read on every start.

Re-reading the plan at every start would silently undo the excludes somebody
added last week, at the next reboot, which is a genuinely maddening bug to be
on the receiving end of.

---

## Layout

The layering is [Ardan Labs' service architecture](https://github.com/ardanlabs/service),
the same as the sibling project [eumaeus](https://github.com/jroedel/eumaeus).
An import may only point downwards.

```
cmd/sion-backup/     the composition root: wiring, subcommands, and the only
                     place that loads a credential

app/                 delivery. Knows about HTTP; knows nothing about restic.
  domain/statusapp/    the localhost pages: status, set-up, settings
  sdk/loopback/        CSRF and DNS-rebinding defence
  sdk/page/            shared chrome and the one stylesheet

business/            the rules. Knows nothing about HTTP.
  domain/backup/       what a run is, and what counts as a good one
  domain/plan/         what to back up and when; the scheduler
  domain/credential/   the three secrets: fetched, used, wiped
  domain/fleet/        reporting, including "eventually, from a plane"
  domain/survey/       what this machine could back up, how big it is, and
                       how fast it can upload — the setup page's numbers

foundation/          technical leaves. Know nothing about backups.
  paths/               where everything lives, per platform
  restic/              the subprocess, its JSON, its exit codes, and the
                       pinned binary it installs and upgrades itself
  dirsize/             how big a folder is, said to be an estimate
  s3probe/             SigV4, and an upload that is never completed
  netcost/             is somebody paying for these bytes
  secrets/             DPAPI / Keychain / Secret Service / file
  sqldb/               one SQLite connection, held open
  eumaeusapi/          HTTP to the server
  web/                 request middleware
```

Two rules earn their keep:

- **A store never imports a sibling store.** Which is why the migration list
  lives in `main.go` — it is the only place allowed to know about all of them.
- **A business domain never imports a sibling business domain.** The backup
  domain does not know the fleet reporter exists; the composition root converts
  a `Run` into an `Event`. This is also why `backupbus.Run` is handed a
  `restic.Repository` with the credentials already in it: secret handling
  happens in exactly one function. `surveybus` measures the upload speed
  through a `Prober` closure for the same reason — probing needs the S3 keys,
  and the package that renders a page must never be handed one.

### Your data is not in this repository, and cannot be

Nothing this program writes resolves inside the source tree. Not in an ignored
subdirectory — physically outside it. `foundation/paths` has a test,
`TestNoDefaultPathInsideRepo`, that fails if a path is ever added that does.

That is stronger than `.gitignore`, which is advisory: `git add -f`, a symlink,
or a rename defeats it silently. A path that was never inside the tree cannot
be committed, because there is nothing there to stage.

This matters because the repository is public. The run history is not secret in
a cryptographic sense; it is a list of every directory on somebody's work
computer, when their laptop was last online, and which of their files could not
be read. That is theirs.

---

## The Eumaeus side

The contract is these documents, in the order to read them:

| | |
|---|---|
| [`docs/model.md`](docs/model.md) | the fleet model — people, machines, repositories, the alerting state machine, and what survives Eumaeus itself being lost. **Read first, and argue with this one.** |
| [`docs/eumaeus-api.md`](docs/eumaeus-api.md) | the endpoint specification, and the rules a correct server has to follow |
| [`eumaeus/docs/openapi.yaml`](https://github.com/jroedel/eumaeus/blob/main/docs/openapi.yaml) | the same endpoints, machine-readable. **It lives there now** ([eumaeus#121](https://github.com/jroedel/eumaeus/issues/121)): a spec the client maintains has no way to notice the server changing, and ours was four features behind for exactly that reason. A test beside their routing table now fails when a served path is missing from the document, or the reverse |
| [eumaeus issues](https://github.com/jroedel/eumaeus/issues) | what this client still needs from the server. One issue per ask, on their tracker — not a document passed back and forth, which is how the last round went unanswered for a week |

Seven endpoints. The installation at `https://terraboskamp.org` answers all of
them; the client speaks the first, third, fourth and seventh:

```
POST /api/backup/v1/enrollments/claim           code → machine token + credentials
GET  /api/backup/v1/machines/me                 state, and the heartbeat
GET  /api/backup/v1/machines/me/credentials     the secrets, audited
POST /api/backup/v1/runs                        a run event
POST /api/backup/v1/machines/me/rotation-request  the owner asks for a fresh bucket
POST /api/backup/v1/machines/me/card-issued     the owner's card was printed
POST /api/backup/v1/diagnostics                 an install that failed, token or not
```

§11 of the API spec lists what is done on this side and what is not —
including four things the server now does that the specification has not caught
up with.

One rule shapes all seven: **the server owns the facts, the machine reports what
it did.** Eumaeus provisions the bucket, generates the repository password,
mints both S3 keys and decides when a machine is overdue. The machine caches
none of it: every run fetches its credentials and discards them.

Writing the client's half first was deliberate. It pinned the contract down
while it was still prose, and `sion-backup doctor` reports a 404 from a server
that has not implemented an endpoint as clearly as it reports a wrong token.

---

## Known gaps

Named here rather than left to be discovered. These are decisions. Work that is
intended and not yet done is in [`docs/todo.md`](docs/todo.md).

- **Self-update has no staged rollout and no kill switch.** Every machine takes
  the newest published release within an hour of it existing, and there is no
  way to hold one back from part of the fleet or recall one already out. What
  stands in for it is a release gate: the release is published as a draft, the
  previously published version is upgraded to it on a real machine, and it is
  only un-drafted if that machine comes back. **The real answer now exists on
  the server** — `GET /machines/me` carries an `agent` block naming the version
  and the per-platform hash, with `-version none` as the kill switch
  ([jroedel/eumaeus#114](https://github.com/jroedel/eumaeus/issues/114)). The
  remaining gap is ours: `selfupdate.Source` is the seam and nothing implements
  it against Eumaeus yet.
- **A rolled-back machine is protected, but only from the release after the
  one that taught it how.** A new version now starts on probation: if it will
  not stay running it is replaced with the previous one and never installed
  again (`foundation/selfupdate/probation.go`). The catch is that the probation
  file is written by the binary performing the swap, so the first upgrade *into*
  a probation-capable release is itself unwatched. It also only recovers from a
  build that fails after the check at the top of the daemon command — which is
  the realistic case, a migration or a config parse, and the smoke test covers
  failures earlier than that.
- **Self-update never happens on two of the three platforms.** On Windows the
  binary lives in `%ProgramFiles%` and the task runs as the signed-in user; on
  macOS it lives in `/usr/local/bin` and the agent runs as the user. Neither
  can write its own binary, so `Writable()` refuses — correctly, and logged
  once at Info rather than reported, because it will be true again in an hour.
  The consequence is that the dashboard cannot tell a machine that *cannot*
  update from one that has stopped checking in.
- **Windows service.** The daemon is installed as a scheduled task, not a real
  service: it does not implement the service control handler
  (`golang.org/x/sys/windows/svc`). "Restart on failure" gets the same
  practical result, and the task does not appear in `services.msc`.
- **There is no pruning, anywhere, by design.** A `restic prune` on a real
  repository in this fleet took over 24 hours on a gigabit uplink — it
  downloads and re-uploads most of the repository rather than editing
  metadata. Space is reclaimed by rotating the bucket instead. `restic.Forget`
  and the retention policy have been *removed* rather than left unwired, and
  `TestTheRunnerCannotDeleteBackupData` fails if they come back. See
  [`docs/model.md`](docs/model.md) §5.4 — the payoff is that no credential
  anywhere in the system can delete backup data.
- **restic is pinned but not signature-checked.** `foundation/restic/pin.go`
  carries the version and upstream hashes and nothing is installed that does
  not match them — but those hashes were copied from upstream's `SHA256SUMS`
  by hand. Verifying restic's GPG signature when bumping the pin is a manual
  step, documented in `deploy/restic.pin` rather than automated. The hash and
  the binary also both come from GitHub, so it proves the download arrived
  intact rather than that upstream was not compromised; what it does buy is
  that a later substitution upstream is caught, because the hash was taken at
  pin time and ships inside our binary.
- **The pin is written down twice.** `foundation/restic/pin.go` and
  `deploy/restic.pin` hold the same version and hashes, because one has to be
  readable by this program and the other before there is a Go toolchain.
  `TestPinMatchesDeployFile` fails on drift, which is a test standing in for a
  single source of truth.
- **Three of the six endpoints are unimplemented here.** The server at
  https://terraboskamp.org answers all six; this client speaks claim,
  credentials and runs. The hourly state poll, the rotation request and the
  card-issued call are not built — see §11 of
  [`docs/eumaeus-api.md`](docs/eumaeus-api.md) for the list and what each costs.
- **Nothing has been enrolled end to end yet.** Every call is exercised against
  a test server and the run event is checked field by field against the
  specification, but no machine has claimed a real code, so the first real
  enrollment is still the first real test.
- **Bucket and IAM provisioning is not built.** Eumaeus is to hold a Wasabi key
  that creates buckets and mints two keys per bucket — a machine key that may
  delete only under `locks/*`, and a read-only restore key for the owner's
  card. restic still writes and removes a lock file on every run as of 0.19.1,
  so that carve-out is required rather than optional (`docs/model.md` §5.2).
- **The "ask for a fresh bucket" button is not wired up.** The card appears and
  states the trade; the endpoint it should post to
  ([`docs/eumaeus-api.md`](docs/eumaeus-api.md) §8) does not exist yet, so for
  now it tells the owner to ask their administrator.
- **`restic check` is not scheduled.** The cheap structural check should run
  weekly; the `--read-data-subset` one costs egress and should be a decision,
  not a default. Both are read-only, so neither needs a credential the machine
  does not already have.
- **History is one rotation cycle, roughly a year.** Not a bug, but it must be
  stated where people will see it: the status page should say "backups
  available since <date>" rather than implying a retention policy that nothing
  enforces.
- **Self-update trusts GitHub twice.** The machine checks for a newer release
  after each backup, verifies the download against the release's own
  `SHA256SUMS`, and runs it once before installing it — but the binary and the
  hash that vouches for it are published by the same workflow to the same
  host, so the hash proves the download arrived intact and nothing more.
  Eumaeus now names the expected version and hash itself
  ([jroedel/eumaeus#114](https://github.com/jroedel/eumaeus/issues/114)), and an
  absent `agent` block is what means "fall back to the release's own sums" —
  so this gap closes as soon as `selfupdate.Source` is wired to it.
- **The setup page's sizes are an estimate, and say so.** `foundation/dirsize`
  walks the folders and adds up file sizes; restic deduplicates and compresses
  before anything leaves the machine, so the real first backup is smaller — by
  a third or better on documents, by almost nothing on photographs. Its exclude
  matching is also an approximation of restic's, deliberately: the authority on
  what is excluded is restic, at backup time, and a second implementation of
  that language would disagree with the first in a way that shows up as missing
  files. A `restic backup --dry-run` would give the exact figure and needs
  credentials and a round trip; it is not wired up.
- **The measured upload speed is not stored.** It lives in the daemon's memory,
  so a restart loses it and the setup page measures again. That is right for
  the page it was built for and wrong for the status page, which could usefully
  say "this connection uploads at 8 Mbit/s" next to a run that took four hours.
- **`skip_on_metered` does nothing on Windows or macOS.** It is offered because
  the alternative somebody reaches for on a phone tether is pausing backups
  entirely and forgetting, but `foundation/netcost` can only answer on Linux.
  Both pages say so beside the switch rather than implying a protection the
  machine will not give. Same gap as the weekly check's, tracked in
  [`docs/todo.md`](docs/todo.md).
- **No restore UI.** Restores are `restic restore` at a command line, with the
  password out of Eumaeus. That is the right place for a rare, high-stakes,
  supervised operation to start; a button would be worse.

## Releasing

```sh
make tag-revision      # vX.Y.(Z+1)
make tag-minor         # vX.(Y+1).0
make tag-major         # v(X+1).0.0
```

Each reads the highest existing tag, increments the part named, shows what it
is about to do, and pushes an annotated tag. The push is what publishes: the
release workflow builds all five binaries and their `SHA256SUMS`, and every
machine in the fleet updates itself to whatever comes out.

Because of that last sentence, the tag has to come from a commit that stays
reachable — so being on `main` and in step with the remote is not a
precondition you have to arrange. The script checks out `main`, fast-forwards
it to `origin/main`, says both out loud, and tells you afterwards how to get
back to the branch you were on.

What it will not do for you is anything that needs a decision: a dirty tree,
commits the remote has never seen, a history that has diverged, or a tag that
already exists. The first of those is a refusal precisely because this moves
branches, and it will not move one out from under uncommitted work.

`ALLOW_BRANCH=<name>` tags somewhere other than `main` — a real thing to want,
worth saying out loud, and the branch it goes to and updates.

## Contributing

`make check` runs what CI runs: format, vet, tests, and a type-check for all
three platforms. CI additionally runs the tests on all three.

The commenting style here is deliberate and worth matching: comments say
**why**, especially where the code looks odd. If a decision cost something —
a retry that might be wrong, a fallback that weakens a guarantee, an ordering
that matters — the comment should say what it bought and what it cost, so the
next person can disagree with it on the merits.
