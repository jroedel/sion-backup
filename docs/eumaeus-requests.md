# What sion-backup needs from Eumaeus

**Audience:** whoever is working on [eumaeus](https://github.com/jroedel/eumaeus).
**Status as of 2026-09-10:** the server at `https://terraboskamp.org` serves all
six endpoints under `/api/backup/v1`, and this client speaks three of them.
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

## 5. Open questions worth a decision

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
