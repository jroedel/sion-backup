# The fleet model

**Status: design, under discussion.** This is the thing to get right before
[`eumaeus-api.md`](eumaeus-api.md) becomes a real specification. Endpoints are
easy to change while they are prose; a domain model is not, once thirty
machines are reporting into it.

It replaces a Google Sheet with 22 columns, one row per machine, maintained by
hand. §7 maps every one of those columns to its new home, because the sheet
cannot be sunset until nothing on it has been lost.

---

## 1. The entities

Five, and the shape of the thing is mostly in how they relate.

```
  Person ──owns──▶ Machine ──backs up to──▶ Repository
    │                 │                          │
    │                 ├──▶ Run          (many, the history)
    │                 ├──▶ AlertState   (exactly one)
    │                 └──▶ MachineToken (one, issued at enrollment)
    │
    └──▶ RestoreRequest
```

### 1.1 Person

Somebody who can log in. The sheet had no such thing — it had an email address
in a column — and adding it is what makes "select whose computer I'm setting
up" possible.

| Field | Notes |
|---|---|
| `id` | |
| `name` | Shown on the dashboard: "whose laptop stopped" is the question |
| `email` | Where warnings go |
| `role` | `admin` or `owner` |
| `active` | A person who has left keeps their machines' history but stops receiving mail |

**Two roles, and the split is by what somebody can act on.**

| | admin | owner |
|---|---|---|
| See the whole fleet | yes | no |
| See their own machines' status | yes | yes |
| Enrol a machine | yes | no |
| Read a repository password | yes, audited | **no** |
| Rotate keys or buckets | yes | no |
| Request a restore | yes | yes |
| Receive staleness warnings | own machines + weekly digest | own machines |

An owner cannot read a password. They have no operation that needs one — a
restore is a request, not a self-service button — and a password an owner can
read is a password that leaves with them.

### 1.2 Machine

One physical computer. The sheet's `Dataset` column, plus everything that is
true of the machine rather than of a run.

| Field | Source | Notes |
|---|---|---|
| `node_id` | admin, at enrollment | Stable, human-chosen. `office-laptop-1`, not `DESKTOP-4KJ2P1` |
| `owner_id` | admin, at enrollment | The Person |
| `hostname` | client | The machine's own name, which may change |
| `os` | client | `windows/amd64` |
| `local_account` | client | **Which OS account runs the daemon.** Not trivia: DPAPI and Keychain bind the cached credentials to it, so a machine whose account changed is a machine that can no longer read its own credentials |
| `agent_version` | client, every run | The sheet's `Script version` |
| `keyring_mechanism` | client | `DPAPI` / `Keychain` / `Secret Service` / `file`. The last one is a finding |
| `warn_after_hours` | admin | Default **240**. Lower for machines that matter more — the admin's own was 49 |
| `paused_until` | owner | See §4.3 |
| `state` | derived | `active` / `retired` |
| `last_seen_ip` | **server-observed** | The sheet's `Last IP`. Taken from the connection, never from the client — a client-reported IP is a client-asserted IP |
| `last_started_at`, `last_verified_at`, `last_failed_at` | derived from Runs | Denormalised because every dashboard row needs them |

### 1.3 Repository

Where one machine's backups go. Split out of the machine because it has its own
lifecycle: it is created, it is rotated, and the old one is deleted.

| Field | Notes |
|---|---|
| `machine_id` | |
| `provider` | `wasabi`, `backblaze`, … — the sheet's `Bucket` column |
| `region` | `us-central-1` |
| `bucket` | The sheet's `Bucket name` |
| `url` | The restic URL the client actually uses. Derived from the three above, and stored, because the client must send back verbatim what it was given |
| `restic_password` | **In the vault.** The one irreplaceable secret |
| `aws_access_key_id`, `aws_secret_access_key` | In the vault |
| `keys_rotated_at` | The sheet's `Last key rotation` |
| `created_at`, `retired_at` | |
| `state` | `active` / `cutting-over` / `retired`. See §5 |

The URL is stored as well as its parts. It looks redundant and is not: the
client keys its local plan and its keyring entries on the exact string, and a
server that reassembled the URL from parts would eventually reassemble it
differently — a changed region hostname, a trailing slash — and the machine
would silently look unenrolled.

### 1.4 Run

One backup attempt. The sheet had room for the last one; this keeps them all,
because the question "is this machine backing up properly" is answered by the
shape of the history and not by the most recent row. A machine that succeeds
every fourth night looks perfect in a last-result column.

Fields are as in [`eumaeus-api.md` §6.1](eumaeus-api.md), plus a server-side
`received_at`. The one that matters here:

> **`verified` is the field the sheet could not have.** It records that a file
> written moments before the run came back out of the repository byte for byte.
> `last_verified_at` — not `last_succeeded_at` — is what the staleness clock
> runs on.

### 1.5 AlertState

Exactly one per machine. This is the sheet's `Has recovery pending?`,
`Last warning sent`, and the weekly email cadence, made explicit. §4.

---

## 2. Enrollment is a login

The old flow — mint a token, paste it into `config.toml`, delete the line
afterwards — is replaced. Three reasons it was wrong: it put a long-lived
secret in a text file, it had no idea who owned the machine, and every token
could read every other machine's escrow entry.

### The flow

