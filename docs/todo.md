# Running to-do

What is known to be missing, in the order it is worth doing. Kept here rather
than in issues so that it is read by whoever is reading the code.

The README's "Known gaps" section describes gaps that are *decisions* — the
absence of pruning, the scheduled task instead of a service. This file is for
work that is intended and not yet done. When something here is finished, delete
the entry; when something here turns out to be a decision after all, move it to
the README and say why.

Last reviewed: 2026-09-11.

## The set-up page: what it left undone

`/setup` is live: a newly enrolled machine holds until somebody chooses what is
backed up, with the folders measured and the first backup timed against its own
bucket. Four things it deliberately did not do.

### The size estimate is a directory walk, not restic — NOT STARTED

`foundation/dirsize` adds up file sizes. restic deduplicates and compresses
first, so the real first backup is smaller — by a third or better on documents,
by almost nothing on photographs. The page says so and calls the figure an
upper bound.

`restic backup --dry-run --json` gives the exact answer, applies the real
excludes, and would remove the approximate exclude matcher in `dirsize` as
well. It needs credentials and a round trip to the repository, which is why it
is not on the first render of a page that has to answer immediately. The shape
that would work: walk for the figures beside each choice, `--dry-run` behind
the "count the folders again" button, which is already a deliberate press.

### The measured upload speed is not stored — NOT STARTED

It lives in `surveybus`'s memory, so a daemon restart loses it. Right for the
page it was built for; wrong for the status page, which could usefully say
"this connection uploads at 8 Mbit/s" beside a run that took four hours, and
wrong for the fleet dashboard, which currently cannot tell a slow machine from
a stalled one. `plan_meta` is where it would go, next to the repository
measurement, which is the same shape of fact.

### The probe has never run against a real bucket — THE OBVIOUS GAP

`foundation/s3probe` is tested against AWS's published SigV4 vectors and
against a fake endpoint that checks the request shape. Neither is Wasabi. The
two things that can only be found for real:

- whether the machine key actually carries `AbortMultipartUpload`. restic needs
  it, so it should — but the production key's policy is Eumaeus's to write and
  is not built yet (see the bucket-provisioning gap in the README). Without it
  the probe leaves an incomplete multipart upload rather than nothing, which is
  billed and invisible.
- whether the region guessed out of the endpoint host is the one Wasabi wants
  to be signed under. Wrong region is a clear refusal naming the right one, so
  the failure is loud and costs the estimate and nothing else.

`scripts/backup-e2e` already has a real bucket and real credentials. Adding one
probe to it is the cheapest way to close this, and it belongs there rather than
in the unit tests for the reason everything else in that harness does.

### The dashboard cannot see a machine waiting to be set up — NEEDS EUMAEUS

A machine that is enrolled and unconfirmed looks exactly like one that has
never reported: no runs. That is the state this feature deliberately creates,
so it is now a state worth being able to see — "three machines were enrolled
last week and nobody has answered the page on two of them" is a question an
administrator should be able to ask without visiting desks.

`fleetbus.Event` carries runs, and there is no run to hang this on. It wants
either a field on the enrolment claim or the `/machines/me` state poll that is
already on the unimplemented list (§11 of `docs/eumaeus-api.md`). An issue on
eumaeus, not work here.

## Self-update: the four findings

From `docs/selfupdate-testing.md`, which has the reasoning. Ordered by what a
failure costs, which is also the order to do them in. Finding 1 is done;
2, 3 and 4 are open.

### 1. No rollback — DONE

`foundation/selfupdate/probation.go`. A new version starts on probation; the
check runs at the top of the daemon command before `wire()`, because `wire()`
failing on a new build is the failure being guarded against. Three starts
without staying up and the previous binary is put back, the version is recorded
as refused, and the process exits 75 so the service manager starts the restored
one. `scripts/selfupdate-e2e/run rolled-back` asserts all of it.

Three limits, documented in `docs/selfupdate-testing.md` and worth remembering
before trusting it:

- The first upgrade *into* a probation-capable release is unwatched, because
  the probation file is written by the binary doing the swap. `v0.2.0 → v0.3.0`
  is not protected; `v0.3.0 → v0.4.0` is. The gate prints which case a release
  is in.
