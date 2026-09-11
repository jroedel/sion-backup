# Eumaeus backup API — specification

**Status: for implementation.** Written against the settled fleet model in
[`model.md`](model.md); read that first for *why* any of this is shaped the way
it is. The machine-readable form is [`openapi.yaml`](openapi.yaml).

Audience: whoever implements these endpoints in
[eumaeus](https://github.com/jroedel/eumaeus).

| | |
|---|---|
| Client | `sion-backup`, one per work computer |
| Endpoints | 6 |
| Base path | `/api/backup/v1` |
| Auth | an enrollment code, once; a machine token thereafter |
| Open questions | 4, listed at the end. None blocks implementation |

---

## 1. What changed since the first draft

The earlier draft assumed the client generated its own repository password and
pushed it to an escrow, authenticated by a token pasted into a config file.
None of that survives.

| Then | Now | Why |
|---|---|---|
| Token pasted into `config.toml` | Enrollment code exchanged for a machine token | A long-lived secret in a text file, with no idea whose machine it was on |
| One token could read any machine's escrow | Token bound to one machine | One stolen laptop yielded the whole fleet |
| Client generated the repository password | **Eumaeus generates it** | Eumaeus provisions the bucket and its keys, so it is already the party that knows them |
| Client PUT credentials to the escrow | Client only ever reads them | There is nothing for the client to tell the server |
| Retention policy in the payload | **Gone** | Nothing prunes ([`model.md`](model.md) §5.4) |
| Credentials cached in the platform keyring | **Fetched every run, never stored** | See below — it deleted 900 lines of client code and made revocation actually work |
| `run_id`, a per-machine autoincrement | `run_uuid` | The old one was `0` on every start event and repeated after a reimage |

The direction of every change is the same: **the server owns the facts, the
machine reports what it did.**

### Nothing is cached on the machine

The earlier design kept the credentials in the platform keyring so a laptop
could back up while offline. That capability was never real — a backup writes
to a bucket over the internet, so a machine that cannot reach Eumaeus almost
certainly cannot reach S3 either — and the price was three credentials sitting
on the most-likely-to-be-stolen computer in the fleet, one of which, the
repository password, cannot be rotated at all.

So `GET /machines/me/credentials` is now called at the start of **every** run.
What that buys:

- **Revocation works.** Revoke the machine token and the machine stops backing
  up immediately. Cutting off a laptop that held cached credentials meant
  rotating the S3 keys and rotating the whole bucket, because the password
  could not be changed.
- **Every use is audited.** A stolen laptop being used is a visible row on the
  server rather than an invisible local read.
- **The restic password never touches the client's disk.**
- It deleted 900 lines of the least testable code in the client — a wrapper
  around DPAPI, the macOS Keychain and the Secret Service, with a file fallback
  for the headless Linux case where none of them answers.

**Be precise about what it does not buy.** One secret remains on the machine:
the token itself, in a 0600 file. Something has to authenticate an unattended
daemon, and whoever takes that file can fetch what the machine can fetch. The
difference is revocability, auditability, and scope — the token authorises
"fetch my credentials", not "read the repository".

**The cost.** Eumaeus is now on the critical path of every backup in the fleet.
An outage means nothing backs up. That shows up as every machine going overdue
at once, which is at least a very legible failure — and with
`warn_after_hours` at 240, an outage of a day or two passes unnoticed.

## 2. Two credentials, and what each can do

### 2.1 The enrollment code

Created by an admin in the Eumaeus web UI, where they choose the owner, the
node ID and the bucket. Carried across the room and typed into
`sion-backup enroll --code K4TP-9QX2`.

| Property | Requirement |
|---|---|
| Format | 8 characters, Crockford base32, one hyphen for legibility: `K4TP-9QX2` |
| Lifetime | 15 minutes |
| Uses | Exactly one. A second claim gets `409` |
| Rate limit | **Required.** 8 base32 characters is 40 bits; unthrottled, that is guessable. No more than 10 attempts per minute per source, and lock the code after 5 failures against it |
| Scope | Creates one machine, for one owner, on one repository |

### 2.2 The machine token

Returned by the claim and written to a 0600 file in the machine's data
directory. It is the only secret the client keeps.

```
Authorization: Bearer <machine token>
```

It is bound to one machine and can do exactly four things: read that machine's
state, read that machine's credentials, post that machine's run events, and
record that machine's owner card was printed. It cannot read another machine,
cannot enrol, cannot write configuration, and cannot reach any other part of
Eumaeus.

**Revocation must be immediate**, and revoking it is how a lost laptop is cut
off. The client treats `401` as terminal: it stops trying, and the status page
says the machine has been de-enrolled rather than showing a network error
forever. A `403` is *not* that — see §3.3.

> Revocation is now sufficient on its own. Because the machine caches no
> credentials, cutting off a stolen laptop is one click: revoke the token and
> its next backup cannot obtain a password. Under the previous design that
> click stopped it *reporting* while it carried on writing, and cutting it off
> properly meant rotating the bucket's keys.
>
> What revocation does not undo is the owner's printed restore card, which
> carries read-only credentials by design ([`model.md`](model.md) §6.4). A card
> that has gone astray is a reason to rotate the bucket.

## 3. Conventions

### 3.1 Base path and versioning

```
/api/backup/v1
```

Versioned from the start. Adding `/v1` costs nothing today and cannot be done
cleanly once binaries are in the field.

### 3.2 Times

RFC 3339 with an offset, as Go's `encoding/json` emits: fractional seconds
present and of variable length, offsets local to the machine rather than UTC.
A fleet in two time zones reports in each one's own; **store what you are given
and convert for display.**

Every timestamp in a request is client-supplied and therefore untrusted. The
server records its own `received_at` alongside, and all alerting logic uses
`received_at` — a machine with a wrong clock must not be able to look healthy.

### 3.3 Errors

```json
{ "error": "that enrollment code has expired", "field": "code" }
```

`error` is one sentence for a human; it reaches a log file on a work computer
and the output of `sion-backup doctor`, so it must be useful and must contain
nothing sensitive. `field` is optional.

The client distinguishes only a handful of outcomes, so the choice of status
matters:

| Status | Client behaviour |
|---|---|
| `2xx` | Success. Response body discarded unless the endpoint returns data |
| `400` | The `error` sentence and `field` are decoded out and shown as they stand — see below |
| `422` | The same, and told apart from `400`: the request was fine and the situation is not |
| `404` | "No such thing" — an ordinary answer, not a failure |
| `409` | Only meaningful on the claim: the code has already been used |
| `401` | Terminal. Never retried. Surfaced to the user as de-enrolment |
| `403` | Refused, but not de-enrolment. What it means is the endpoint's business |
| anything else `>= 300` | Generic failure: logged with the body included, retried later |

Everything outside that list is one error to the client: a `429` and a `500`
are indistinguishable. If you need it to behave differently, the case has to
map onto one of the seven.

**`400` and `422` are two answers.** A `400` is this client sending nonsense:
nobody standing at the machine can help, and the answer is to stop and make
enough noise that somebody fixes the client. A `422` is the server saying the
request was well-formed and something on its own side has to change first — the
machine's bucket is not provisioned, or the code enrols it against a repository
it has never written to (§4.2) — and the person standing at the machine is
exactly who can fix that, usually without the code expiring. Both carry the
sentence; only one is worth apologising for.

**`401` and `403` are two answers, not one.** They were one until 2026-09-10,
and the bug that reading hides is quiet: `403` is how §8 says a fleet has
fresh buckets switched off, which is the ordinary state of a fleet where
nobody has turned them on, and a client that read it as `401` would tell an
owner their machine had been de-enrolled because an administrator had never
changed a setting. Where the two genuinely coincide — §6, where a token that
may not read its own credentials is finished either way — it is the endpoint's
own code that puts them back together, and says so.

**The `error` sentence reaches a person.** On a refused claim it is printed to
whoever is standing at the machine, so the client decodes `error` and `field`
and shows that and nothing else: no JSON, no status, and no package name from
either side. Anything unparseable is kept verbatim instead, because a proxy's
error page is still better than an empty line.

### 3.4 Limits

| | |
|---|---|
| Request body | reject above 64 KiB |
| Response body | the client reads at most 1 MiB |
| Client timeout | 30 s per call |
| Unknown request fields | **must be ignored**, so a newer client can add one without a coordinated deployment |

### 3.5 Rate limiting

A machine makes one poll an hour, one credentials read per rotation, and two
run events per backup. Thirty machines is a trivial load. If you limit, return
`429` — but see §3.3: the client cannot see it specifically and will treat it
as a generic failure and retry the whole flush later. `Retry-After` is ignored.

---

## 4. `POST /enrollments/claim`

Exchanges a one-time code for a machine token and everything the machine needs
to start backing up. **The only unauthenticated endpoint.**

### Request

```json
{
  "code": "K4TP-9QX2",
  "hostname": "DESKTOP-4KJ2P1",
  "os": "windows/amd64",
  "local_account": "CORP\\jdoe",
  "agent": "v1.2.0",
  "legacy": {
    "repository_url": "s3:https://s3.us-central-1.wasabisys.com/bucket123",
    "snapshots": 1412,
    "oldest_snapshot": "2019-03-01T22:04:00Z"
  }
}
```

| Field | Req | Notes |
|---|---|---|
| `code` | yes | As issued. Compare case-insensitively and ignore hyphens |
| `hostname` | yes | The machine's own name. May differ from `node_id` and may change later |
| `os` | yes | `GOOS/GOARCH` |
| `local_account` | yes | **The OS account the daemon runs as.** Not trivia: DPAPI and Keychain bind the cached credentials to it, so a machine whose account changes can no longer read its own credentials, and the dashboard needs to be able to show why |
| `agent` | yes | Client build |
| `legacy` | no | The old backup this machine is being migrated off, as the machine sees it. Sent only by `adopt-enroll`, and only about an install it actually found. See §4.2 |

### Response `200`

```json
{
  "machine_token": "mt_7f3c…",
  "node_id": "office-laptop-1",
  "owner": { "name": "Fr. N", "email": "n@example.org" },
  "warn_after_hours": 240,
  "repository": {
    "url": "s3:https://s3.us-central-1.wasabisys.com/example-node-bucket",
    "provider": "wasabi",
    "region": "us-central-1",
    "bucket": "example-node-bucket",
    "created_at": "2026-09-09T10:04:00+02:00",
    "adopted": true,
    "snapshots": 1412
  },
  "credentials_version": 1,
  "credentials": {
    "restic_password": "kQ8v…",
    "machine": { "access_key_id": "…", "secret_access_key": "…" },
    "restore": { "access_key_id": "…", "secret_access_key": "…" }
  }
}
```

The `machine` key writes and may delete only under `locks/`. The `restore` key
is read-only and is what gets printed on the owner's card
([`model.md`](model.md) §6.4) — the client needs both.

### 4.1 An adopted repository

`adopted` and `snapshots` describe a bucket taken over from a legacy install
rather than provisioned empty — [eumaeus#112](https://github.com/jroedel/eumaeus/issues/112),
`eumaeus backup adopt`. The same two fields appear on `repository` in §5, so a
machine adopted before they shipped picks them up on its next poll.

| Field | Notes |
|---|---|
| `adopted` | `omitempty`. **Absence means "ask restic", not "there is nothing there"** |
| `snapshots` | `omitempty`. What restic reported at adoption, recorded by hand with `-snapshots`. The repository remains the authority on what it contains |
| `created_at` | On an adopted repository this is the **history horizon** — 2019, say — and not when the row in Eumaeus was made. It is what "backups available since …" should read |

The client reads all three in `sion-backup adopt-enroll`, which claims the code
and then checks that the repository it was handed is the legacy machine's own.
That check is the point: `provision` typed where `adopt` was meant returns a
perfectly good empty bucket, and every step after it succeeds. So the client
treats a missing `adopted` as no answer rather than as a denial, and asks
restic for the snapshot count either way.

### 4.2 `legacy` — what the machine already has

Optional, and sent only by `sion-backup adopt-enroll`, about an install it
actually found. A claim without it takes exactly the path it took before the
block existed, which is every machine in the field today.
[eumaeus#123](https://github.com/jroedel/eumaeus/issues/123).

| Field | Notes |
|---|---|
| `repository_url` | Where the legacy script writes, in restic's syntax. **Omitted rather than guessed at**: a client that found an install and could not open its repository says nothing, because "I do not know" must not read as "somewhere else" |
| `snapshots` | What restic reported when the client opened it |
| `oldest_snapshot` | Where its history starts. A value in the future is dropped by the server rather than refused — a laptop with a wrong clock must still be able to enrol |

The server does exactly two things with it.

**It refuses a claim whose `repository_url` is not the repository the code
enrols this machine against** — `422`, `field: legacy.repository_url`, and a
sentence naming both. **The code is not consumed.** Adopt the bucket the
machine is writing to and present the same code again.

That refusal is the point of the block. `provision` typed where `adopt` was
meant returns a working, empty bucket: the claim succeeds, the first backup
succeeds, the dashboard goes green, and years of snapshots sit in a bucket
nothing points at. Both buckets are real, so the machine that has been writing
to one of them nightly is the only thing that can tell the difference — and the
claim is the last moment at which saying so is free. The client checks after
the fact too, in `adopt.go`, because a server that has not been told cannot
answer; but by then the code is spent and the recovery is a second one.

The comparison ignores surrounding space and trailing slashes and is otherwise
exact. Case is significant, because on a provider where `Bucket123` and
`bucket123` are two buckets, folding it would accept a claim against the wrong
one.

**It fills in the history of a bucket that was adopted without one**, from
`snapshots` and `oldest_snapshot`. It can only widen what is known: a figure an
administrator typed at `adopt` is never replaced, and a provisioned repository
takes neither.

`422` rather than `409` for the refusal, which is Eumaeus's decision and a good
one: `409` on this endpoint already means "that code has been used, ask for
another", which is the opposite of what has happened and would send whoever is
standing at the machine for the one thing that cannot help.

### Responses

| Status | When |
|---|---|
| `200` | Claimed |
| `404` | No such code, or it has expired |
| `409` | Already claimed. **Do not re-issue the token** — see below |
| `422` | Two causes. The code is valid and the machine it names has no repository provisioned yet — or `legacy.repository_url` is not the repository that code enrols it against (§4.2), in which case **the code is not consumed** |
| `429` | Rate limited |

### Server rules

1. **The claim is atomic and single-use.** Mark the code consumed in the same
   transaction that creates the machine and mints the token. Two concurrent
   claims must produce one machine.
2. **The token is shown once.** Store a hash; a token the server can redisplay
   is a token a copy of the database yields.
3. **A repeated claim gets `409`, never a fresh token.** A retry after a
   dropped response is indistinguishable from an attacker replaying a code, so
   the safe answer is refusal — the admin issues a new code, which is cheap and
   they are standing right there.
4. **Record `hostname`, `os`, `local_account`, `agent`** on the machine, and
   the source IP as `last_seen_ip`.

---

## 5. `GET /machines/me`

The machine's own state, with no secrets in it. **This is also the heartbeat**
— it is what populates `last_seen_ip` and proves a paused machine is still
alive.

Polled hourly and at startup.

### Response `200`

```json
{
  "node_id": "office-laptop-1",
  "owner": { "name": "Fr. N", "email": "n@example.org" },
  "state": "active",
  "paused_until": null,
  "warn_after_hours": 240,
  "repository": {
    "url": "s3:https://s3.us-central-1.wasabisys.com/example-node-bucket",
    "state": "active",
    "created_at": "2026-03-14T09:00:00+01:00"
  },
  "credentials_version": 3,
  "card": { "state": "issued", "issued_at": "2026-03-14T09:12:00+01:00" },
  "fresh_bucket_available": true,
  "storage_price_per_tib_month": 6.99,
  "server_time": "2026-09-09T21:40:11+02:00"
}
```

| Field | Notes |
|---|---|
| `state` | `active` or `retired`. A retired machine's client should stop backing up and say so |
| `repository.state` | `active`, `cutting-over`, `retired`. During a cutover the URL is already the **new** one |
| `credentials_version` | Increments whenever any credential changes. Informational now that the client fetches per run — it is what the audit log records, and what a support call can compare against |
| `repository.created_at` | The history horizon. The client shows "backups available since …" from this |
| `fresh_bucket_available` | Whether the organisation is willing to provision a new bucket for this machine. **A permission, not a recommendation** — see §5.1 |
| `storage_price_per_tib_month` | What storage costs, so the machine can turn reclaimable bytes into money. Omitted or `0` means the status page talks in gigabytes only |
| `server_time` | So the client can detect its own clock being wrong, which would otherwise corrupt snapshot timestamps and the alerting silently |

### 5.1 Who decides to rotate, and why it is split in two

The server says whether a fresh bucket is *allowed*. The machine decides
whether it is *worth it*, and only the machine can: the figures come from
`restic stats` against the repository, and the machine is the thing already
holding credentials to read it.

So `fresh_bucket_available` is a permission. The client combines it with its
own weekly measurement and raises the subject only when all three of these
hold:

| Condition | Why on its own it is not enough |
|---|---|
| repository older than 90 days | a machine whose files never change has cheap history and should not be nagged |
| ≥ 30% of it is superseded data | a few large deletions make a young repository look terrible for a fortnight |
| ≥ 5 GiB reclaimable | below that it is not worth two days of somebody's uplink |

The card then shows both halves of the trade in the same breath — what would be
saved, and that everything older than today would be discarded — and does
nothing. **Rotation is always the owner's idea**, which is the only version of
this that works: the person who pays the bandwidth is the person who has to
agree, and an organisation that imposes it will find machines mysteriously
switched off on rotation day.

Support `ETag`/`If-None-Match` and answer `304`. An hourly poll from thirty
machines that changes twice a year should not be thirty full responses an hour.

Because this contains no secrets, it may be logged in full — unlike §6.

| Status | When |
|---|---|
| `200` / `304` | |
| `401` | Token revoked or the machine retired. Terminal for the client |

---

## 6. `GET /machines/me/credentials`

The secrets. Separate from §5 precisely so that the frequent call carries none
and this one can be audited.

**Called at the start of every backup**, because the client stores nothing —
see §1. That makes this the busiest secret-bearing endpoint in the system
(roughly one call per machine per day) and the reason its audit log is worth
having: it is the only place a stolen machine token shows up.

### Response `200`

```json
{
  "credentials_version": 3,
  "repository": { "url": "s3:https://…/example-node-bucket" },
  "credentials": {
    "restic_password": "kQ8v…",
    "machine": { "access_key_id": "…", "secret_access_key": "…" },
    "restore": { "access_key_id": "…", "secret_access_key": "…" }
  }
}
```

`repository.url` is repeated here deliberately: fetching credentials and
learning which repository they open must be one atomic answer, or a client that
polled across a rotation could pair the new password with the old bucket.

### Server rules

1. **Audit every call**: which machine, when, from what address, which
   `credentials_version`. Not the values. This is the only telemetry that would
   reveal a stolen token being used — and since a legitimate machine calls
   about once a day, a burst or a call from an unexpected address is a signal
   worth acting on.
2. **Never log the body**, here or in any proxy in front of it.
3. **Return the credentials for the repository the machine should be using
   now.** During a cutover that is the new one.
4. A `restic_password` must never be empty in a `200`. If the record is
   incomplete, return `500` with an explanatory `error` — a partial answer
   would make a client believe it is enrolled against a repository it cannot
   open, and the failure would surface as an unverified backup at 1am.

---

## 7. `POST /runs`

One event about one backup. Two phases, one endpoint.

### Request

```json
{
  "run_uuid": "0192f3a1-7c4e-7b21-9f10-3c2d5e8a41b7",
  "phase": "finished",
  "started_at": "2026-09-09T13:04:22.117+02:00",
  "finished_at": "2026-09-09T13:19:48.882+02:00",
  "outcome": "success",
  "message": "4211 files, 88.0 MiB added",
  "snapshot_id": "a1b2c3d4",
  "files_processed": 4211,
  "bytes_processed": 9928374651,
  "data_added": 92274688,
  "unreadable_files": 0,
  "verified": true,
  "agent": "v1.2.0",
  "os": "windows/amd64"
}
```

The machine is identified by the token, not by a field in the body. A client
cannot post about another machine because it has no way to name one.

| Field | `started` | `finished` | Notes |
|---|---|---|---|
| `run_uuid` | yes | yes | **UUIDv7**, generated by the client before the run begins. Same value on both phases. Sorts by time, so the dashboard can order a machine's runs without trusting its clock |
| `phase` | `started` | `finished` | |
| `started_at` | yes | yes | Client clock |
| `finished_at` | — | yes | |
| `outcome` | — | yes | Five values, below |
| `message` | — | usually | One sentence for a human. Never contains a credential |
| `snapshot_id` | — | if one was written | **Present on `incomplete` too** |
| `files_processed`, `bytes_processed`, `data_added` | — | if non-zero | Omitted when zero — treat absent as `0`, not unknown |
| `unreadable_files` | — | if non-zero | **A count, not a list.** The paths are on the machine's own status page; filenames are identifying, and this crosses to a server whose logs the client does not control |
| `seeding` | yes | yes | The first backup into a new repository. **Not "full vs incremental" — see below** |
| `repository_url` | yes | yes | Which bucket this run wrote to. Lets the server confirm a machine went where it was sent, which matters during a cutover |
| `verified` | `false` | yes | Always present |
| `agent`, `os` | yes | yes | |

#### There is no full/incremental distinction to report

It is tempting to send one, and restic does not have one to send. Every
`restic backup` is the same operation: walk the source, chunk it, upload only
the blobs the repository does not already hold. There is no `--full` flag, and
`--force` forces re-*reading* the source files rather than re-uploading them —
the chunks it produces still deduplicate against the existing index, so a
"full" backup into an existing repository uploads almost nothing and frees
nothing.

`TestMeasureAgainstRealRestic` in the client pins this down against the real
binary: two backups of unchanged data leave zero reclaimable bytes.

What is genuinely worth distinguishing is the **seeding** run — the first
backup into a newly provisioned repository, which uploads everything, takes
hours or days, and is the one the cutover guard waits on. That is what
`seeding` carries.

### `outcome`

| Value | Meaning | Resets the staleness clock | Dashboard |
|---|---|---|---|
| `success` | Complete snapshot, verified by restoring from it | yes | green |
| `degraded` | Verified, but taken without a filesystem snapshot; open files may be missing | yes | amber |
| `incomplete` | A real, restorable snapshot with named files missing | yes | amber |
| `unverified` | restic wrote a snapshot and nothing came back out of it | **no** | **red, and email the admin at once** |
| `failed` | No snapshot | no | red |

`degraded` and `incomplete` reset the clock because a restorable snapshot
exists; treating them as "not backing up" would put a permanent alarm on every
Windows machine whose user is not an administrator, and an alarm that is always
on is an alarm nobody reads.

`unverified` is the loudest, louder than `failed`. A failure is visible and
somebody will fix it; `unverified` means the machine believes it is protected
and may not be. See [`model.md`](model.md) §3.

**Treat an unrecognised `outcome` as red rather than rejecting the event.** A
future client adding a sixth value must not blank a dashboard.

### Server rules

1. **Idempotent on `(machine, run_uuid, phase)`.** The client marks a run
   reported only after the POST succeeds, so an acknowledgement lost in transit
   produces a duplicate. Accept it and return `200`.
2. **Never reject an event for being old.** A laptop that backed up on a plane
   reports when the lid opens in the office. Days late is normal.
3. **A `started` with no `finished` is a finding, not a data error** — it is a
   machine that died mid-backup, and it is precisely what the start event
   exists to make visible.
4. **A seeding run may take days.** Do not treat a long-running `started` on a
   `seeding` run as a failure, and show it as progress rather than as an alarm.
   It is also the run the cutover guard is waiting for: the old bucket is
   retired only after a `verified` seeding run plus seven days.
5. **A `snapshot_id` you have no record of creating is an alarm.** Nothing
   legitimate produces one. Since nothing in this system can delete backup data
   ([`model.md`](model.md) §5.4), adding junk is the *only* thing a compromised
   machine key can do — so this is the detection that matters.
6. Update `last_seen_ip` from the connection.

| Status | When |
|---|---|
| `200`, `204` | Recorded |
| `400` | Malformed. **The client marks the run reported and logs loudly** — a request the server calls malformed will not become well-formed by being resent. This is why `400` may never mean "the server could not store it": see [jroedel/eumaeus#115](https://github.com/jroedel/eumaeus/issues/115) |
| `401` | Terminal |

---

## 8. `POST /machines/me/rotation-request`

The owner has read the card in §5.1 and said yes. This is how they say it.

### Request

```json
{
  "repository_url": "s3:https://…/example-node-bucket",
  "reclaimable_bytes": 204010946560,
  "fresh_bytes": 161061273600,
  "measured_at": "2026-09-09T13:20:00+02:00"
}
```

The figures come along so the admin sees what the owner was shown, and so a
request made against numbers that have since changed can be recognised as
stale. The server does not have to trust them; it can measure again.

Returns `202` with no body. The request is a work item, not a command:
Eumaeus queues it for an administrator, who provisions the new bucket. Nothing
on the machine can create a bucket, and nothing should.

| Status | When |
|---|---|
| `202` | Queued |
| `409` | A request is already open for this machine, or a rotation is already in progress |
| `403` | Fresh buckets are not on offer. The setting is **fleet-wide**, not per machine, so this is an ordinary answer and not a fault of this machine's |

> **Why a request and not an automatic provision.** Eumaeus holds the Wasabi
> key and could create the bucket on the spot. It should not: provisioning
> starts a bill, and it starts a cutover in which two buckets exist and one is
> eventually deleted. A person should be in that loop, and the loop runs once a
> year per machine.

## 9. `POST /machines/me/card-issued`

Records that the owner's restore card has been printed, which drives the
`owner_card` state on the dashboard ([`model.md`](model.md) §6.5).

### Request

```json
{
  "repository_url": "s3:https://…/example-node-bucket",
  "printed_at": "2026-09-09T13:22:00+02:00"
}
```

`repository_url` so that a card printed for a superseded bucket cannot mark the
current one as covered.

Returns `204`.

> This records that the card was *rendered*, which is not the same as it being
> printed, handed over and filed in a safe. The dashboard should treat this as
> "the machine offered a card" and keep a separate, manual confirmation for
> "the card is in the safe" — the second is the one that matters and only a
> person can assert it.

---

## 10. Rules that carry weight

Not feature requests. These are the properties the client's design assumes, and
a server that breaks one makes the client unsafe.

1. **Never log a request or response body on §4 or §6.** They carry the keys to
   somebody's entire backup. Log method, path, machine, status.
2. **Audit every credential read** (§6). Who, when, from where. Never the value.
3. **Store credentials in the vault, not the control database.** They are the
   highest-value data Eumaeus would hold, and the vault is what Eumaeus already
   built for exactly this.
4. **Never delete a repository's password when a machine is de-enrolled.** It
   is the only way to read snapshots that already exist. Retiring a machine
   must not destroy its history; deletion is a separate deliberate act.
5. **Hand the client the repository URL verbatim and never rewrite it.** The
   client stores it in its local plan and shows it on the status page, and the
   restore card printed for the owner carries it verbatim. A server that
   reassembled the URL from provider/region/bucket would eventually reassemble
   it differently, and a card printed last year would stop working.
6. **`credentials_version` increments on any credential change**, including an
   S3 key rotation that leaves the password untouched. The client no longer
   depends on it to notice a rotation — it refetches anyway — but it is what
   makes the audit log answerable.
7. **A token that is unknown, malformed, revoked or expired is `401`.** Not
   `500`. §3.3 is not a style preference: `401` is the one status the client
   treats as terminal, and anything else is retried forever. A revoked laptop
   answered with `500` keeps calling every hour and its owner is told the
   server is having trouble, which is the opposite of what happened.

## 11. Client status

Done, in this repository:

| Change | Where |
|---|---|
| Nothing cached; credentials fetched per run | `business/domain/credential/credentialbus` |
| `foundation/secrets` deleted — DPAPI, Keychain, Secret Service, all of it | *gone, ~900 lines* |
| One secret on disk, in a 0600 file | `foundation/token` |
| `enroll --code`, claiming against this API | `cmd/sion-backup/enroll.go` |
| The owner's restore card, printed at enrollment | `cmd/sion-backup/enroll.go` |
| `401` surfaced as de-enrolment, not a network error | `credentialbus.Unauthorised` |
| `403` kept apart from it, so a fleet setting cannot read as de-enrolment | `eumaeusapi.ErrForbidden` |
| The server's own sentence shown to the installer, without a prefix | `eumaeusapi.BadRequest` |
| Weekly `restic stats` measurement, stored locally | `foundation/restic.Measure`, `planbus.Measurement` |
| The rotation card, with both halves of the trade | `statusapp`, `planbus.ConsiderRotation` |
| Every call under the versioned base path (§3.1) | `eumaeusapi.APIPrefix` |
| The fleet's server as the built-in default, so enrolling needs only a code | `cmd/sion-backup.DefaultEumaeusURL` |
| `run_uuid` on both phases, stored so a late report keeps its identity | `backupbus.Run`, `backupdb`, `cmd/.../events.go` |
| `seeding` and `repository_url` on run events | `backupbus.Runner.Seeding`, `fleetbus.Event` |
| A rejected event dropped rather than resent forever | `fleetbus.ErrRejected`, `fleetbus.Flush` |
| Install failures and panics, queued on disk and sent with or without a token | `business/domain/diag`, `eumaeusdiag` |
| `repository.adopted`, `repository.snapshots` and the history horizon on the claim (§4.1) | `eumaeuscreds.Enrollment` |
| The `legacy` block on the claim (§4.2), so a code pointing at the wrong bucket is refused before it is spent | `eumaeuscreds.LegacyInstall`, `cmd/sion-backup/adopt.go` |
| `422` carrying the server's own sentence, kept apart from `400` | `eumaeusapi.ErrUnprocessable`, `eumaeusapi.BadRequest` |
| Taking a legacy install over: the plan assembled from it, the Eumaeus commands printed, the adopted bucket checked afterwards | `cmd/sion-backup/adopt.go` |

Still to do:

| Change | Where | Size |
|---|---|---|
| The "ask for a fresh bucket" button, posting §8 | `statusapp` | small |
| Take `fresh_bucket_available` and the price from §5 rather than config | `statusapp`, daemon | small |
| Hourly poll of §5, for `paused_until` and retirement | new, in the daemon | medium |
| `POST /machines/me/card-issued` after printing | `cmd/sion-backup/enroll.go` | trivial |
| Show "backups available since" from `repository.created_at` | `statusapp` | trivial |
| `selfupdate.Source` against the `agent` block, replacing the GitHub fallback | `foundation/selfupdate` | medium |
| Honour `poll_after_seconds` once the poll exists | daemon | trivial |
| Take the seeding flag from `repository.adopted` rather than local history. **Live now, not hypothetical:** every machine `adopt-enroll` migrates has no local history, so its first run reports `seeding: true` against a repository holding years of snapshots, and §7's cutover guard waits on it | `backupbus`, `cmd/.../events.go` | small |

### 11.1 Live on the server, not yet specified above

Three things `terraboskamp.org` does that this document does not describe.
They are recorded here so the gap is visible rather than discovered; each links
to the issue where the server side is set out, and none of them is guesswork on
our part.

| What | Issue |
|---|---|
| `POST /diagnostics` — §5's install failures and panics, accepted with or without a machine token, `202` always, deduplicated on `(install_id, kind)` | [eumaeus#113](https://github.com/jroedel/eumaeus/issues/113) |
| An `agent` block on §5's response, naming the version, an optional `minimum`, and a per-platform URL and SHA-256. **Absent when nobody has decided**, which is not the same as an empty version | [eumaeus#114](https://github.com/jroedel/eumaeus/issues/114) |
| `poll_after_seconds` on §5's response. Omitted means no opinion; floored at 60 seconds and capped at a day | [eumaeus#117](https://github.com/jroedel/eumaeus/issues/117) |

Writing them up properly here is work this repository owes — the client
cannot consume any of them until it is done anyway. `repository.adopted` and
`repository.snapshots` were the fourth of these and are now written up, in
§4.1, because `adopt-enroll` consumes them.

`openapi.yaml` is a different matter: it is asked to move to eumaeus in
[eumaeus#121](https://github.com/jroedel/eumaeus/issues/121), because a
machine-readable spec maintained by the client has no way to notice the
server changing — these four are the proof. It stays here, and stays wrong
in all four ways, including §4.1 which this document now describes and that
file still does not, until that is answered; sion-backup#29 is the checklist
of what this repository deletes when it is.

## 12. Still open

1. **Does the client need to learn `paused_until` from the server?** Today
   pausing is local, set by the owner on their own machine. If an admin can
   also pause from Eumaeus, the two need reconciling — last-writer-wins is
   probably fine, but it should be decided rather than discovered.
2. **Should the restore-request button live on the machine's local status page
   as well as in Eumaeus?** It would need a sixth endpoint. The argument
   against is that a machine too broken to back up may also be too broken to
   ask for help, so the web UI is the more reliable place.
3. **How does a machine learn it has been retired?** §5 returns
   `state: retired`, but a retired machine may simply never poll again. This
   matters less than it did — there are no cached credentials to delete, and
   revoking the token stops the machine dead — but a client that knew it was
   retired could say so instead of reporting failures.
4. **Should the client tolerate a brief Eumaeus outage by retrying within a
   run?** A backup that fails because the server was restarting is noise. A few
   minutes of retry with backoff would absorb it; more than that just delays
   the honest failure.

## 13. Out of scope

- **Remote configuration.** The server does not push backup plans. What a
  machine backs up is decided on that machine by the person using it; a central
  push would be a remote-code-execution-shaped feature on every laptop.
- **Remote restore.** Rare, high-stakes, supervised, and deliberately startable
  with nothing but the printed card and a downloaded restic binary.
- **Binary distribution and self-update.** The version is reported so "which
  machines are behind" is answerable; acting on it is manual.
- **Mirroring snapshot listings.** The repository is the authority on what it
  contains. A second copy in Eumaeus is a second thing to be wrong.