```
  admin, in a browser              admin, at the machine
  ─────────────────────            ─────────────────────
  Eumaeus → Enrol a computer
    ├── who owns it?  [ Fr. N ▾ ]
    ├── node id       [ ______ ]
    ├── bucket        [ ______ ]
    └── ▶ Create

         ENROLLMENT CODE
            K4TP-9QX2              $ sion-backup enroll --code K4TP-9QX2
         valid 15 minutes  ─────▶
                                   ├── POST /enrollments/claim
                                   │     code + hostname + os + local_account
                                   ◀── machine token (persistent)
                                   │   + repository url
                                   │   + restic password
                                   │
                                   ├── stores all three in the platform keyring
                                   ├── proves the bucket opens
                                   └── prints the recovery sheet  (§6)
```

The owner is chosen in the web UI, where there is a list of people to pick
from, rather than typed at a command line where it would be a string with a
typo in it. The code is short, single-use, and expires — the admin carries it
across the room, not across a month.

**The machine token is bound to the machine.** It can read and write that
machine's escrow entry, and post that machine's run events. Nothing else. This
is what closes the hole in the previous design, where one stolen laptop yielded
the repository passwords for the whole fleet.

A machine that is reimaged is re-enrolled: a new code, a new token, the same
`node_id`, and — critically — **the same repository password**, fetched from
the escrow rather than generated. Its existing snapshots stay readable.

---

## 3. Two independent things can be wrong

The sheet had one alarm: hours since the last backup. That conflates two
failures that need different people to do different things.

### 3.1 Staleness — *the machine is not backing up*

Measured as `now − last_verified_at` against `warn_after_hours`.

Actionable by **the owner**: turn the laptop on, plug it in, connect to
something. So the owner is who gets the email.

### 3.2 Quality — *the machine is backing up, and something is wrong with it*

The outcome of the most recent run.

| Outcome | Resets the staleness clock? | Who hears about it |
|---|---|---|
| `success` | yes | nobody |
| `degraded` | yes | dashboard amber |
| `incomplete` | yes | dashboard amber |
| `unverified` | **no** | **admin, immediately** |
| `failed` | no | nobody yet — staleness will catch it |

Two rows deserve their reasoning.

**`degraded` and `incomplete` reset the clock.** A snapshot exists and can be
restored from; it is short a locked Outlook file, or was taken without VSS.
Treating that as "not backing up" would put a permanent alarm on every Windows
machine whose user is not an administrator, and an alarm that is always on is
an alarm nobody reads.

**`unverified` does not, and alerts at once.** restic said it wrote a snapshot
and nothing came back out of it. The machine believes it is protected and may
not be. Waiting 240 hours to mention that would be waiting through ten more
runs of the same. It goes to the admin rather than the owner because there is
nothing an owner can do about it.

---

## 4. The alerting state machine

This is `Has recovery pending?` and `Last warning sent`, done properly.

```
                    last_verified_at older than
                       warn_after_hours
        ┌────┐  ────────────────────────────────▶  ┌─────────┐
        │ ok │                                     │ overdue │ ──┐
        └────┘  ◀────────────────────────────────  └─────────┘   │ every
           ▲         a verified run arrives             │        │ warn_repeat_hours
           │         → RECOVERED email                  └────────┘ → WARNING email
           │
        ┌────────┐                          ┌─────────┐
        │ paused │ ──── paused_until ────▶  │ retired │  no alerts, ever
        └────────┘      expires             └─────────┘
```

| Setting | Default | Notes |
|---|---|---|
| `warn_after_hours` | 240 (10 days) | Per machine. The admin's own was 49 |
| `warn_repeat_hours` | 168 (weekly) | Matches what the sheet was doing by hand |
| `max_pause_hours` | 720 (30 days) | §4.3 |

### 4.1 The emails

**WARNING**, to the owner, repeated weekly while overdue:

> Your computer *office-laptop-1* has not backed up since **31 July**, 11 days
> ago. Backups run automatically when it is switched on and connected — if it
> has been off or travelling, turning it on and leaving it a few hours is
> usually all it needs. If that does not work, reply to this email.

Says what to do. A warning that only states a fact trains people to filter it.

**RECOVERED**, to the owner, once, on the transition back:

> *office-laptop-1* is backing up again — a verified backup completed at 16:36
> today. Nothing further is needed.

The transition email is the whole reason the state machine exists rather than a
nightly "is it stale" query. Somebody who has been nagged four times needs to
be told it stopped mattering, or the next warning means nothing.

**The admin digest**, weekly, one email: everything overdue, everything
degraded, every machine whose offline copy is stale, every machine still on an
old agent version. Plus an immediate email for any `unverified` run.

A digest rather than per-machine escalation: thirty machines producing
individual escalations is how an inbox becomes a filter rule.

### 4.2 The `warn_after_hours` default

240 hours is ten days, which sounds enormous for a backup and is right for this
fleet. These are laptops belonging to people who travel; a week away with the
machine shut is normal, and an alarm on day two would be wrong four times a
month. Machines that are always on — a desktop, the server — should be set much
lower, which is what the admin's own 49 was doing.

### 4.3 Pausing

The owner can pause backups from their own machine's status page — for a
metered connection or a long trip — and that suppresses alerts, or it is not
really a pause.

