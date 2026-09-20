package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// cardHarness is a machine with a plan and nobody to report to. The fleet
// being absent is deliberate: every refusal below happens before anything
// would be sent, and a test that reached the network would be testing the
// wrong thing.
func cardHarness(t *testing.T, repository string) (*deps, context.Context) {
	t.Helper()

	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	d := &deps{
		log:  quietLog(),
		db:   db,
		plan: planbus.NewBusiness(plandb.NewStore(db)),
	}

	if repository != "" {
		plan := planbus.Plan{
			NodeID:     "office-laptop-1",
			Repository: repository,
			Targets:    []string{t.TempDir()},
			Schedule:   planbus.Schedule{Times: []string{"02:00"}},
		}

		if err := d.plan.Put(ctx, plan, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	return d, ctx
}

// TestACardForABucketThisMachineHasLeftIsNotRecorded is the race the hidden
// field exists to catch: a cutover between the printing and the confirming.
//
// The paper in somebody's hand opens the old bucket. Recording it against the
// new one would tell everybody reading card_issued_at that a bucket nobody
// has ever printed for has a card, which is exactly the claim the whole
// second button exists to keep honest.
func TestACardForABucketThisMachineHasLeftIsNotRecorded(t *testing.T) {
	d, ctx := cardHarness(t, "s3:https://s3.example.invalid/new-bucket")

	err := d.cardPrinted(ctx, "s3:https://s3.example.invalid/old-bucket")
	if err == nil {
		t.Fatal("a card for a bucket this machine has left was recorded")
	}

	if !strings.Contains(err.Error(), "no longer backs up to") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// And it says what to do about it, because somebody holding a freshly
	// printed page needs to know it is the wrong page.
	if !strings.Contains(err.Error(), "Print a new card") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// TestACardWithNoRepositoryIsNotRecorded. A confirmation that names nothing
// cannot be checked against anything, so it is refused rather than recorded
// against whatever this machine happens to be using.
func TestACardWithNoRepositoryIsNotRecorded(t *testing.T) {
	d, ctx := cardHarness(t, "s3:https://s3.example.invalid/bucket")

	err := d.cardPrinted(ctx, "")
	if err == nil {
		t.Fatal("a confirmation naming no repository was recorded")
	}

	if !strings.Contains(err.Error(), "which repository") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestConfirmingACardMatchesTheCurrentRepository. The ordinary case: the card
// names the bucket this machine backs up to, so the check passes and the
// record is attempted. There is no fleet here, which machinebus reports as
// nothing to tell -- and that is a success, because everything that could be
// done was.
func TestConfirmingACardMatchesTheCurrentRepository(t *testing.T) {
	const repository = "s3:https://s3.example.invalid/bucket"

	d, ctx := cardHarness(t, repository)

	if err := d.cardPrinted(ctx, repository); err != nil {
		t.Errorf("a card for this machine's own bucket was refused: %v", err)
	}
}

// TestRecordingACardWithoutAPlan. A machine that has never been given a
// repository has no card to have printed, and says so rather than reporting
// one against an empty string.
func TestRecordingACardWithoutAPlan(t *testing.T) {
	d, ctx := cardHarness(t, "")

	err := d.confirmCardPrinted(ctx)
	if err == nil {
		t.Fatal("a machine with no repository recorded a card")
	}

	if !strings.Contains(err.Error(), "no backup plan") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
