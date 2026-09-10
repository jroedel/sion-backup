package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Init creates a repository.
//
// Only ever called deliberately, from `sion-backup enroll`, and never on the
// backup path. A backup that initialises a missing repository is a backup that
// silently starts a brand-new one when a typo appears in the URL, or when the
// bucket has been emptied — and it reports success while doing it. The correct
// answer to "there is no repository at that URL" during a scheduled run is to
// fail loudly.
func (r *Runner) Init(ctx context.Context, repo Repository) error {
	_, err := r.run(ctx, repo, "init")

	return err
}

// Exists reports whether a repository is already there and the password opens
// it.
//
// `cat config` is the cheapest call that proves all three of the things
// enrollment needs to know: the endpoint answers, the S3 credentials are
// accepted, and the password is right. Doing it here means those failures are
// found while an administrator is present, not at 1am.
func (r *Runner) Exists(ctx context.Context, repo Repository) (bool, error) {
	_, err := r.run(ctx, repo, "cat", "config")
	if err == nil {
		return true, nil
	}

	var rerr *Error
	if errors.As(err, &rerr) && rerr.Code == ExitNoRepository {
		return false, nil
	}

	return false, err
}

// Snapshot is one entry from `restic snapshots`.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Username string    `json:"username"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
}

// Snapshots lists what is in the repository, newest last, as restic orders it.
func (r *Runner) Snapshots(ctx context.Context, repo Repository) ([]Snapshot, error) {
	out, err := r.run(ctx, repo, "snapshots", "--json")
	if err != nil {
		return nil, err
	}

	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("restic: reading the snapshot list: %w", err)
	}

	return snaps, nil
}

// RestoreOptions is one restore invocation.
type RestoreOptions struct {
	// Snapshot is an ID, or "latest".
	Snapshot string

	// Target is the directory to restore into. Everything below it may be
	// overwritten, so this is never a directory that is also a backup target
	// — see foundation/paths.Scratch and the comment there.
	Target string

	// Include restricts the restore to matching paths. The verification round
	// trip uses it to pull back one file rather than a terabyte.
	Include []string
}

// Restore pulls files back out of the repository.
func (r *Runner) Restore(ctx context.Context, repo Repository, opts RestoreOptions) error {
	if opts.Snapshot == "" {
		return errors.New("restic: restore needs a snapshot ID or \"latest\"")
	}

	if opts.Target == "" {
		return errors.New("restic: restore needs a target directory")
	}

	args := []string{"restore", opts.Snapshot, "--target", opts.Target}

	for _, inc := range opts.Include {
		args = append(args, "--include", inc)
	}

	_, err := r.run(ctx, repo, args...)

	return err
}

// Check verifies the repository's structure.
//
// Structure only, by default: it reads the metadata and confirms every pack a
// snapshot references exists. It does not read the pack contents, so it will
// not notice a file the storage provider has silently corrupted. That is what
// `--read-data-subset` is for, and it costs egress on every byte it reads —
// which on Wasabi is billed. The scheduler runs the cheap one weekly; the
// expensive one is a decision for a person.
func (r *Runner) Check(ctx context.Context, repo Repository, opts ...CheckOption) error {
	args := []string{"check"}

	var cfg checkConfig
	for _, o := range opts {
		o(&cfg)
	}

	if cfg.subset != "" {
		args = append(args, "--read-data-subset="+cfg.subset)
	}

	_, err := r.run(ctx, repo, args...)

	return err
}

// CheckOption varies what a check actually reads.
type CheckOption func(*checkConfig)

type checkConfig struct{ subset string }

// ReadDataSubset makes the check re-read a slice of the pack data rather than
// only the metadata that refers to it.
//
// This is the difference between "the repository is internally consistent" and
// "the bytes are still there and still decrypt". A plain check reads indexes
// and trees, so it catches a missing pack and a broken reference; it cannot
// catch a pack that is present, correctly named, the right length, and wrong.
// Only reading it back does.
//
// The cost is the point of the slice. Reading everything means downloading the
// whole repository, which on a laptop is measured in hours and on a phone in
// money. "1/52" a week covers all of it in a year and moves about two percent
// of it at a time.
func ReadDataSubset(spec string) CheckOption {
	return func(c *checkConfig) { c.subset = spec }
}