It expires. `max_pause_hours` defaults to 30 days, after which the machine
alerts again regardless. A pause with no expiry is how a machine stops backing
up permanently, quietly, with a note in the database explaining that it was
intentional once.

---

## 5. Buckets: provisioning, rotation, and three keys

Eumaeus holds a Wasabi API token and provisions buckets itself. The token can
create buckets and keys; it **cannot delete a bucket**. Bucket deletion stays a
human action in the Wasabi console, which is the right place for the one
operation that destroys an organisation's backups irreversibly.

### 5.1 Two keys per bucket

Provisioning a bucket creates two access keys — it was three until pruning was
dropped. This is the change that makes the rest of the design safe, and it
costs nothing but a few API calls.

| Key | Permissions | Held by | Why |
|---|---|---|---|
| **machine** | `PutObject`, `GetObject`, `ListBucket`, and `DeleteObject` **on `locks/*` only** | the laptop's keyring | Enough to back up, and nothing else. See §5.3 |
| **restore** | `GetObject`, `ListBucket` — read only | printed on the owner's card (§6.4) | A lost card exposes their own data; it cannot destroy it |
| ~~prune~~ | — | **nobody. It does not exist** | This fleet does not prune (§5.4) |

Two keys, not three. Neither can delete the bucket, touch another bucket, or
delete a single byte of backup data — the machine key's only delete permission
is under `locks/`.

> **No key in this system can destroy a backup.** That is a stronger sentence
> than any design here started out able to make, and it is a consequence of
> not pruning rather than of clever policy. It costs the ability to reclaim
> space inside a live repository, which is what bucket rotation is for.

**Why the machine key cannot delete data.** A laptop that is ransomwared, or
stolen unlocked, hands the attacker whatever that machine can do. If the machine
key can delete objects, the attacker can destroy the off-site copy — which is
the one thing an off-site copy exists to prevent. Append-only means the worst a
compromised laptop can do to the repository is add to it.

### 5.2 What Eumaeus's Wasabi key can and cannot do

Eumaeus holds a Wasabi key that manages IAM — users, policies, access keys —
and creates buckets. This is the right trade: the alternative is per-bucket
IAM done by hand three times per machine, which is toil that does not get done,
and a fleet where half the buckets quietly share one key because it was
quicker.

The rest of this section is about the reservation, because the obvious one does
not do what it looks like it does.

#### Withholding `DeleteBucket` buys almost nothing

Two reasons, and the second is fatal.

**A bucket must be empty before it can be deleted.** The destructive act is
emptying it, and that is `DeleteObject` — which no key in this system holds
any more (§5.4), but which Eumaeus's IAM rights let it mint at will. An attacker who erases every object leaves an empty bucket standing:
the reservation held, and the backups are gone.

**A key that can write IAM policies can grant itself anything.** With
`iam:CreateUser`, `iam:PutUserPolicy` and `iam:CreateAccessKey`, a compromised
Eumaeus creates a new user, attaches a policy allowing `s3:DeleteBucket`,
issues a key, and uses it. The deny on the *provisioning* key is bypassed by
not using the provisioning key. AWS solves this with permissions boundaries;
**Wasabi's documentation does not describe boundary support**, so this cannot
currently be assumed — it is worth asking Wasabi support directly before any
design leans on it.

So: keep the `DeleteBucket` deny. It costs nothing and it stops a fat-finger.
Do not count it as security.

#### Object immutability does not fit restic. Withdrawn.

An earlier draft of this section proposed compliance-mode immutability with a
30-day window, on the argument that with a 90-day retention floor prune would
never touch a locked object. **That argument was wrong**, and checking restic
0.19.1's `decidePackAction` shows three independent reasons:

1. **Unindexed packs are removed immediately, with no age check.** A pack
   uploaded by a backup that was interrupted before it wrote its snapshot is
   referenced by nothing, and prune deletes it on sight — minutes after it was
   written. On a fleet of laptops, "suspended mid-backup" is not an edge case,
   it is Tuesday.

2. **Small packs are repacked even when 100% used.** A pack below the target
   size is repacked and its original deleted, regardless of age or of every
   blob in it being live. Daily incremental backups produce small packs; this
   is the mechanism by which prune touches recent data constantly.

3. **restic sets no object-lock headers on upload.** There is no
   `x-amz-object-lock-retain-until-date` anywhere in its S3 backend, so
   immutability could only come from a bucket-wide default retention — which
   would also lock the `locks/` objects restic writes and deletes on *every
   run*. That breaks backups, not just pruning.

Point 3 is the fatal one, and it is independent of prune: object-level
immutability and restic-on-S3 are simply not compatible without changes to
restic.

#### What to reserve instead: versioning, and the version-delete permission

Bucket versioning gives soft-delete, and unlike object lock it is invisible to
restic. A `DeleteObject` becomes a delete marker; the data is still there.

```
  every key Eumaeus mints         DENIED:  s3:DeleteObjectVersion
                                           s3:PutBucketVersioning
                                           s3:PutLifecycleConfiguration
  the Wasabi account root         holds all three   (in the safe)
```

- restic deletes lock files and packs exactly as it always has, and sees them
  as gone. Nothing changes for the client. ✓
- A lifecycle rule expires noncurrent versions after **30 days**, so pruning
  still reclaims space — just 30 days later than it otherwise would. ✓
