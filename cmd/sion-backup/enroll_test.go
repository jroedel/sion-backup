package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/credential/sources/eumaeuscreds"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// planFor runs the enrolment's plan-writing step against a real database and
// returns whatever it left behind.
func planFor(t *testing.T, e eumaeuscreds.Enrollment) (planbus.Plan, error) {
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

	d := &deps{log: quietLog(), plan: planbus.NewBusiness(plandb.NewStore(db))}

	if err := d.storePlan(ctx, e); err != nil {
		t.Fatalf("storing the plan: %v", err)
	}

	return d.plan.Get(ctx)
}

// TestEnrolmentRemembersTheRepositoryWithNoTargetsYet.
//
// The whole point of the command: the server tells this machine its node ID
// and which bucket it writes to, and nothing else ever will. A machine
// installed from a bare binary has no config.toml to seed targets from, so
// this is the ordinary path and not an edge case.
//
// It regressed exactly once, on the first Mac: the plan was assembled, found
// to have no targets, and dropped rather than written. The machine kept its
// token, forgot its repository, and told its owner it had never been enrolled.
func TestEnrolmentRemembersTheRepositoryWithNoTargetsYet(t *testing.T) {
	plan, err := planFor(t, eumaeuscreds.Enrollment{
		NodeID:        "macbook-air",
		RepositoryURL: "s3:https://s3.example/macbook-air-backup",
	})
	if err != nil {
		t.Fatalf("a machine that has just enrolled has no plan at all: %v", err)
	}

	switch {
	case plan.NodeID != "macbook-air":
		t.Errorf("node ID %q, want the one the server gave", plan.NodeID)
	case plan.Repository != "s3:https://s3.example/macbook-air-backup":
		t.Errorf("repository %q, want the one the server gave", plan.Repository)
	case len(plan.Targets) != 0:
		t.Errorf("targets %v, want none until somebody chooses", plan.Targets)
	}
}

// TestAJustEnrolledMachineIsNotYetSchedulable.
//
// The other half, and the reason the plan above is safe to store: it cannot
// run and it cannot be confirmed. A backup of no targets would report success.
func TestAJustEnrolledMachineIsNotYetSchedulable(t *testing.T) {
	plan, err := planFor(t, eumaeuscreds.Enrollment{
		NodeID:        "macbook-air",
		RepositoryURL: "s3:https://s3.example/macbook-air-backup",
	})
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("enrolment confirmed the plan; nobody at the machine has chosen anything")
	}

	if err := plan.Runnable(); err == nil {
		t.Error("a plan with no targets is runnable")
	}
}

// TestEnrolmentKeepsTargetsItWasSeededWith, so the config.toml path still
// produces a plan that can be confirmed.
func TestEnrolmentKeepsTargetsItWasSeededWith(t *testing.T) {
	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close()

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	d := &deps{
		log:      quietLog(),
		plan:     planbus.NewBusiness(plandb.NewStore(db)),
		cfg:      Config{Targets: []string{"/home/somebody"}},
		cfgFound: true,
	}

	e := eumaeuscreds.Enrollment{
		NodeID:        "office-laptop-1",
		RepositoryURL: "s3:https://s3.example/office-laptop-1",
	}

	if err := d.storePlan(ctx, e); err != nil {
		t.Fatal(err)
	}

	plan, err := d.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Targets) != 1 || plan.Targets[0] != "/home/somebody" {
		t.Errorf("targets %v, want the ones config.toml seeded", plan.Targets)
	}

	if err := plan.Runnable(); err != nil {
		t.Errorf("a seeded plan is not runnable: %v", err)
	}
}
