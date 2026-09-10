# Reply to Eumaeus's answers

**Written after** [`eumaeus/docs/sion-backup-answers.md`][answers], as it stood
on branch `backup-api-requests`, commit `4c9ba97`, 2026-09-10 13:17 +0200.
**Continues, and does not replace,** [`eumaeus-requests.md`](eumaeus-requests.md),
which is still the standing list; where the two disagree this document is the
later one and wins.

[answers]: https://github.com/jroedel/eumaeus/blob/backup-api-requests/docs/sion-backup-answers.md

Read in full, acted on, and thank you — particularly for §2, which found a bug
in your code by taking our error handling seriously, and for §4, which found
two settings that could never have held a value. Both are the kind of thing
that would have been discovered at nine in the evening at somebody's desk.

Below: what this client changed in response (§1), the part of our document
your reply did not see (§2), the decisions answered (§3), what is left open
(§4), and where all of it leaves the two machines being enrolled this week
(§5).

---

## 1. What changed here, because of your answers

| Change | Because |
|---|---|
| `401` and `403` are two errors, not one | Your §4: `403` is how a fleet says fresh buckets are off |
| A `400`'s `error` and `field` are decoded and printed as they stand | Your §3.1 |
| `docs/eumaeus-api.md` §3.3 rewritten to match both | It described the old, conflated reading |

**The `403` split is the one worth telling you about, because the bug was
ours and your reply is what exposed it.** This client folded `401` and `403`
into one terminal error meaning *the machine has been de-enrolled* — our own
contract document said to, in three separate tables. Your §4 then says a
rotation request answers `403` when fresh buckets are not on offer, which is
the ordinary state of a fleet where nobody has run
`eumaeus backup settings -fresh-buckets=on`. So the button we are about to
build would have told an owner their machine had been cut off because an
administrator had never changed a setting. It is fixed before it shipped
rather than after, which is the whole value of your having written the
statuses down.

That fix asks one thing of you, and it is the only new constraint in this
document:

> **Please keep revocation and retirement on `401`, and never on `403`.**
> `eumaeuscreds` is now the single place that deliberately treats them alike —
> a token that may not read its own credentials is finished whichever status
> says so — and everywhere else `403` may not mean de-enrolment. Your §5.2
> says retirement already refuses at `Authenticate`, so we believe this is
> already true; we would like it to stay true.

One consequence to check on your side: **the audit-write failure in your §2**
("refusing to hand out credentials that could not be audited") must be a
`5xx`. If it ever answered `403`, this client would report a full disk on your
server as a de-enrolled laptop, and stop.

**On the `400` sentence.** It now reaches the installer as one line and
nothing else:

```
sion-backup: a claim must say what the machine is called (hostname)
```

Two small asks that follow from printing it verbatim. Keep `field` matching
the request's JSON key exactly (`hostname`, not `Hostname`) — it is shown in
brackets and is meant to be findable in what was typed. And keep `error` free
of anything from the machine's own filesystem, since it lands in a log there.
Anything we cannot parse is kept and shown raw, so an HTML error page from a
proxy is still legible; nothing is silently dropped.

**On normalisation (your §3.2).** We deliberately do *not* normalise the code
on this side, now that we know you do: one implementation of Crockford
folding is a thing that cannot disagree with itself, and it should be the one
next to the lookup. If it ever moves, tell us and we will take it.

**On the arithmetic (your §3.3).** Accepted, including that the per-code lock
is not what protects the eight characters. Our §3.3 asked whether the lock was
in; the useful answer turned out to be that the question was wrong. Nothing
here depends on it.

---

## 2. Your reply predates the last section of ours

Timing, since this is the sort of thing that is otherwise discovered by
waiting: our §5 landed in `ee704c0` at 13:04, your reply was committed at
13:17, and it answers §1–§4 and the open questions — which were §5 when you
started. So **§5 of `eumaeus-requests.md` has had no reply**, and it is the
only part of that document asking for anything to be built.

It matters more than the rest because client-side work is already standing on
all three, written against the shapes proposed there:

| Ours, built and waiting | Yours, not built | What happens today |
|---|---|---|
| `business/domain/diag` — install failures and panics, held on disk, sent when accepted, anonymous when there is no token yet | `POST /diagnostics` (§5.1) | Every send is a `404`; reports queue on disk and age out. Correct, and invisible |
| `foundation/selfupdate` — verify, smoke-test, swap, keep the old binary | `agent.version` / `minimum` / `artifacts` in `GET /machines/me` (§5.2) | Falls back to the release's own `SHA256SUMS`, which proves the download was not corrupted, not that it was not replaced |
| `cmd/sion-backup/recon` — prints an existing install's repository, node ID and exclude count without printing the password | Provisioning that adopts an existing bucket (§5.3) | Two machines are being migrated by hand this week with years of snapshots each |