- A ransomwared laptop, or a mistaken prune, destroys nothing permanently.
  Every version is recoverable for 30 days by the account root. ✓

**Be honest about what this is.** It is a *recovery window*, not prevention.
An attacker who has compromised Eumaeus has IAM and can grant themselves
`DeleteObjectVersion`, exactly as they could grant themselves `DeleteBucket`
(§5.2). What versioning genuinely defends against is the two failures that
actually happen: a compromised **machine**, which has no IAM at all, and a
**mistake** — a prune with the wrong policy, a rotation that deletes the wrong
bucket. Those are far more likely than a server compromise, and previously
nothing defended against them.

#### Prevention against a compromised Eumaeus needs a second account

Nothing inside one Wasabi account can constrain a key that manages that
account's IAM. If prevention rather than a recovery window is wanted, it has to
live somewhere Eumaeus cannot reach: a replication target in a second provider
or a second account, whose credentials Eumaeus does not hold and never sees.

That is a real option and a bigger conversation than this document. It is
recorded here so that "we handed over the keys and nothing enforces the
reservation" is a decision that was made, rather than one that happened.

#### Shape of the IAM Eumaeus creates

One user per (machine, role), with an inline policy naming exactly one bucket.
Groups buy nothing here because no two policies are alike — every one is
scoped to a different bucket.

```
  user  sion-backup/office-laptop-1-machine   → write + DeleteObject on locks/*
  user  sion-backup/office-laptop-1-restore   → read only
  user  sion-backup/office-laptop-1-prune     → full object access
```

All of them under the IAM path `/sion-backup/`, so the provisioning key's own
IAM permissions can be scoped to `.../user/sion-backup/*` and it cannot touch
the admin's account. That is defence against a mistake rather than against an
attacker — see the escalation problem above — but mistakes are the more common
failure.

**S3 key rotation becomes a scheduled job.** The sheet's `Last key rotation`
column was manual and therefore aspirational. With Eumaeus holding IAM, machine
and restore keys can rotate every 90 days automatically: mint the new key, push
it to the machine, delete the old. This is cheap precisely because it is *not*
the repository password — the keys are replaceable and the password is not.
A rotated restore key does mean reissuing the owner's card (§6.4), which argues
for rotating restore keys on the bucket cycle rather than the 90-day one.

### 5.3 The lock-file carve-out, which is still necessary

**restic has not fixed this, and does not intend to.** Checked against 0.19.1:

- `restic backup` calls `openWithAppendLock`, which despite the name is simply
  a *non-exclusive* lock. It writes `locks/<id>` at the start and deletes it at
  the end, on every run.
- Stale locks are only cleaned by `restic unlock`, which is manual. A laptop
  suspended mid-backup leaves one behind, and every later run fails with exit
  11 until something removes it.
- restic's own documentation says append-only is a property of the *backend* —
  rest-server implements it, "few other standard backends do" — and recommends
  putting rclone in between for the ones that don't.

So a policy with no `DeleteObject` at all breaks restic, exactly as it did
years ago. The fix is not to grant delete or to give up, but to **scope delete
to the `locks/` prefix**:

```jsonc
// machine key — append-only for data, self-managing for locks
{
  "Effect": "Allow",
  "Action": ["s3:PutObject", "s3:GetObject", "s3:ListBucket"],
  "Resource": ["arn:aws:s3:::BUCKET", "arn:aws:s3:::BUCKET/*"]
},
{
  "Effect": "Allow",
  "Action": ["s3:DeleteObject"],
  "Resource": ["arn:aws:s3:::BUCKET/locks/*"]   // and nowhere else
}
```

`locks/` is where the default repository layout puts them
(`internal/backend/layout/layout_default.go`), and it holds no backup data —
only short-lived coordination files. A compromised machine can delete every
lock in the repository and the worst it achieves is letting two restics run at
once.

This also lets the client keep self-healing stale locks after a suspend, which
it already does: `backupbus` calls `restic unlock` when a run fails with exit
11, and that is the single most common way a machine in this fleet quietly
stops backing up.

**Commands that delete anything outside `locks/`** — and therefore need the
prune key — are `forget`, `prune`, `tag`, `rewrite`, and `key remove`. **None
of them is ever run** (§5.4), so no key needs to exist that could. `backup` is
not among them, which is what makes that possible.

### 5.4 There is no pruning. Rotation reclaims the space.

**Decided, on measurement.** A single `restic prune` on a real repository in
this fleet, over a gigabit uplink, took more than twenty-four hours. That is
not a tuning problem to be solved with more connections.

The reason is in what prune does. It is not a metadata operation: for every
pack it wants to consolidate it downloads the pack, extracts the blobs still
live, writes and uploads a new pack, and deletes the original. Reading
`decidePackAction` in restic 0.19.1 shows how much that catches:

- a pack present in the repository but **not referenced in the index** is
  removed immediately, with no age check — which is every pack uploaded by a
  backup interrupted before it wrote its snapshot, and on a fleet of laptops
  that is routine;
- a pack **smaller than the target size is repacked even when 100% of its
  blobs are in use**, so ordinary daily incrementals generate repack work
  indefinitely;
- everything partly-used is a repack candidate.

So a prune moves a large fraction of the repository in *both* directions. On
the machines this is for — laptops on hotel wifi and domestic uplinks — that
is not a maintenance window, it is a week.

