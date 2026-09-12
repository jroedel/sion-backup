package planbus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// memory is a Storer that keeps one plan, which is all this domain has.
type memory struct {
	plan   planbus.Plan
	stored bool
}

func (m *memory) Get(context.Context) (planbus.Plan, error) {
	if !m.stored {
		return planbus.Plan{}, planbus.ErrNoPlan
	}

	return m.plan, nil
}

func (m *memory) Put(_ context.Context, p planbus.Plan) error {
	m.plan, m.stored = p, true

	return nil
}

func (m *memory) Seeded(context.Context) (bool, error) { return false, nil }
func (m *memory) MarkSeeded(context.Context) error     { return nil }
func (m *memory) GetMeasurement(context.Context) (planbus.Measurement, error) {
	return planbus.Measurement{}, nil
}
func (m *memory) PutMeasurement(context.Context, planbus.Measurement) error { return nil }
func (m *memory) GetIntegrity(context.Context) (planbus.Integrity, error) {
	return planbus.Integrity{}, nil
}
func (m *memory) PutIntegrity(context.Context, planbus.Integrity) error { return nil }

func usable() planbus.Plan {
	return planbus.Plan{
		NodeID:     "office-laptop-1",
		Repository: "s3:https://s3.example/bucket",
		Targets:    []string{"/home/jeff"},
		Schedule:   planbus.DefaultSchedule(),
	}
}

// TestAStoredPlanIsNotAConfirmedPlan is the gate's whole premise: writing a
// plan and saying yes to it are two different acts, and only the second one
// releases the scheduler.
func TestAStoredPlanIsNotAConfirmedPlan(t *testing.T) {
	ctx := context.Background()
	b := planbus.NewBusiness(&memory{})

	if err := b.Put(ctx, usable(), time.Now()); err != nil {
		t.Fatal(err)
	}

	plan, err := b.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Fatal("a plan came back confirmed although nobody has confirmed it")
	}

	now := time.Now()

	if err := b.Confirm(ctx, now); err != nil {
		t.Fatal(err)
	}

	plan, err = b.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !plan.ConfirmedAt.Equal(now) {
		t.Errorf("confirmed at %s, want %s", plan.ConfirmedAt, now)
	}
}

// TestConfirmingSurvivesAnEdit checks that changing a setting afterwards does
// not put the machine back behind the gate.
//
// It is what makes the settings page usable at all: somebody adding one
// exclude pattern in March must not find that their laptop has stopped backing
// up until they visit a page they have never seen.
func TestConfirmingSurvivesAnEdit(t *testing.T) {
	ctx := context.Background()
	b := planbus.NewBusiness(&memory{})

	if err := b.Put(ctx, usable(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := b.Confirm(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	plan, err := b.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	plan.Excludes = append(plan.Excludes, "*.iso")

	if err := b.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	after, err := b.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !after.Confirmed() {
		t.Error("editing the plan un-confirmed it, which would stop a working machine")
	}
}

// TestAnUnusablePlanIsNotConfirmed: Confirm validates, so that pressing the
// button on a plan that could not produce a backup does not release the
// scheduler onto it.
func TestAnUnusablePlanIsNotConfirmed(t *testing.T) {
	ctx := context.Background()
	store := &memory{}
	b := planbus.NewBusiness(store)

	// Straight into the store, past Put's own validation, which is the only
	// way to get a plan like this: a row written by an older build, or one
	// edited underneath us.
	if err := store.Put(ctx, planbus.Plan{NodeID: "n", Repository: "r"}); err != nil {
		t.Fatal(err)
	}

	if err := b.Confirm(ctx, time.Now()); err == nil {
		t.Fatal("a plan with no targets was confirmed; it would report success having " +
			"backed up nothing")
	}
}

func TestANegativeSizeLimitIsRefused(t *testing.T) {
	plan := usable()
	plan.SkipLargerThanGB = -1

	if err := plan.Validate(); err == nil {
		t.Error("a negative size limit was accepted")
	}
}

func TestAMissingPlanIsReportedAsSuch(t *testing.T) {
	if _, err := planbus.NewBusiness(&memory{}).Get(context.Background()); !errors.Is(err, planbus.ErrNoPlan) {
		t.Errorf("got %v, want ErrNoPlan", err)
	}
}

// TestAMachineJustEnrolledCanStoreWhatTheServerToldIt.
//
// The state between enrolment and somebody choosing folders: a node ID and a
// repository URL, and no targets yet. It has to be storable, because those two
// facts arrive in `enroll` and nothing else learns them.
//
// When it was not, a Mac enrolled, saved its token, silently failed to write
// this plan, and then told its owner — on the page enrolment had just opened —
// that the computer had never been enrolled. `enroll` meanwhile refused to run
// again, correctly, because it had.
func TestAMachineJustEnrolledCanStoreWhatTheServerToldIt(t *testing.T) {
	ctx := context.Background()
	store := &memory{}
	b := planbus.NewBusiness(store)

	plan := usable()
	plan.Targets = nil

	if err := b.Put(ctx, plan, time.Now()); err != nil {
		t.Fatalf("a freshly enrolled machine could not store its own plan: %v", err)
	}

	got, err := b.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if got.NodeID != plan.NodeID || got.Repository != plan.Repository {
		t.Errorf("stored %+v, want the node ID and repository the server gave", got)
	}
}

// TestAPlanWithNoTargetsIsStorableButNotRunnable is the split, stated once.
func TestAPlanWithNoTargetsIsStorableButNotRunnable(t *testing.T) {
	plan := usable()
	plan.Targets = nil

	if err := plan.Validate(); err != nil {
		t.Errorf("a plan with no targets cannot be stored: %v", err)
	}

	if err := plan.Runnable(); err == nil {
		t.Error("a plan with no targets is runnable; it would back up nothing and " +
			"report success")
	}
}

// TestConfirmingNeedsTargets, because confirming is what releases the
// scheduler.
func TestConfirmingNeedsTargets(t *testing.T) {
	ctx := context.Background()
	store := &memory{}
	b := planbus.NewBusiness(store)

	plan := usable()
	plan.Targets = nil

	if err := b.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := b.Confirm(ctx, time.Now()); err == nil {
		t.Fatal("a plan with no targets was confirmed into the scheduler")
	}
}

// TestAUsablePlanIsStillRunnable, so the split did not quietly let everything
// through.
func TestAUsablePlanIsStillRunnable(t *testing.T) {
	if err := usable().Runnable(); err != nil {
		t.Errorf("an ordinary plan is not runnable: %v", err)
	}

	for _, tc := range []struct {
		name string
		with func(*planbus.Plan)
	}{
		{"no node ID", func(p *planbus.Plan) { p.NodeID = "" }},
		{"no repository", func(p *planbus.Plan) { p.Repository = "" }},
		{"a negative size limit", func(p *planbus.Plan) { p.SkipLargerThanGB = -1 }},
	} {
		plan := usable()
		tc.with(&plan)

		if err := plan.Validate(); err == nil {
			t.Errorf("a plan with %s was accepted for storage", tc.name)
		}

		if err := plan.Runnable(); err == nil {
			t.Errorf("a plan with %s was accepted as runnable", tc.name)
		}
	}
}
