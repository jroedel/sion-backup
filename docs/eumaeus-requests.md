# What sion-backup needs from Eumaeus

**Audience:** whoever is working on [eumaeus](https://github.com/jroedel/eumaeus).
**Status as of 2026-09-10:** the server at `https://terraboskamp.org` serves all
six endpoints under `/api/backup/v1`, and this client speaks three of them.
§5 is the part with new work in it: a place to send install failures, an
authoritative agent version, and provisioning that can adopt the repositories
the machines being migrated already have.
Nothing here is a complaint about what is built — most of this document is
"please keep doing that", written down so it does not regress.

The contract is [`eumaeus-api.md`](eumaeus-api.md); this is the client's side of
the conversation about it.

---

## 1. To enrol two machines this week

Two machines are being deployed by hand: one Linux laptop, and a colleague's
machine the day after. Each needs, in this order:

| # | What | Command on the server |
|---|---|---|
| 1 | The owner recorded | `eumaeus backup person add …` |
| 2 | A bucket, its two keys, and the machine record | `eumaeus backup provision …` |
| 3 | A one-time code, carried to the machine | `eumaeus backup code <node-id>` |

**Node IDs** need agreeing before provisioning, because they are what the
history is filed under and changing one later detaches a machine from its own
past. They should say whose machine it is and survive a reimage.

**The code is good for fifteen minutes and once.** Whoever is standing at the
machine needs it issued while they are standing there, not the evening before.

### The one provisioning detail that will break a backup on night one

The machine key must be able to **delete objects under `locks/`** and nowhere
else. restic writes a lock at the start of every run and removes it at the end;
a key that cannot remove its own lock leaves the repository locked and the
second run fails with a stale lock. `docs/model.md` §5.2 has the rule, and
`wasabi/policy.go` looks like it already implements it — please confirm the
policy actually attached to the machine key on a provisioned bucket, because it
is invisible until the second night.

The restore key on the owner's printed card must be **read-only**, so a card
that goes astray cannot destroy the backup.

---

## 2. Behaviours this client now depends on

These were verified against the live server on 2026-09-10 and all behave
correctly today. They are listed because the client's error handling is built on
them, and a regression in any one of them is silent from the server's side.

| Behaviour | Why the client cares |
|---|---|
| **`401` for a token that is unknown, malformed, revoked or expired** — never `500` | `401` is the only status the client treats as terminal. It stops trying and the status page says the machine has been de-enrolled. Anything else is retried hourly forever, and a revoked laptop would tell its owner the server is having trouble |
| **`404` for an enrollment code that is unknown or expired** | Becomes "that code is not valid, or has expired" in front of whoever is standing at the machine |
| **`409` for a code that has already been claimed** — and never a fresh token | A retry after a dropped response is indistinguishable from a replayed code |
| **`400` only for a body that could not be parsed** | The client marks such a run reported and drops it. A `400` returned for an event that is merely unwelcome would silently erase it from the dashboard |
| **Unknown request fields ignored** | Lets this client add a field without a coordinated deployment of thirty machines |
| **Idempotent on `(machine, run_uuid, phase)`** | The client marks a run reported only after the POST succeeds, so a lost acknowledgement produces a duplicate. It cannot resolve that; the server can |
| **The repository URL handed back verbatim** | It goes into the local plan, onto the status page, and onto the owner's printed card. A URL reassembled from provider/region/bucket would eventually be reassembled differently, and a card printed last year would stop working |

Two more from `eumaeus-api.md` §10 that cannot be verified from outside, and
that matter more than any of the above:

- **Never log the body of the claim or the credentials read.** They carry the
  keys to somebody's entire backup.
- **Audit every credential read** — who, when, from where; never the value. A
  stolen laptop being used should be a visible row.

---

## 3. Small things noticed while testing

Neither blocks anything.

1. **An internal package name reaches the user.** A claim with no hostname
   answers:

   ```json
   { "error": "backupbus: a claim must say what the machine is called" }
   ```

   That sentence is printed verbatim by `sion-backup enroll` to whoever is
   installing the machine. §3.3 asks for one sentence for a human; the `backupbus:`
   prefix is the only thing to drop.

2. **Enrollment codes appear to be matched literally.** `k4tp-9qx2` and
   `K4TP9QX2` are both rejected the same way a wrong code is, which is correct
   behaviour for a wrong code but indistinguishable from one typed in lower case
   or without the hyphen. Crockford base32 is normally case-insensitive and
   ignores hyphens, and somebody reading a code off a screen at the far end of a
   room will type it lower case. Normalising before lookup would save a support
   call; if it is already normalised, ignore this.

3. **Rate limiting on the claim** (§2.1: no more than 10 attempts per minute per
   source, lock a code after 5 failures against it) — is it in? Eight base32
   characters is 40 bits, which is guessable unthrottled. Not testable politely
   from outside.

---

## 4. What the client will build next, and what it needs

In the order they are likely to be built. Each is a client-side job; the
server-side endpoint already exists in all three cases, so what is wanted here is
confirmation of the payloads rather than new work.

| Client work | Endpoint | What we would like confirmed |
|---|---|---|
| Hourly state poll | `GET /machines/me` | That `paused_until`, `state: retired` and `fresh_bucket_available` are populated in practice, and that polling hourly from thirty machines is welcome |
| The "ask for a fresh bucket" button | `POST /machines/me/rotation-request` | That `{repository_url, reclaimable_bytes, fresh_bytes, measured_at}` is the shape wanted, and what the owner should be told happens next |
| Card printed | `POST /machines/me/card-issued` | That `{repository_url, printed_at}` is right, and whether re-printing a card should post again |

Two things the client would rather read than be configured with:

- **`storage_price_per_tib_month`** from `GET /machines/me`. It is local config
  today (`[storage] price_per_tib_month`), which means a change of provider is an
  edit on every machine. Zero means "talk in gigabytes, not money", which is the
  right answer when nobody has said.
- **`repository.created_at`**, so the status page can say "backups available
  since March" rather than implying a history nothing enforces.

---

## 5. Three things we would like built

These are new. Each one exists because of something being done to the client
this week, and each is small on its own.

### 5.1 Somewhere to send install failures and panics

`POST /api/backup/v1/diagnostics`, **accepted with or without a machine
token**.

The anonymous case is the point of it. Machines are being migrated by hand,
and the failures worth hearing about are the ones where the install did not
finish — which is to say, before there is a token. Somebody standing at a
colleague's laptop at nine in the evening is not going to copy a stack trace
into an email, and the failure that nobody reports is the one that happens
again on the next machine.

```json
{
  "kind": "install-failed",
  "occurred_at": "2026-09-10T21:14:02.117+02:00",
  "agent": "v1.4.0",
  "os": "windows/amd64",
  "install_id": "0192f3a1-7c4e-7b21-9f10-3c2d5e8a41b7",
  "node_id": "office-laptop-1",
  "step": "verify-restic-hash",
  "detail": "sha256 mismatch: got 9f2c..., want 4a1b...",
  "prior_version": "legacy-windows-1.3"
}
```

| Field | |
|---|---|
| `kind` | `install-failed`, `panic`, `update-failed`, `update-rolled-back`, `repository-damaged`. Treat an unknown kind as another rather than rejecting it |
| | `repository-damaged` is the gravest: a repository failed its own integrity check, which means the backups ALREADY TAKEN may not come back. Every other kind is about a backup that did not happen. This one should page somebody |
| | `update-rolled-back` is the one to surface loudest: it means a release installed, would not stay running, and the machine put the previous version back. One is a reason to look; two from different machines is a reason to un-publish the release before the rest of the fleet takes it |
| `install_id` | Generated by the installer. Dedupe on `(install_id, kind)` so a retry is not a second row |
| `node_id` | Absent when the machine never got that far |
| `step` | Which part of the install; ours, short, from a fixed list |
| `detail` | Our own error text and stack frames. Capped at 16 KiB |

What the client promises about `detail`: **never a path from the user's file
tree, never a credential.** It carries our error strings and the file and
function names of our own source. Treat it as untrusted text all the same —
it reaches the server from a machine that may be the compromised one, so
escape it before it is rendered anywhere.

What we would like back: `202` or `204`, always, even for a body you decide to
drop. A client retrying a diagnostic report is a client spending its evening on
something that is not a backup. Rate-limit by source and cap the body; the
client sends at most a handful per install and one per panic, and holds them on
disk until they are accepted.

Somewhere to read them matters as much as somewhere to send them —
`eumaeus backup show <node-id>` listing recent reports, and some way to see the
ones with no node at all.

### 5.2 The version the fleet should be running

The client is gaining self-update: it checks on every run, and pulls binaries
built by GitHub Actions on tag. That needs an authoritative answer to "what
should this machine be running, and what should it hash to".

We would like it in `GET /machines/me`:

```json
"agent": {
  "version": "v1.4.0",
  "minimum": "v1.2.0",
  "artifacts": {
    "windows/amd64": { "url": "https://github.com/.../sion-backup-windows-amd64.exe",
                       "sha256": "4a1b..." },
    "linux/amd64":   { "url": "...", "sha256": "..." }
  }
}
```

**Why not just trust GitHub.** The release and its `SHA256SUMS` sit on the same
host, so a hash taken from there proves the download was not corrupted — not
that it was not replaced. Whoever can push a release can push both. This
machine already trusts Eumaeus with the repository password, so nothing is lost
by having Eumaeus name the hash, and something real is gained: a compromised
release cannot reach the fleet without also compromising Eumaeus.

It also buys three operational things that are hard to get any other way: a
staged rollout (update one machine, watch it for a night), a kill switch for a
bad build, and a rollback that does not mean touching thirty computers.

`minimum` is optional and would let the server refuse a client too old to be
trusted. Until any of this exists the client verifies against the release's own
`SHA256SUMS` and refuses to install a binary that is not listed in it.

### 5.3 Provisioning that can adopt an existing repository

Every machine being migrated already has a bucket with years of snapshots in
it, and a password for it. Provisioning today creates a new bucket, which means
two things nobody wants: a full re-upload measured in days, and a history that
starts the day we migrated.

The ask is a provisioning path that adopts what is already there — the existing
bucket, with new IAM keys minted against it (rather than reusing the legacy
keys, which have been in a plain-text script on a laptop for two years) and the
existing restic password registered as the repository's password.

Per machine we can hand over: the repository URL, the legacy node ID, the
exclude list, and the existing password. `sion-backup recon` on the machine
prints the first three and says which file the password is in without printing
it, so the handover is one screen:

```
legacy install   FOUND (linux 1.1)
  repository     s3:https://s3.us-central-1.wasabisys.com/<bucket>
  node id        dell3-backup
  excludes       11 entries in /home/user/Documents/backup-dell3/excludes.txt
  credentials    present in the script — not shown here
```

The password itself comes out of band, from that file.

One consequence worth knowing about, which is ours to handle either way: the
`seeding` flag on run events is answered from the machine's local history,
which for a freshly migrated machine is empty — so the first run would claim to
be seeding a repository that already holds a year of snapshots. If the claim
response said whether the repository already has snapshots (`repository.
adopted: true`, or a snapshot count) we would use it and be exact. Otherwise we
will ask restic once at enrollment and record the answer.

### And one thing that is not ours

The legacy `nodes` binaries have an API token for
`https://schoenstatt-fathers.link/en/api/v1/nodes/` **compiled into them**, and
that binary was installed on every machine in the old fleet. Deleting our copy
achieves nothing; the token wants revoking on that server, whether or not any
of the rest of this happens.

---

## 6. Open questions worth a decision

From §12 of the specification, the two that affect what the client does:

1. **Does pausing come from the server?** It is local today — the owner pauses
   their own machine. If an admin can also pause from Eumaeus, the two need
   reconciling. Last-writer-wins is probably fine; it should be decided rather
   than discovered.
2. **How does a machine learn it has been retired?** `GET /machines/me` returns
   `state: retired`, but a retired machine may simply never poll again. This
   matters less than it did — there are no cached credentials to delete, and
   revoking the token stops the machine dead — but a client that knew could say
   so instead of reporting failures.

And one from this side:

3. **What should a machine do during an Eumaeus outage?** Nothing is cached, so
   an outage means the whole fleet stops backing up, and with `warn_after_hours`
   at 240 an outage of a day or two passes unnoticed. The client currently fails
   the run and tries again on the next schedule. A few minutes of retry with
   backoff inside the run would absorb a restart; more than that just delays an
   honest failure. Is a maintenance window something the server could announce —
   or is this squarely the client's problem?