#### What follows from not pruning

The simplification runs further than the schedule.

| | had we pruned | as decided |
|---|---|---|
| Keys per bucket | 3 | **2** |
| A delete-capable credential exists | yes, on Eumaeus | **no, nowhere** |
| Scheduled Eumaeus job reaching every bucket | required | **does not exist** |
| Retention policy | required — and count-based retention is unsafe against an append-only repository | **does not exist** |
| Deletes inside a live repository | routine | **never** |
| History horizon | as long as you like | one rotation cycle |

The second row is the prize. With no prune key to mint, and the machine key
able to delete only under `locks/`, **there is no credential anywhere in this
system that can destroy a backup.** Not a stolen laptop, not a compromised
Eumaeus, not a mistaken maintenance job. The repository is append-only with no
exception carved out for housekeeping — which is exactly the exception every
append-only design usually has to make.

`restic.Forget` and the `Retention` type are removed from `foundation/restic`
rather than left unwired, and `TestTheRunnerCannotDeleteBackupData` fails if
either returns. A dead function that quietly requires a delete-capable
credential is a trap for whoever wires it up in two years.

#### The history horizon, stated honestly

History is the age of the current repository: roughly a year.

That is a real limit and it must be visible rather than implied. The inherited
retention policy — 24 monthly, 5 yearly — was never achievable alongside
rotate-and-delete, and it is gone. The status page should say *"backups
available since 14 March 2026"*, and a rotation should warn that snapshots
older than the cutover will be discarded, because "can I have a file from
eighteen months ago" deserves a straight answer before it is asked.

Anything needing multi-year retention — accounts, legal records — is a
different problem with a different tool, and should not be smuggled in as a
backup policy that nothing enforces.

### 5.5 Rotation: triggered by growth, not by the calendar

Provision a new bucket, ask the owner to run a fresh backup from scratch when
they next have decent upload bandwidth, verify it, then delete the old bucket
in the console.

"Once a year" is the current practice and a reasonable default, but a poor
rule on its own. A machine holding
documents that barely change can go years before its repository is meaningfully
larger than the data in it; a machine with a large mailbox or a VM image
rewritten daily can double in weeks. Asking both owners for three days of
upload bandwidth on the same schedule is wrong in both directions.

Eumaeus already collects the number that decides it (§5.6). **Rotate when the
repository exceeds roughly twice its restore size**, checked monthly, with a
year as a backstop for machines that never trip it. That also means the ask
comes with its own justification attached: *"your backup is 340 GB and your
files are 150 GB — a fresh start would save 190 GB, and needs about two days of
uploading."*

**Be clear about what this buys, because it is not mainly space.**

A fresh repository ends up holding the same thing that `restic forget --prune`
would leave behind: the current data, deduplicated. Restic already stores an
unchanged file once however many snapshots reference it. What makes a
repository larger than the data it protects is *retained history* — snapshots
holding blobs that no longer exist on the machine — and `prune` reclaims
exactly that, in place, without re-uploading anything.

So rotation does not compress the data. **It discards the history**, and the
space saved equals the history thrown away. If that is what is wanted, `forget`
does it in minutes rather than in three days of somebody's upstream bandwidth.

What rotation genuinely buys, and these are real:

1. **A new repository password every six months.** A password that has been on
   a laptop, on a printed sheet and in an escrow for five years has had five
   years of opportunities to leak. This is the strongest argument for the
   cycle, and it has nothing to do with size.
2. **Proof that every source file is still readable.** A fresh full backup
   reads everything. An incremental one only reads what changed, so a file that
   became unreadable in 2024 is quietly absent from every snapshot since.
3. **A clean baseline** — no accumulated repository cruft, and no dependence on
   `prune`, historically the operation most likely to go wrong.
4. **It exercises the recovery path.** A new sheet is printed and filed. A
   recovery procedure nobody performs is a recovery procedure that does not
   work.

Framed that way, the six-month cycle is a *key rotation and verification*
exercise that happens to reclaim space, and it should be scheduled as one.

### 5.6 Making it the owner's idea

Rotation costs the owner two days of upload and every snapshot they have. It
saves the organisation money. Anyone being asked to trade the first for the
second should see both numbers in the same sentence, and should be able to say
no — which is also the only version of this that works in practice. An
organisation that imposes a rotation day finds machines switched off on it.

So the suggestion appears on the machine's own status page, and it does
nothing but suggest.

#### Why the machine computes it and the server only permits it

The figures come from `restic stats` against the repository, and the machine is
the only party already holding credentials to read it. Both calls are
read-only, so measuring never needs anything that could delete.

```
restic stats --json --mode raw-data          # everything stored: the bill
restic stats --json --mode raw-data latest   # what a fresh repo would start at
```

The difference is what rotation reclaims — and it is *exactly* the history
rotation discards. There is no third figure to compare against, because there
is no prune (§5.4).

The server contributes two things it alone knows: whether the organisation is
willing to provision another bucket (`fresh_bucket_available`), and what
storage costs. The machine does the arithmetic and decides whether the subject
is worth raising at all.

#### The three thresholds, and why one alone is not enough

