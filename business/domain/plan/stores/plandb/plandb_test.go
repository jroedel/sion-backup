package plandb_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// theOldSchema is the plan table as the first releases wrote it: no style, no
// confirmed_at, no skip columns.
//
// Written out here rather than fetched from git, because what is being tested
// is what happens to a database that exists on a real machine today, and the
// only honest way to have one of those in a test is to create it.
const theOldSchema = `
CREATE TABLE plan (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    node_id            TEXT    NOT NULL,
    repository         TEXT    NOT NULL,
    targets            TEXT    NOT NULL,
    excludes           TEXT    NOT NULL,
    schedule_times     TEXT    NOT NULL,
    jitter_minutes     INTEGER NOT NULL,
    min_interval_secs  INTEGER NOT NULL,
    pack_size_mib      INTEGER NOT NULL,
    read_concurrency   INTEGER NOT NULL,
    use_fs_snapshot    INTEGER NOT NULL,
    allow_vss_fallback INTEGER NOT NULL,
    one_file_system    INTEGER NOT NULL,
    paused             INTEGER NOT NULL,
    updated_at         TEXT    NOT NULL
);
CREATE TABLE plan_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

func open(t *testing.T) *sqldb.DB {
	t.Helper()

	db, err := sqldb.Open(context.Background(), filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// TestAMachineThatWasAlreadyBackingUpKeepsBackingUp is the most consequential
// test in this package.
//
// The scheduler now refuses to start a run against a plan nobody has confirmed
// — see planbus.Plan.ConfirmedAt. Every machine in the fleet has a plan that
// predates the column, and if the migration left those at the column's empty
// default, taking this release would stop every one of them backing up, on the
// same day, silently. The backfill in plandb.addColumns is what prevents that,
// and this is what proves it.
func TestAMachineThatWasAlreadyBackingUpKeepsBackingUp(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	if _, err := db.ExecContext(ctx, theOldSchema); err != nil {
		t.Fatal(err)
	}

	edited := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)

	// A plan as an older build would have left it: a real machine, backing up
	// nightly, set up three months ago.
	_, err := db.ExecContext(ctx, `
		INSERT INTO plan VALUES (1, 'office-laptop-1', 's3:https://example/bucket',
			'["/home/jeff"]', '["*.iso"]', '["13:00"]', 30, 21600, 16, 4, 0, 1, 1, 0, ?)`,
		edited.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatalf("migrating a database from an older build: %v", err)
	}

	plan, err := plandb.NewStore(db).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !plan.Confirmed() {
		t.Fatal("a plan that was already running came back unconfirmed: taking this " +
			"release would stop this machine backing up")
	}

	// As of when it was last edited, not as of the migration. That is the
	// honest answer — somebody chose this plan, on that day — and it also
	// keeps the date stable across repeated migrations.
	if !plan.ConfirmedAt.Equal(edited) {
		t.Errorf("confirmed at %s, want the day the plan was last edited, %s",
			plan.ConfirmedAt, edited)
	}

	// Nothing else may have moved.
	if plan.NodeID != "office-laptop-1" || len(plan.Targets) != 1 || plan.Targets[0] != "/home/jeff" {
		t.Errorf("the migration changed the plan: %+v", plan)
	}
}

// TestMigratingTwiceChangesNothing covers the ordinary case: the daemon
// applies every migration on every start.
func TestMigratingTwiceChangesNothing(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	if _, err := db.ExecContext(ctx, theOldSchema); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan VALUES (1, 'n', 'r', '["/a"]', '[]', '["13:00"]', 30, 21600,
			0, 0, 0, 1, 1, 0, ?)`, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		if err := plandb.Migrate(ctx, db); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}

	store := plandb.NewStore(db)

	plan, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !plan.Confirmed() {
		t.Error("a repeated migration lost the confirmation")
	}

	// And a plan that is deliberately unconfirmed must stay that way through
	// the next start, or a machine waiting to be set up would set itself up.
	plan.ConfirmedAt = time.Time{}

	if err := store.Put(ctx, plan); err != nil {
		t.Fatal(err)
	}

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	again, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if again.Confirmed() {
		t.Error("a migration confirmed a plan that was deliberately waiting for somebody")
	}
}

// TestAFreshMachineStartsUnconfirmed is the other half: a database created by
// this build, with a plan written into it by enrolment, must wait.
func TestAFreshMachineStartsUnconfirmed(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	store := plandb.NewStore(db)

	if err := store.Put(ctx, planbus.Plan{
		NodeID:     "new-laptop",
		Repository: "s3:https://example/bucket",
		Targets:    []string{"/home/jeff"},
		Schedule:   planbus.DefaultSchedule(),
		UpdatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("a plan nobody has answered for came back confirmed")
	}
}

// TestTheNewColumnsSurviveARoundTrip is the ordinary storage test for the
// three settings the setup page added.
func TestTheNewColumnsSurviveARoundTrip(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	store := plandb.NewStore(db)
	confirmed := time.Now().Truncate(time.Second)

	want := planbus.Plan{
		NodeID:           "n",
		Repository:       "s3:https://example/bucket",
		Targets:          []string{"/home/jeff"},
		Schedule:         planbus.Hourly(),
		Style:            "home",
		SkipLargerThanGB: 2,
		SkipOnMetered:    true,
		ConfirmedAt:      confirmed,
		UpdatedAt:        confirmed,
	}

	if err := store.Put(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case got.Style != want.Style:
		t.Errorf("style %q, want %q", got.Style, want.Style)
	case got.SkipLargerThanGB != want.SkipLargerThanGB:
		t.Errorf("size limit %d, want %d", got.SkipLargerThanGB, want.SkipLargerThanGB)
	case got.SkipOnMetered != want.SkipOnMetered:
		t.Errorf("metered setting %v, want %v", got.SkipOnMetered, want.SkipOnMetered)
	case !got.ConfirmedAt.Equal(confirmed):
		t.Errorf("confirmed at %s, want %s", got.ConfirmedAt, confirmed)
	case len(got.Schedule.Times) != 24:
		t.Errorf("an hourly schedule came back with %d times", len(got.Schedule.Times))
	}
}
