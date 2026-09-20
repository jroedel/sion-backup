package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
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

// TestACardIsRecordedAgainstTheBucketItNames, and not against wherever this
// machine happens to be writing when the button is pressed.
//
// The two differ for a few minutes after a cutover, which is exactly when an
// owner is told to print a card -- and the plan's copy of the repository is
// the one that lags. Recording what the card says keeps the statement true:
// paper exists for that bucket. The new one keeps its card state of `never`,
// which is also true, and goes on asking.
func TestACardIsRecordedAgainstTheBucketItNames(t *testing.T) {
	const printed = "s3:https://s3.example.invalid/old-bucket"

	d, ctx := cardHarness(t, "s3:https://s3.example.invalid/new-bucket")

	fleet := &cardFleet{}
	d.machine = machinebus.NewBusiness(fleet)

	if err := d.cardPrinted(ctx, printed); err != nil {
		t.Fatalf("cardPrinted: %v", err)
	}

	if fleet.issued != printed {
		t.Errorf("recorded against %q, want the bucket the card named (%q)", fleet.issued, printed)
	}
}

// TestACardWithNoRepositoryIsNotRecorded. A confirmation that names nothing
// cannot be recorded as anything, and guessing would put a card against a
// bucket nobody printed one for.
func TestACardWithNoRepositoryIsNotRecorded(t *testing.T) {
	d, ctx := cardHarness(t, "s3:https://s3.example.invalid/bucket")

	fleet := &cardFleet{}
	d.machine = machinebus.NewBusiness(fleet)

	err := d.cardPrinted(ctx, "")
	if err == nil {
		t.Fatal("a confirmation naming no repository was recorded")
	}

	if !strings.Contains(err.Error(), "which repository") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	if fleet.issued != "" {
		t.Errorf("something was recorded anyway: %q", fleet.issued)
	}
}

// TestPrintedAsksTheServerWhereThisMachineWrites. `--printed` fetches no
// credentials, so it has no card to read the repository off -- and the plan's
// copy is the one that lags a cutover. The server is the authority and is
// asked first.
func TestPrintedAsksTheServerWhereThisMachineWrites(t *testing.T) {
	d, ctx := cardHarness(t, "s3:https://s3.example.invalid/what-the-plan-still-says")

	fleet := &cardFleet{now: "s3:https://s3.example.invalid/where-it-writes-now"}
	d.machine = machinebus.NewBusiness(fleet)

	if err := d.confirmCardPrinted(ctx); err != nil {
		t.Fatalf("confirmCardPrinted: %v", err)
	}

	if fleet.issued != fleet.now {
		t.Errorf("recorded against %q, want the server's answer (%q)", fleet.issued, fleet.now)
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

// cardFleet is a server that answers where this machine writes and remembers
// what it was told about cards. Every other call is unused here and says so
// rather than pretending to work.
type cardFleet struct {
	now    string
	issued string
}

func (f *cardFleet) State(context.Context) (machinebus.State, error) {
	if f.now == "" {
		return machinebus.State{}, machinebus.ErrNotEnrolled
	}

	return machinebus.State{NodeID: "office-laptop-1", RepositoryURL: f.now}, nil
}

func (f *cardFleet) CardIssued(_ context.Context, repositoryURL string, _ time.Time) error {
	f.issued = repositoryURL

	return nil
}

func (f *cardFleet) RequestRotation(context.Context, machinebus.Measured) error {
	return errors.New("not used in these tests")
}

func (f *cardFleet) Cutover(context.Context, string) error {
	return errors.New("not used in these tests")
}

func (f *cardFleet) ReleaseOldBucket(context.Context, string) (machinebus.Released, error) {
	return machinebus.Released{}, errors.New("not used in these tests")
}