| Condition | Alone it would |
|---|---|
| repository older than 90 days | nag the owner of a machine whose files never change, where history is nearly free |
| ≥ 30% superseded data | fire in week two, when a few large deletions briefly make the ratio look dreadful |
| ≥ 5 GiB reclaimable | mention savings not worth two days of somebody's uplink |

Measured weekly, after a successful backup, because `restic stats` walks the
whole index and is not something to run nightly.

#### What the card says

> **A fresh start would free 190 GiB**
>
> This backup has been running for 200 days, and 56% of what it stores is old
> versions of files that have since changed or been deleted. Starting again in
> a new bucket would save about $1 a month, $16 a year.
>
> **Stored now** 340 GiB across 200 backups
> **Your files today** 150 GiB
> **Would need uploading** 150 GiB — best started on a Friday, on a connection
> you are not paying for by the gigabyte
> **Would be lost** the ability to recover a file as it was at any point in the
> last 200 days. Everything you have *now* is kept.
>
> Nothing happens unless you ask.

The last line is the design. A button posts a request; an administrator
provisions the bucket. Nothing on the machine can create one.

> **Quote money carefully or not at all.** Below a configured price the card
> talks in gigabytes only. Storage is billed in ways this program does not
> model — per-account minimums, minimum retention periods — and a figure
> presented as exact will eventually be wrong in a way that costs the next
> number its credibility.

#### There is no "full backup" to take instead

Worth recording, because it is the obvious alternative and it does not exist.

restic has no full/incremental distinction. Every `backup` walks the source,
chunks it, and uploads only blobs the repository does not already hold. There
is no `--full` flag; `--force` forces re-*reading* the source files, and the
chunks it produces still deduplicate against the existing index. A "full"
backup into an existing repository therefore uploads almost nothing and frees
**nothing** — no pack can be discarded, because the new snapshot references the
same blobs the old packs hold.

The only repository that contains just the current data is one that has never
held anything else. That is what a new bucket is, and it is why the old one is
deleted whole rather than cleaned up.

`TestMeasureAgainstRealRestic` pins this against the actual binary: two backups
of unchanged data leave zero reclaimable bytes.

### 5.7 The cutover, and its two guards

```
  active ──▶ cutting-over ──▶ (old bucket emptied and deleted) ──▶ active
             │
             ├── new bucket + three keys provisioned
             ├── new password generated and escrowed
             ├── owner asked to run a full backup on good bandwidth
             └── BOTH buckets exist; only the new one receives backups
```

1. **Eumaeus refuses to retire the old repository until the new one has at
   least one `verified` run**, and by default until it has seven days of them.
   A cutover deleted the same afternoon has proven that the new bucket accepts
   writes, not that it can be restored from.

2. **The bucket is emptied and deleted before the escrow entry is.** The other
   order leaves a bucket nobody can read, still billing, forever — with no way
   to check what was in it.

The old owner sheet is destroyed at the same time, and a new one printed. A
drawer with three superseded sheets in it is a drawer where nobody knows which
one is live.

## 6. Recovery: what survives Eumaeus

### 6.1 The circular dependency, named

Eumaeus holds every repository password. Eumaeus runs on a server. If that
server is backed up by this system, then **the password needed to restore
Eumaeus is inside Eumaeus.** Lose the server and the recovery path is gone
along with it.

This is not hypothetical — it is the default outcome of building the obvious
thing, and the sheet's `Where is repo pass backed up?` column exists because
the problem was already understood and being handled by hand.

### 6.2 The bootstrap secrets

The fix is to name the minimum set of secrets that must exist **outside every
system**, keep it as small as possible, and put it on paper.

Four things:

| # | Secret | Why it cannot be stored digitally |
|---|---|---|
| 1 | Eumaeus's **vault password** | Decrypts everything else. Already outside by design |
| 2 | The **Eumaeus server's own** repository password | Needed to restore the thing that holds all the others |
| 3 | The key that decrypts the **recovery export** (§6.3) | Otherwise the export is a second copy of the same problem |
| 4 | The **Wasabi account root** login | Eumaeus's provisioning key is reissuable *from* this, so this is what must survive. It is also the only credential that can act on a bucket after Eumaeus is compromised |

Everything else in the system is recoverable from those three. That is a
statement worth being able to make in one sentence, and worth testing once a
year by actually doing it.

> **The Eumaeus server's own machine is enrolled differently.** Its repository
> password is set by hand and is *not* generated into the escrow. The dashboard
> marks it `self-hosted` so that nobody later "fixes the inconsistency" and
> recreates the cycle.

### 6.3 Two offline copies, for two different readers

**The owner's restore card**, printed at enrollment and after every rotation,
kept by the owner. §6.4 — this is the one that changes the character of the
whole system.

**The admin's encrypted export**, run monthly, kept on a USB key off-site:
every active repository, one file, decryptable only with bootstrap secret #3.

They are not redundant copies of one thing; they have different readers and
different failure modes. The card lets one person recover their own machine
with no help from anybody. The export lets one person recover the whole fleet.
Paper survives ransomware and a locked cloud account; the export survives a
fire and stays current as machines are added.

### 6.4 The owner's restore card: they do not depend on us

The card carries everything needed to restore, and names no software of ours.

