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
make restic                  # the pinned restic, verified against deploy/restic.pin
./sion-backup paths          # where it will keep things — nothing is in this tree
./sion-backup doctor         # what is missing
```

For a machine rather than a working copy, take the binaries from a
[release](https://github.com/jroedel/sion-backup/releases) and verify them
before running anything:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

To set up a machine:

```sh
# 1. In Eumaeus: sign in, "Enrol a computer", choose the owner and the bucket.
#    It shows a code, good for fifteen minutes, usable once.

# 2. At the machine. This writes the one token it will keep, proves the bucket
#    opens, and prints the owner's restore card.
#    --server is only needed to point at something other than the fleet's own
#    server, https://terraboskamp.org.
./sion-backup enroll --code K4TP-9QX2

# 3. Take the first backup in the foreground and watch it.
./sion-backup run

# 4. Install the service.
cp deploy/systemd/sion-backup.service ~/.config/systemd/user/
systemctl --user enable --now sion-backup
loginctl enable-linger "$USER"
```

Then open <http://127.0.0.1:7391/>.

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

So restic ships beside this binary, pinned and checksummed, and every run
records which version wrote the snapshot.

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

### The user edits the settings, the administrator does not

Folders, excludes, times and the pause switch are on the status page, because
the person using the machine is the only one who knows that the work they care
about now lives in a folder that did not exist when it was set up.

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
  domain/statusapp/    the localhost page
  sdk/loopback/        CSRF and DNS-rebinding defence
  sdk/page/            shared chrome and the one stylesheet

business/            the rules. Knows nothing about HTTP.
  domain/backup/       what a run is, and what counts as a good one
  domain/plan/         what to back up and when; the scheduler
  domain/credential/   the three secrets: fetched, used, wiped
  domain/fleet/        reporting, including "eventually, from a plane"

foundation/          technical leaves. Know nothing about backups.
  paths/               where everything lives, per platform
  restic/              the subprocess, its JSON, and its exit codes
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
  happens in exactly one function.

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

## The Eumaeus side does not exist yet

The contract is three documents, in the order to read them:

| | |
|---|---|
| [`docs/model.md`](docs/model.md) | the fleet model — people, machines, repositories, the alerting state machine, and what survives Eumaeus itself being lost. **Read first, and argue with this one.** |
| [`docs/eumaeus-api.md`](docs/eumaeus-api.md) | the endpoint specification, and the rules a correct server has to follow |
| [`docs/openapi.yaml`](docs/openapi.yaml) | the same endpoints, machine-readable. `make api-check` validates it and all 29 examples in it |
| [`docs/eumaeus-requests.md`](docs/eumaeus-requests.md) | what this client needs from the server, and the behaviours it now depends on — the document to hand to whoever works on Eumaeus |

Six endpoints. The installation at `https://terraboskamp.org` now answers
under this base path; the client speaks the first, third and fourth of them:

```
POST /api/backup/v1/enrollments/claim           code → machine token + credentials
GET  /api/backup/v1/machines/me                 state, and the heartbeat
GET  /api/backup/v1/machines/me/credentials     the secrets, audited
POST /api/backup/v1/runs                        a run event
POST /api/backup/v1/machines/me/rotation-request  the owner asks for a fresh bucket
POST /api/backup/v1/machines/me/card-issued     the owner's card was printed
```

§11 of the API spec lists what is done on this side and what is not.

One rule shapes all six: **the server owns the facts, the machine reports what
it did.** Eumaeus provisions the bucket, generates the repository password,
mints both S3 keys and decides when a machine is overdue. The machine caches
none of it: every run fetches its credentials and discards them.

Writing the client's half first was deliberate. It pinned the contract down
while it was still prose, and `sion-backup doctor` reports a 404 from a server
that has not implemented an endpoint as clearly as it reports a wrong token.

---

## Known gaps

Named here rather than left to be discovered.

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
- **restic is pinned but not signature-checked.** `deploy/restic.pin` carries
  the version and upstream hashes, and every installer verifies against it —
  but the hashes were copied from upstream's `SHA256SUMS` by hand. Verifying
  restic's GPG signature when bumping the pin is a manual step, documented in
  that file rather than automated.
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
  [`docs/eumaeus-requests.md`](docs/eumaeus-requests.md) §5.2 asks Eumaeus to
  name the expected version and hash instead; `selfupdate.Source` is the seam
  that goes through.
- **No restore UI.** Restores are `restic restore` at a command line, with the
  password out of Eumaeus. That is the right place for a rare, high-stakes,
  supervised operation to start; a button would be worse.

## Contributing

`make check` runs what CI runs: format, vet, tests, and a type-check for all
three platforms. CI additionally runs the tests on all three.

The commenting style here is deliberate and worth matching: comments say
**why**, especially where the code looks odd. If a decision cost something —
a retry that might be wrong, a fallback that weakens a guarantee, an ordering
that matters — the comment should say what it bought and what it cost, so the
next person can disagree with it on the merits.
