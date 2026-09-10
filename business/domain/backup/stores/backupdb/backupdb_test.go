package backupdb_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/backup/stores/backupdb"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// theOldSchema is the run table as it shipped before run_uuid and seeding —
// enough of it to be the thing a machine in the field actually has.
const theOldSchema = `
CREATE TABLE run (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id               TEXT    NOT NULL,
    repository            TEXT    NOT NULL,
    started_at            TEXT    NOT NULL,
    finished_at           TEXT,
    outcome               TEXT    NOT NULL,
    message               TEXT    NOT NULL DEFAULT '',
    snapshot_id           TEXT    NOT NULL DEFAULT '',
    files_new             INTEGER NOT NULL DEFAULT 0,
    files_changed         INTEGER NOT NULL DEFAULT 0,
    total_files_processed INTEGER NOT NULL DEFAULT 0,
    total_bytes_processed INTEGER NOT NULL DEFAULT 0,
    data_added            INTEGER NOT NULL DEFAULT 0,
    unreadable_files      TEXT    NOT NULL DEFAULT '[]',
    verified              INTEGER NOT NULL DEFAULT 0,
    vss_fell_back         INTEGER NOT NULL DEFAULT 0,
    reported_at           TEXT
);`

func open(t *testing.T) (*sqldb.DB, context.Context) {
	t.Helper()

	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db, ctx
}

// TestMigrateAddsColumnsToAnExistingTable. CREATE TABLE IF NOT EXISTS does
// nothing to a table that exists, so a machine that has been backing up for
// months would carry on writing rows without a run_uuid and every one of its
// events would be rejected.
func TestMigrateAddsColumnsToAnExistingTable(t *testing.T) {
	db, ctx := open(t)

	if _, err := db.ExecContext(ctx, theOldSchema); err != nil {
		t.Fatal(err)
	}

	// One row of history from before the change, which must survive.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO run (node_id, repository, started_at, outcome, snapshot_id)
		 VALUES ('office-laptop-1', 's3:https://s3.example.invalid/bucket', ?, 'success', 'old')`,
		time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	if err := backupdb.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Twice, because a migration that is not idempotent fails on the second
	// start rather than the first, which is a much worse day.
	if err := backupdb.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate, second time: %v", err)
	}

	store := backupdb.NewStore(db)

	runs, err := store.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("reading the migrated history: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("history holds %d runs, want the one that was there before", len(runs))
	}

	if runs[0].RunUUID != "" || runs[0].Seeding {
		t.Errorf("an old row came back as uuid %q seeding %v, want empty and false",
			runs[0].RunUUID, runs[0].Seeding)
	}

	// And the new columns are writable.
	id, err := store.Create(ctx, backupbus.Run{
		NodeID:     "office-laptop-1",
		RunUUID:    "0192f3a1-7c4e-7b21-9f10-3c2d5e8a41b7",
		Repository: "s3:https://s3.example.invalid/bucket",
		Seeding:    true,
		StartedAt:  time.Now(),
		Outcome:    backupbus.OutcomeFailed,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	runs, err = store.Recent(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if runs[0].ID != id || runs[0].RunUUID == "" || !runs[0].Seeding {
		t.Errorf("the new row came back as %+v", runs[0])
	}
}

// TestSeenRepositoryIgnoresRunsThatWroteNothing. A repository is seeded by a
// snapshot, not by an attempt: three failed runs against a new bucket leave it
// as empty as it started.
func TestSeenRepositoryIgnoresRunsThatWroteNothing(t *testing.T) {
	db, ctx := open(t)

	if err := backupdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	store := backupdb.NewStore(db)
	const repo = "s3:https://s3.example.invalid/bucket"

	failed := backupbus.Run{
		NodeID:     "office-laptop-1",
		Repository: repo,
		StartedAt:  time.Now(),
		Outcome:    backupbus.OutcomeFailed,
	}

	id, err := store.Create(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}

	failed.ID = id
	failed.FinishedAt = time.Now()

	if err := store.Finish(ctx, failed); err != nil {
		t.Fatal(err)
	}

	seen, err := store.SeenRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	if seen {
		t.Error("a run that wrote no snapshot counted as having filled the repository")
	}

	// An unverified run did write a snapshot. The bucket is not empty any
	// more, whatever else is wrong with it.
	unverified := failed
	unverified.SnapshotID = "a1b2c3d4"
	unverified.Outcome = backupbus.OutcomeUnverified

	if unverified.ID, err = store.Create(ctx, unverified); err != nil {
		t.Fatal(err)
	}

	if err := store.Finish(ctx, unverified); err != nil {
		t.Fatal(err)
	}

	if seen, err = store.SeenRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}

	if !seen {
		t.Error("a written snapshot did not count: the next run would claim to be seeding")
	}

	// Another bucket is another question.
	if seen, err = store.SeenRepository(ctx, repo+"-2"); err != nil {
		t.Fatal(err)
	}

	if seen {
		t.Error("a different repository was reported as already written to")
	}
}