```
  ┌──────────────────────────────────────────────────────────────────┐
  │  RESTORING YOUR OWN FILES — office-laptop-1                      │
  │                                                                  │
  │  You do not need Eumaeus, the office network, or anybody's help. │
  │  These steps work from any computer, anywhere, forever.          │
  │                                                                  │
  │  1. Download restic (free, one file, no installer):              │
  │       https://github.com/restic/restic/releases                  │
  │     Any version 0.14 or newer will read this backup.             │
  │                                                                  │
  │  2. Open a terminal where you saved it, and set four values.     │
  │                                                                  │
  │     Windows (PowerShell):                                        │
  │       $env:RESTIC_REPOSITORY="s3:https://s3.<region>.wasabisys…" │
  │       $env:AWS_ACCESS_KEY_ID="<read-only key>"                   │
  │       $env:AWS_SECRET_ACCESS_KEY="<read-only secret>"            │
  │       $env:RESTIC_PASSWORD="<43 characters>"                     │
  │                                                                  │
  │     macOS / Linux:  the same four, with  export NAME="value"     │
  │                                                                  │
  │  3. See what is stored:      restic snapshots                    │
  │                                                                  │
  │  4. Get everything back:                                         │
  │       restic restore latest --target ./restored                  │
  │                                                                  │
  │     Or one folder:                                               │
  │       restic restore latest --target ./restored \                │
  │              --include "/Users/you/Documents"                    │
  │                                                                  │
  │  These credentials can READ your backup and nothing else. They   │
  │  cannot change or delete it, and they open no other computer's.  │
  │                                                                  │
  │  Keep this page somewhere safe — it is enough to read every file │
  │  on your computer.  Destroy it when you receive a new one.       │
  │                                                                  │
  │  Issued 2026-09-09 · replaced at the next bucket change (~6 mo)  │
  └──────────────────────────────────────────────────────────────────┘
```

**Why this is worth more than it costs.**

The obvious way to build a restore feature is a button in Eumaeus. It would be
easier for the user and it would make them dependent on us: on the server being
up, on the admin being reachable, on the organisation still running this
software in ten years. A backup that can only be read through one small
organisation's web application is a backup with a single point of failure that
is not the storage provider.

The card removes that. What the software provides is convenience — scheduling,
verification, alerting, someone noticing when it stops — and none of that is
load-bearing at the moment of restore. The data is in a standard restic
repository, readable by the stock binary, and the person whose files they are
holds the credentials to read it.

That is also the honest answer to somebody who is uneasy about their employer
holding their backups. They can verify the claim themselves, today, with a
free download and four environment variables.

**Consequences of the card being complete:**

- The read-only restore key (§5.1) exists *because* of this card. Printing a
  key that can delete objects would mean a lost page could destroy the backup;
  a read-only one means a lost page exposes the owner's own data and nothing
  else.
- The card must be reissued on rotation, and the old one destroyed. Eumaeus
  tracks `card_issued_at` per repository.
- `restic snapshots` on the card is deliberate: step 3 proves the credentials
  work *before* anybody needs them. Enrollment should have the owner run it
  once, standing there, so the card has been tested rather than filed on trust.

### 6.5 Eumaeus tracks the copies without holding them

Two derived fields per repository — together, the successor to the sheet's
`Where is repo pass backed up?`, which was its most valuable column and its
most laborious.

| Field | States | Meaning |
|---|---|---|
| `owner_card` | `never` / `issued` / `superseded` | Has this repository's card been printed and handed over? `superseded` means the bucket rotated and the new card has not been issued |
| `admin_export` | `never` / `current` / `stale` | Is this repository's password in an export made since the password was set? `stale` after 90 days |

Neither field holds a credential. They record only that a copy was made, and
when.

A machine is genuinely safe only when both are green. Backups that run
perfectly to a bucket whose password exists in exactly one database is a
machine one server failure away from having no backups at all — and there was
previously no way to see that at a glance.

The dashboard should carry the fleet-level version, because that is the
sentence that gets somebody to open the safe:

> **3 machines have no offline copy of their password. 2 owner cards are
> superseded.**

### 6.6 Restore requests

Most owners can now restore themselves from the card (§6.4), which is the
point. The request exists for the person who would rather not: a button on
their machine's page in Eumaeus, and a row the admin sees.

| Field | |
|---|---|
| `machine_id`, `requested_by`, `requested_at` | |
| `what` | Free text — "the Contracts folder, as it was in June" |
| `state` | `open` / `done` / `declined` |

Deliberately thin. Restores are rare, high-stakes and supervised, and this is a
work item, not a workflow engine.

---

## 7. The sheet, column by column

Nothing is dropped without saying so.

| Sheet column | New home |
|---|---|
| `Dataset` | `machine.node_id` |
| `Last result` | latest `run.outcome` |
| `Hours since backup` | derived from `machine.last_verified_at` |
| `Bucket` | `repository.provider` + `region` |
| `Bucket name` | `repository.bucket` |
| `Script version` | `machine.agent_version`, reported every run |
| `Where is repo pass backed up?` | `repository.owner_card` + `admin_export` — §6.5 |
| `Backup local user account` | `machine.local_account` |
| `Last key rotation` | `repository.keys_rotated_at` |
| `Last bucket rotation` | `repository.retired_at` on the retired row |
| `Last IP` | `machine.last_seen_ip`, **server-observed** |
| `Last successful backup` | `machine.last_verified_at` — now means *verified*, not merely exited zero |
| `Hours since last start` | derived from `machine.last_started_at` |
| `Last start` | `machine.last_started_at` |
| `Last fail` | `machine.last_failed_at` |
| `Warn after hours` | `machine.warn_after_hours`, default 240 |
| `Warning email` | `person.email`, via `machine.owner_id` |
| `Last warning sent` | `alert.last_warning_sent_at` |
| `Has recovery pending?` | `alert.state` = `overdue` — §4 |
| `start node` | **gone** |
| `success node` | **gone** |
| `failure node` | **gone** |