// There is deliberately no Forget or Prune here.
//
// This fleet does not prune. The decision came from measurement, not taste: a
// single prune on a real repository over a gigabit uplink took more than
// twenty-four hours, because prune is not a metadata operation — it downloads
// every pack it wants to consolidate, extracts the live blobs, uploads new
// packs and deletes the originals. The bytes moved are a large fraction of the
// repository, in both directions.
//
// Space is reclaimed by rotating the bucket instead: a fresh repository every
// year or so, and the old one deleted whole. See docs/model.md §5.4.
//
// The consequence is worth stating because it is the strongest security
// property this design has: **no key anywhere in the system can delete backup
// data.** The machine's key may delete only under locks/, and no prune key
// exists to be stolen. The repository is append-only, enforced by IAM, with no
// exception carved out for maintenance.
//
// Adding Forget back would give that away. If history ever needs trimming, the
// answer is to rotate sooner.

// Unlock removes stale locks.
//
// A laptop that is suspended mid-backup leaves a lock behind, and every run
// after it fails with exit 11 until somebody clears it. That is the single
// most common way a machine in this fleet stops backing up, so the runner does
// it automatically on a locked repository rather than waiting for a human.
//
// Only stale locks: restic's own definition, which is a lock whose owning
// process is gone or whose timestamp is old. A lock held by a running restic
// is left alone, because removing that one corrupts what it is protecting.
func (r *Runner) Unlock(ctx context.Context, repo Repository) error {
	_, err := r.run(ctx, repo, "unlock")

	return err
}

// Stats is what `restic stats` reports for one counting mode.
type Stats struct {
	TotalSize      int64 `json:"total_size"`
	TotalFileCount int64 `json:"total_file_count"`
	TotalBlobCount int64 `json:"total_blob_count"`
	SnapshotsCount int   `json:"snapshots_count"`
}

// Size is what a repository would cost to keep, and what a fresh one would
// start at.
//
// # There is no such thing as a full backup in restic
//
// This function exists because the obvious plan — "take a full backup, then
// throw away the old packs" — cannot work, and it is worth being precise about
// why.
//
// restic has no full/incremental distinction. Every `backup` is the same
// operation: walk the source, chunk it, and upload only the blobs the
// repository does not already hold. There is no --full flag, and `--force`
// only forces re-*reading* the source files; the chunks it produces still
// deduplicate against the existing index, so nothing new is written and no
// pack is freed. A "full backup" into an existing repository uploads almost
// nothing and reclaims exactly nothing.
//
// The only way to end up holding just the current data is a repository that
// has never held anything else — a new bucket. That is what rotation is, and
// it is why the old bucket is deleted whole rather than cleaned up.
//
// So the two numbers below are the honest ones to put in front of somebody:
//
//	Now    every blob in the repository, which is what the bill is for
//	Fresh  the blobs the newest snapshot actually needs
//
// The difference is what rotation reclaims, and it is also exactly the history
// that rotation discards. Saying both in one breath is the only fair way to
// ask for two days of somebody's bandwidth.
type Size struct {
	// Now is the deduplicated size of everything in the repository.
	Now int64

	// Fresh is the deduplicated size of the latest snapshot alone.
	Fresh int64

	// Snapshots is how many the repository holds.
	Snapshots int
}

// Reclaimable is Now minus Fresh, floored at zero.
func (s Size) Reclaimable() int64 {
	if s.Now <= s.Fresh {
		return 0
	}

	return s.Now - s.Fresh
}

// Measure reads both figures.
//
// Both are read-only, so the machine's own key is enough — measuring never
// needs a credential that could delete anything.
func (r *Runner) Measure(ctx context.Context, repo Repository) (Size, error) {
	whole, err := r.stats(ctx, repo, "raw-data", "")
	if err != nil {
		return Size{}, err
	}

	latest, err := r.stats(ctx, repo, "raw-data", "latest")
	if err != nil {
		return Size{}, err
	}

	return Size{Now: whole.TotalSize, Fresh: latest.TotalSize, Snapshots: whole.SnapshotsCount}, nil
}

func (r *Runner) stats(ctx context.Context, repo Repository, mode, snapshot string) (Stats, error) {
	args := []string{"stats", "--json", "--mode", mode}
	if snapshot != "" {
		args = append(args, snapshot)
	}

	out, err := r.run(ctx, repo, args...)
	if err != nil {
		return Stats{}, err
	}

	var s Stats
	if err := json.Unmarshal(out, &s); err != nil {
		return Stats{}, fmt.Errorf("restic: reading stats output: %w", err)
	}

	return s, nil
}
