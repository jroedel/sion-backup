# rotation-gate

The bucket rotation loop, run against a real Eumaeus.

```
make rotation-gate                # or: scripts/rotation-gate/run
EUMAEUS_DIR=~/src/eumaeus make rotation-gate
scripts/rotation-gate/run --reap-only
```

About ninety seconds, and it needs an Eumaeus checkout, a Go toolchain and the
Wasabi credentials the rest of the release path uses. Without any of those it
exits 2 and says which, because most machines that build this repository do not
have the server's source and a gate that fails on them is a gate people learn
to ignore.

## Why it exists

Every other test of rotation here runs against `eumaeusstub` — a server in
`scripts/backup-e2e/eumaeusstub`, written by the same person who read the
specification. Those tests prove the client is consistent with one reading of
the contract. They cannot prove the reading is right.

That matters because a misreading is the failure that actually reaches an
owner. Both sides pass their own tests, both sides are internally coherent, and
a laptop is pointed at a bucket nobody meant. No amount of care on one side
finds it. The only thing that does is running the two programs against each
other.

## The assertion that paid for the file

`repository.created_at`.

The whole rotation age policy hangs off it — `RotateAfter` at a year,
`InsistAfter` at two, in `business/domain/plan/planbus/rotation.go` — and the
API documentation can be read two ways: the date the repository row was made,
or the date the owner's snapshots start. For a bucket created last week those
are the same date. For an adopted bucket that has been backing up since 2023
they differ by years, and the two readings put that machine at opposite ends of
the policy:

- *the row's date* — brand new, say nothing for a year
- *the history horizon* — two and a half years old, insist

Nothing in either repository's tests can tell them apart, because each side is
self-consistent under both. So the gate adopts a bucket with a horizon thirty
months back, enrols a machine against it, and asks the server.

The answer is the history horizon. `Business.State` serves
`repo.HistoryHorizon()`, which is `HistorySince` when set and the row's
`CreatedAt` otherwise. The policy is measuring how long the owner's data has
been accumulating, which is what it was meant to measure and what `README.md`
and `docs/model.md` §5.5 say.

It is checked rather than remembered, because the day somebody changes that one
line to `repo.CreatedAt` — a change that looks like a tidy-up — every adopted
machine in the fleet silently becomes a new one, no legacy bucket ever reaches
`RotateAfter`, and no test on either side goes red.

## What it fakes, and why exactly that

One call: `backupbus.Provisioner.Provision`, which creates a bucket and mints
two IAM identities at Wasabi. `_eumactl/main.go` carries the argument in full.
Briefly: it is real money and a real key per run, for the one part of a
rotation that is Eumaeus's business with a provider rather than a contract
between these two programs. Eumaeus's own tests cover it. Nothing a client can
observe depends on it.

Everything either side of it is real — `PlanAdoption`, `PlanProvision`,
`SettleProvision`, `stateForNewRepository`, `expectEmpty`, `BeginCutover`,
`ReleaseOldBucket`, `PlanRetirement`, `cardFor` — and so are the ten endpoints,
the vault, the storage and the restic repositories.

The two "buckets" are two prefixes in the one test bucket. Nothing in Eumaeus
can delete a bucket, by policy, so a gate that made one per run would leave a
trail for a person to clean up by hand; a prefix is a repository as far as
restic and the contract are concerned, and the reaper can remove it.

## The order it walks

1. Build Eumaeus from the checkout; `vault init`, `user add -primary`,
   `backup init`, `serve`. The two password prompts go through `script(1)`,
   because Eumaeus reads them from `/dev/tty` and deliberately not from stdin.
2. An owner, and one adopted bucket with thirty months of history.
3. `sion-backup enroll`, then a backup. From here the gate uses the token the
   **client** stored: a second claim is a second enrolment, and Eumaeus revokes
   the token before it.
4. What `created_at` means. See above.
5. The machine's own reading of it, on `/rotation`.
6. The ask, and asking twice.
7. A second bucket — and Eumaeus, not the gate, decides it is `offered`.
8. The machine notices, shows the offer, and does not move.
9. The cutover; `expect_empty` turns true *here* and not before.
10. Releasing the old bucket is refused while the new one is empty.
11. The seeding run — the one time this program creates a repository — then the
    release, then retirement, then the card going `never` → `superseded` →
    `issued`.
12. The refusal.

## The refusal

The machine's own bucket is emptied underneath it: every object deleted, the
row in Eumaeus untouched and `active`. It must fail rather than start again.

This is the failure in the real world — a bucket emptied by a person cleaning
up, a lifecycle rule, a restore gone wrong — and a build that creates a
repository here reports a successful backup every night afterwards, against an
empty one, until somebody needs a file.

It is done by deleting objects rather than by pointing the machine somewhere
else, which is the difference from `scripts/backup-e2e/run-rotation`. That
harness has an admin endpoint for pointing a machine at an empty URL; a real
server has no such thing, and will not offer an active machine an empty bucket.
Reaching into the database to arrange one would be testing a state the system
cannot produce. Emptying a bucket is a state the system very much can produce.

The precondition — that the server is *not* offering `expect_empty` — is
asserted first. An assertion that cannot fail is worse than no assertion.

## Where it belongs

Not in `release.yml`. A cutover re-uploads every byte the machine holds, and
adding that to every tag buys a slow release rather than a safe one.

A nightly, and a thing to run by hand when either side's rotation code changes.