The last three were three separate ping identifiers per machine —
`<node>-backup-start`, `-success`, `-failure` — because the only signal a shell
script could send was "this URL was hit". A structured event carries the phase
and the outcome as fields, so one machine identity replaces three, and the
outcome can be `incomplete` rather than being rounded to success or failure.

### What the sheet could not do, that this can

- Tell a verified backup from one that merely exited zero.
- Say *which* files are missing from a snapshot.
- Show more than the last run, so "fails every fourth night" becomes visible.
- Notice that a machine's password exists in only one place.
- Say who owns a machine, rather than which address to mail.
- Send the RECOVERED email without somebody watching for it.

---

## 8. Still open

Answered since the last revision: the owner's card carries the S3 keys as well
as the password (a read-only pair, §5.1), and the restore path is the stock
restic binary rather than anything of ours (§9).

1. **What happens to a Person who leaves?** Their machines transfer; their
   history must not be deleted. `active = false` plus a reassignment, probably
   — but their old restore cards are still valid credentials, so leaving should
   trigger a rotation of the machines they held.
2. **Does the admin digest go out even when everything is fine?** A silent
   system is indistinguishable from a broken one. A weekly "30 machines, all
   verified" is cheap and proves the alerting is alive. I would send it.
3. **How is the monthly export actually moved off the server?** Eumaeus can
   produce the file on a schedule; somebody must carry it to the USB key. That
   step is the one that will be forgotten, and `admin_export = stale` is the
   only thing that will notice.
4. **Decided: there is no pruning** (§5.4), on measured evidence — a prune took
   over 24 hours on a gigabit uplink. This removed the prune key, the scheduled
   Eumaeus job, the retention policy, and the snapshot-injection attack that
   count-based retention would have enabled.
5. **Decided: rotation is the reclamation mechanism**, roughly yearly, or when
   the repository passes twice its restore size (§5.5).
6. **What happens when Eumaeus finds a snapshot it has no Run for?** §5.4 makes
   this the *only* remaining detection of a compromised machine key, since
   nothing can be deleted any more — an attacker's options are reduced to
   adding junk. Worth building, and it needs a way to be acknowledged rather
   than firing repeatedly.
7. **Who deletes the retired bucket, and how is it not forgotten?** Eumaeus
   cannot delete buckets, so retirement ends in a human step in the Wasabi
   console. That belongs on the admin's dashboard as a work item — *"the old
   bucket for office-laptop-1 has been superseded for 30 days, 340 GB, safe to
   delete"* — or it will simply be paid for forever.
8. **Does Wasabi support IAM permissions boundaries?** Not documented (§5.2).
   If it does, the provisioning key can be genuinely constrained, which is the
   difference between a recovery window and actual prevention.
9. **Is 30 days the right lifecycle expiry for noncurrent versions?** It is the
   window in which a mistaken or malicious delete can still be undone, and it
   is also how long reclaimed space stays billed.
10. **What does Eumaeus do when a bucket's provisioning fails halfway?** A
   bucket created with no keys, or keys with no escrow entry, is a mess that
   needs either a rollback or a repair command. The Wasabi token cannot delete
   buckets, so rollback is necessarily partial — worth deciding before it
   happens rather than during.
11. **Is `warn_after_hours` per machine, per person, or fleet-wide with
   overrides?** The sheet was per machine. Per machine with a fleet default
   seems obvious; worth confirming nobody wants "all of Fr. N's machines are
   urgent".

## 9. What this settles in the API draft

- **D6 (token scoping)** — answered. Tokens are issued per machine at
  enrollment and bound to it. No shared token exists.
- **D4 (`repository` as a key)** — answered. The server owns the URL and hands
  it to the client at enrollment; the client echoes it verbatim.
- **D5 (overdue detection)** — answered differently and better. The server
  owns `warn_after_hours` and the alert state; the client does not send
  `next_run_at` and does not need to.
- **New**: the enrollment claim endpoint, the offline-copy state, and the
  rotation guards are all absent from the current draft.

`eumaeus-api.md` should be rewritten against this model once §8 is settled,
rather than patched.

### The client keeps shelling out to restic

Worth recording here because §6.4 is the reason, not just the implementation.

restic cannot be used as a Go library. Every package in the module is under
`internal/`, which Go's own module rules make unimportable from outside it —
the only non-internal files are `doc.go` and `build.go`, and `cmd/restic` is
`package main`. Using it as a library means forking it and carrying that fork.

Even if it were importable, the owner's restore card settles it. The card says
"download restic and run these four commands", and that promise is only true if
the repository was written by the same stock restic the owner downloads.
Embedding a fork would make our repositories the output of our build, and
compatibility with the public binary would become something to hope for.

So: ship the official binary beside ours, pin its version, verify its checksum
on install, and record in each run which version wrote the snapshot.