No re-statement here; the section is unchanged and still reads correctly.
**Please read `eumaeus-requests.md` §5 and answer it the way you answered the
rest** — including "no", which is a usable answer for any of the three.

Two additions to it, both from your own reply:

- **Node IDs (your §1).** Your rule is three to forty characters, lowercase,
  starting and ending alphanumeric. The legacy machines' existing IDs —
  `dell3-backup` and its siblings — satisfy it. If §5.3 gets built, we would
  like adoption to keep the legacy ID rather than mint a new one, since the
  point of adopting the bucket is to keep the history attached to the machine
  it belongs to.
- **The compiled-in token** at `https://schoenstatt-fathers.link/en/api/v1/nodes/`
  still wants revoking on that server, whatever happens to the rest. It is the
  one item on our list that is not about this API at all, and the one with a
  credential in the field right now.

---

## 3. The decisions, answered

Your §5 left one of these to us explicitly. Here is that one and the other
two, so that nothing waits on a reply from this side.

### 3.1 Pausing — agreed, and building it that way

Union, neither side clearing the other, and we do not write it back. This
client will take whichever of the local pause and `paused_until` is later and
treat both as suppression of alerts, not of backups. It costs us nothing to
build now against a field that is `null`, so we will, and it will not need
revisiting when the admin side arrives.

### 3.2 The retirement tombstone — no, do not build it

You asked whether the sentence is worth more than you think. It is not.

"This machine has been de-enrolled" is already the true sentence, and the
distinction between *revoked* and *retired* is one no owner has ever needed to
act on differently — in both cases the answer is to speak to whoever
administers the fleet. Keeping a working credential on a machine that may have
been stolen, so that it can read a better sentence off a status page nobody is
looking at, is a bad trade and you are right to have named it as one.

We would rather have the distinction you drew at the end of that section:
**revoking is reversible and re-enrollable, retiring is permanent.** That is
the operationally useful fact, it is now in our runbook, and it needed no code
on either side.

### 3.3 Outage behaviour — taking your recommendation

Three attempts over about five minutes, jittered, inside the run, then an
honest failure. Two notes on where it will and will not go:

- **Only around the credentials fetch**, which is the call that prevents a
  backup rather than degrading one. The hourly state poll will not retry at
  all — the next hour is the retry, and thirty machines retrying a poll is the
  synchronised thundering herd you warned about, bought for nothing.
- **The jitter is already there to borrow.** `planbus.Schedule` spreads the
  fleet's start times by a hash of the node ID and the date; the retry will
  use the same idea rather than a fresh source of randomness, so two machines
  that retry together once do not retry together three times.

**Your half — the fleet falling silent together — we would like on the list
rather than in this reply**, which is where you put it, and that is right. One
observation from this side for whenever it is picked up: this client cannot
detect it at all. A machine that cannot reach Eumaeus cannot report that it
cannot reach Eumaeus, so the correlation is visible only where the machines
are counted. It is genuinely yours; we cannot help with it from here.

---

## 4. What we are waiting on, and one new small thing

### 4.1 The fix in your §2 is not deployed

`backup-api-requests` is not merged: `terraboskamp.org` still answers `400`
when a store fails, and by our §2 this client still marks such a run reported
and drops it. That is the one behaviour where our error handling and your
current production code disagree in a way that loses data quietly.

We are not changing the client to defend against it — the drop-on-`400`
bargain is right, and defending against a server we are about to fix would
mean keeping the defence forever. **Tell us when it is on `main` and
deployed** and we will consider it closed. Until then it is the only known way
this system can silently lose a run that happened.

### 4.2 Please keep the answers document in the repository

We reference it by section from `eumaeus-api.md` and from this file. It is the
only written record of why the per-code lock is not load-bearing, and of what
`fresh_bucket_available` actually scopes to.

### 4.3 A poll-cadence hint, if it is ever cheap

Not a request, an offer. You noted that thirty polls an hour is nothing and
thirty a minute would not be, and that the `ETag` saves bandwidth rather than
work because the poll is also the heartbeat. If a `poll_after_seconds` (or
`Cache-Control: max-age`) ever appeared on the state poll, this client would
honour it, and you would be able to slow the fleet down from your side without
touching thirty machines. Until then we will poll hourly and leave it alone.

---

## 5. Where this leaves the two enrolments this week

Unblocked, and the order in your §1 is what we will follow:
`eumaeus backup check`, then `person add`, `provision`, `code`, with the code
issued while somebody is standing at the machine. `sion-backup enroll` now
prints your refusal sentences as sentences, which is the part of this reply
that will actually be noticed on the evening it matters.

The two machines will be enrolled against fresh buckets and will re-upload,
because §5.3 does not exist yet. That is a decision we can live with for two
machines and not for thirty, which is the whole reason §5.3 is written down.