- It recovers only from a build that fails *after* the check. Earlier failures
  are the smoke test's job, and it does run the binary before installing it.
- A build that starts and then fails at backup time settles probation first.
  That failure is a failed run on the dashboard from a machine that is still
  reachable, which is the acceptable outcome.

Left undone deliberately: nothing restores a machine that reached `Stuck` — a
version that failed probation with no working binary to go back to. It is
reported and carried on with, because a machine on a bad version is still
better than one with no binary at all, and the honest fix is a fresh install by
a person.

### 2. No staged rollout and no kill switch — MITIGATED, NOT FIXED

Every machine takes the newest published release within an hour of it existing.
There is no way to hold a release back from part of the fleet, and no way to
recall one that is already out.

What exists now is the release gate: the release is published as a draft, the
previously published version is upgraded to it on a real machine, and it is
only un-drafted if that machine comes back. That stops a bad release being
*published*. It does nothing about one that already was.

The real fix is the Eumaeus source — `selfupdate.Source` is the seam, and
[jroedel/eumaeus#114](https://github.com/jroedel/eumaeus/issues/114) is the
ask. It also removes finding 4 and the rate-limit ceiling in one move, because
Eumaeus already tells the machine its own repository password and is therefore
already trusted more than GitHub is.

No longer blocked: the `agent` block is live on the server, so this is ours
to build. An absent block means "verify against the release's own sums",
which is the state the fleet is in until `Source` is wired up.

### 3. The smoke test cannot catch the likeliest brick — NOT STARTED, AND NOW WORTH LESS

`version` returns from main's dispatch (`cmd/sion-backup/main.go:131`) before
`wire()`, so it opens no database, reads no config and never looks for restic.
A build with a broken migration answers it perfectly and dies on `daemon`.

A `sion-backup selftest` that opens the database read-only, parses the config,
and resolves restic would catch most of it before anything is installed, and
`smokeTest` would call that instead of `version`.

Worth less now that finding 1 is done: the machine already recovers from
exactly this build. What a selftest would buy is one less crash loop and one
less rollback report — a nicer failure, not a different one. The exception is
the case probation cannot cover, an upgrade from a release that predates it,
where a selftest is the only guard. Worth doing before the fleet is large
enough for that to matter; not urgent.

### 4. Self-update never happens on Windows or macOS — NOT STARTED

`%ProgramFiles%` with a task running as the signed-in user, and
`/usr/local/bin` with a user agent. Neither is writable by the account that
runs the service, so `Writable()` refuses. That refusal is correct.

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

## Real backups in the gate — BUILT (scripts/backup-e2e)

The gate proves a machine comes back after an upgrade. It does not prove the
machine still backs up, which is the thing the program is for. Nothing in any
test has ever written to a bucket, and nothing has ever restored a file.

### What makes this easy

`foundation/eumaeusapi/eumaeusapi.go:239` refuses plain HTTP except on
loopback. So the gate needs no TLS trick for this: a fake Eumaeus on
`http://127.0.0.1:8088` and `[eumaeus] url` in the machine's config.toml is
enough. Four endpoints under `/api/backup/v1`:

    POST /enrollments/claim        -> a machine token and the credentials
    GET  /machines/me/credentials  -> the same credentials
    POST /runs                     -> 202
    POST /diagnostics              -> 202

The repository URL comes from the credentials response, not from config, so
the fake is where the bucket is injected. Targets and excludes come from
config.toml (`cmd/sion-backup/daemon.go:242`). The fleet's real Eumaeus is
never involved.

### What Wasabi needs to look like

Confirmed working: `sion-backup-gate` in `us-central-1`, list, put, get and
delete all verified by a signed round trip from `scripts/deploy-ready`.

One bucket, dedicated, not production and preferably not the production
sub-account. Object lock OFF -- a default retention makes test repositories
uncleanable. Versioning off. A sub-user with its own key pair and a policy
scoped to that bucket:

    Bucket:  s3:ListBucket, s3:GetBucketLocation, s3:ListBucketMultipartUploads
    Objects: s3:GetObject, s3:PutObject, s3:DeleteObject,
             s3:AbortMultipartUpload, s3:ListMultipartUploadParts

**This key may delete, and the fleet's must never.** That is the whole point of
`TestTheRunnerCannotDeleteBackupData` and of removing `restic.Forget`. Test
repositories have to be cleanable, which is a different job with a different
key in a different bucket. Nobody should copy this policy to production.

Endpoint is `s3.<region>.wasabisys.com`, except us-east-1 which is the bare
`s3.wasabisys.com`. Repository URL:

    s3:https://s3.<region>.wasabisys.com/<bucket>/gate/<date>/<run-id>/<shape>

### Secrets and variables

Split deliberately, and `scripts/deploy-ready` depends on the split:

| | |
| --- | --- |
| Secrets | `WASABI_ACCESS_KEY_ID`, `WASABI_SECRET_ACCESS_KEY` |
| Variables | `WASABI_BUCKET`, `WASABI_REGION` |

Both live in `~/.config/sion-backup/sion-backup-deploy.env` on a machine that
runs the gate by hand, and reach the repository through `make
deploy-propagate`. Until a workflow references the two secrets,
`scripts/deploy-ready` warns that they are set and unused -- which is the
signal that this work is not finished, and clears itself when it is.

A secret cannot be read back, so it can only be tested. A variable can, so the
local value and the repository value are really compared -- and a gate writing
to a different bucket from the one somebody is testing against is exactly the
sort of thing that is never noticed.

### Cost, checked rather than worried about

Wasabi bills a minimum storage duration: deleting an object does not stop it
being billed for the minimum period (90 days on the long-standing pay-go plan;
some plans 30). Verify against the actual plan.

It does not bite at this scale. The standard plan bills a 1 TB minimum per
month regardless, so test data is free at the margin until it crosses that --
roughly twenty thousand runs at 50 MB each. Create a repository per run as
planned; a reaper for prefixes older than N days keeps the bucket legible
rather than cheap.

### What to assert

Not "the backup exited 0". In order of what would actually catch something:

1. **Restore a file and compare bytes.** A backup that cannot be restored is
   not a backup, and nothing in this repository has ever restored anything.
2. `restic check` on the repository -- catches a corrupt pack that a zero exit
   status will not.
3. Two backups with a mutation between them: a changed file, a new file, a
   deleted one. The second is the valuable one, because it is the only one
   that exercises the parent snapshot, dedup and the unchanged-file path.
4. Snapshot count is 2 and the second has a parent.
5. An unreadable file in the target lands in `UnreadableFiles`, and the run is
   recorded degraded rather than failed.

### What it found on its first real run

Two bugs, both in the same place, and neither reachable without a real backup.

**Per-file errors were read from the wrong stream.** restic writes progress
and the summary to stdout and per-file failures to STDERR, as JSON, with the
same envelope. `parseBackup` read stdout only -- so every incomplete backup in
the fleet reported "0 files could not be read" while its outcome said files
were missing, and `Run.UnreadableFiles` was always empty. That list is
described in its own comment as "the list somebody actually needs: which files
are not in my backup", and it could never have contained anything.
`fleetbus.Event.UnreadableFiles` was therefore always 0 on the dashboard too.

**The summary line blanked the error list.** Decoding it unmarshalled over the
accumulated summary in place, and the code that compensated appended a
synthetic "and N more" entry -- replacing real filenames with a count, and
costing a slot in a capped list to do it. Now decoded into a fresh value and
copied field by field, and `Summary.ErrorCount` carries the true total
separately from the capped list, because "4000 files could not be read, here
are the first hundred" and "100 files could not be read" are different
sentences and only one gets somebody to look.

Both are covered by tests written from restic 0.19.1's actual output.

### The weekly repository check — DONE, and the numbers behind it

`restic check` used to be defined and never called, so no machine had ever
verified that what is in the bucket is still what was put there. It runs
weekly now, after a backup, sized from the last measurement, skipped on a
metered connection and recorded either way.

Measured against a real 257 MiB repository, at 3.0 MiB/s:

| operation | time | downloaded | of repository |
| --- | --- | --- | --- |
| `check` (metadata only) | 4.9s | 96 KiB | 0.04% |
| `check --read-data-subset=1/52` | 5.0s | 96 KiB | 0.04% |
| `check --read-data` | 89.8s | 270.7 MiB | 105% |
| one backup of 257 MiB | 234s | 5.7 MiB down, 265 MiB up | — |

Three things follow, and the second is the one worth remembering.

**A full read-data check is not affordable.** It moves the whole repository.
At the rate above, the two-hour budget buys about 21 GB, and client machines
are bigger than that. Nothing schedules it; `--full-read` in the harness exists
to keep the number honest.

**The fraction form silently checks nothing.** `--read-data-subset=1/52`
downloaded exactly as much as a plain check, twice, at two different corpus
sizes -- because it selects whole pack files and a fiftieth of sixteen packs is
none. A verification that verifies nothing and reports success is worse than no
verification, because somebody believes it. That is why `planbus.CheckSubset`
emits a SIZE (`64M`, `393M`) and not a fraction, clamped between 64 MiB and
1 GiB: the floor stops a small repository being skipped, the ceiling stops a
500 GB one costing an evening.

**Metadata-only is nearly free at any size** -- 0.04% here -- so it happens on
every check and only the slice varies.

`restic.Init` remains uncalled, and that one is correct: Eumaeus provisions the
repository. The harness stands in for it.

### Metered connections — HALF DONE

`foundation/netcost` answers "is somebody paying for these bytes". On Linux it
asks NetworkManager about the interface carrying the default route, which is a
real answer. On Windows and macOS it returns Unknown, and that is a gap rather
than a design:

- **Windows**: `NetworkInformation.GetInternetConnectionProfile().GetConnectionCost()`,
  whose `NetworkCostType` distinguishes unrestricted, fixed and variable.
  Reaching WinRT from a program that uses no cgo means hand-rolled COM
  activation. Doable; not done.
- **macOS**: `NWPathMonitor.currentPath.isExpensive`, plus `isConstrained` for
  Low Data Mode. Objective-C.

Until then `[tuning] metered` in config.toml is how a person says what the
machine cannot work out. Note what this gates and what it does not: the weekly
check only. Backups run on any connection, because a backup that did not happen
is the failure this program exists to prevent and no link is expensive enough
to be worth choosing that one instead.

Unknown is deliberately NOT read as metered. Two thirds of the fleet cannot
tell, and the cautious reading would mean no Windows or Mac ever verified a
repository -- trading a certain harm for a possible one. The slice is bounded
instead, so being wrong costs a few hundred megabytes rather than a phone bill.

### Open questions

- Same Wasabi account as production? If so the policy scoping is load-bearing
  and deserves the same treatment the reap tag filter got.
- Real boxes only, or the container matrix too? Keeping the 19 container cases
  hermetic is worth something; this belongs in the real-box gate.
- ~~Blocking or degrading?~~ **Decided: both.** A failed restore or a failed
  integrity check blocks the release; an unreachable bucket only warns. The
  line is one question -- did this tell us anything about the release? The
  harness answers it by probing the bucket FIRST, before anything ambiguous
  happens, so `enroll` failing is never mistaken for Wasabi being down.

  The probe is also why it refuses to degrade when it cannot probe. Degrading
  is how this declines to block a release, so reaching it by accident -- a
  missing tool, a typo -- would be a gate that never gates, and the output
  would look exactly like an outage. That path is `exit 1`, not `exit 2`.
  Found by running it: `curl` was missing from the image and every run
  degraded silently.

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
| `readonly-mount` | A read-only mount, which `Writable()` names as its own cause |

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

One file holds every credential the release path needs, and one command
checks and pushes them:

    mkdir -p ~/.config/sion-backup && chmod 700 ~/.config/sion-backup
    umask 077
    cat > ~/.config/sion-backup/sion-backup-deploy.env <<'ENV'
    VULTR_API_KEY=...
    WASABI_ACCESS_KEY_ID=...
    WASABI_SECRET_ACCESS_KEY=...
    WASABI_BUCKET=sion-backup-gate
    WASABI_REGION=us-central-1
    ENV

    make deploy-ready        # check it, and test every credential live
    make deploy-propagate    # push secrets and variables to the repository

The directory mode is not fussiness. A 0600 file inside a group-writable
directory cannot be read by the group, but it can be renamed and replaced --
which substitutes a credential rather than stealing one, and is the worse of
the two, because the next release would be gated with somebody else's key and
nothing would look wrong.

Without the secret the gate still runs, in a privileged container on the
runner, and says so with a warning. That is weaker evidence — a container that
can do anything proves little about a laptop that cannot — but it is never
skipped, because an ungated release is the thing the gate exists to prevent.

The key is account-wide; Vultr has no per-resource scoping. Restrict it to your
own address in its access control list for local use, and if blast radius
matters, use a separate Vultr account with a small balance for CI.

### Vultr: things learned by actually using it — DONE

Two, both found by the first real run rather than by reading.

**The image name is a prefix, not a name.** Vultr calls it `Debian 12 x64
(bookworm)`, so an exact match on `Debian 12 x64` resolves to nothing and the
run dies before buying anything. Matched on prefix now, with the whole
catalogue printed when the pin stops matching -- because the next Debian will
break it again for the same reason, and the fix should be obvious from the
error.

**`reap` trusted a server-side filter for a destructive call.** It asked Vultr
for `?tag=sion-selfupdate-gate` and deleted everything that came back. The
account also holds `eumaeus`, the fleet's own server, untagged. Had Vultr ever
stopped honouring that query parameter or renamed it, the request would have
succeeded, returned every instance, and reap would have destroyed production.

Now filtered a second time on the client, on the tags each instance actually
reports, and `down` checks the tag before deleting even though it works from an
id it created itself. Verified by handing the client-side filter the
*unfiltered* listing and confirming it acts on nothing.

The general rule, worth remembering the next time something in here deletes
anything: a filter whose failure mode is "destroy production" does not get to
be the only filter.

### v0.3.0 is tagged, gated, and NOT published — NEEDS A DECISION

The tag exists on the remote. The gate never ran: `vultr-testbed up` refused at
its own preflight because Vultr answered

    {"error":"Unauthorized IP address","status":401}

The key is valid; its IP access control list does not include the runner. It
cannot: a GitHub-hosted runner's address changes every run and comes from
ranges far too large to list. The same key is refused from the development box
too, whose egress address is not stable either.

So nothing reached the fleet, which is the design working —
`/releases/latest` still answers `v0.2.0` to an unauthenticated client, which
is exactly what a client machine asks. Nothing was bought: the preflight runs
before the instance is created, so there is no leaked box, and this could not
be confirmed the other way because checking also needs the key.

RESOLVED by rotating the key and removing the IP filtering, which is option 1
below. The key is in this repository's Actions secrets. Note what that means:
it is account-wide, works from anywhere, and the account holds the fleet's own
Eumaeus server. Fork pull requests cannot reach it, and the client-side tag
check above is what stands between a bug in this harness and that server. A
separate Vultr account for CI, funded with a small balance, is still the
tidier arrangement and is worth doing if the harness grows.

What remains is to get v0.3.0 published. Actions re-runs use the workflow file
from the TRIGGERING commit, and the tag points at one that predates the fixes
to the publish path -- so re-running the failed run would still take the broken
path. The tag has to move to a commit that has them. It was never published and
no machine ever saw it, so moving it costs nothing, and a dead tag that never
produced a release is worse to explain later.

### Rehearse the gate before the first tag — SUPERSEDED

Overtaken by events: the first tag was cut before the rehearsal, and the gate
blocked it, which is the outcome the rehearsal was meant to avoid discovering
this way. Kept as a note that the rehearsal is still the cheaper order for the
next credential-dependent thing added to the release path.

    make release GOOS=linux GOARCH=amd64
    scripts/selfupdate-gate --candidate dist/sion-backup-linux-amd64 --version v0.4.0

Publishes nothing, and exercises the credential before a tag depends on it.

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
