package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// server stands in for Eumaeus.
type server struct {
	state machinebus.State
	err   error
	asked int
}

func (s *server) State(context.Context) (machinebus.State, error) {
	s.asked++

	return s.state, s.err
}

// machineFor builds a deps with a real database and a stubbed server.
func machineFor(t *testing.T, src machinebus.Source) (*deps, context.Context) {
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
		log:     quietLog(),
		plan:    planbus.NewBusiness(plandb.NewStore(db)),
		machine: machinebus.NewBusiness(src),
		paths:   paths.Paths{},
	}

	return d, ctx
}

func known() machinebus.State {
	return machinebus.State{
		NodeID:        "macbook-air",
		RepositoryURL: "s3:https://s3.example/macbook-air-backup",
	}
}

// TestAMachineThatLostItsPlanGetsItBack.
//
// The repair this exists for. Before v0.6.0 enrolment could store the token
// and drop the plan, leaving a machine that told its owner it had never been
// enrolled while refusing to be enrolled again. It was holding a working token
// the whole time, and the server was holding both the facts it had lost.
func TestAMachineThatLostItsPlanGetsItBack(t *testing.T) {
	d, ctx := machineFor(t, &server{state: known()})

	d.recoverPlan(ctx)

	plan, err := d.plan.Get(ctx)
	if err != nil {
		t.Fatalf("the plan was not recovered: %v", err)
	}

	if plan.NodeID != "macbook-air" || plan.Repository != known().RepositoryURL {
		t.Errorf("recovered %+v, want what the server said", plan)
	}
}

// TestARecoveredPlanCannotRunOrBeScheduled.
//
// Why writing one unasked is safe. It has no targets, so nothing may back up
// from it, and it is unconfirmed, so the scheduler will not touch it. All it
// does is stop the machine lying about itself.
func TestARecoveredPlanCannotRunOrBeScheduled(t *testing.T) {
	d, ctx := machineFor(t, &server{state: known()})

	d.recoverPlan(ctx)

	plan, err := d.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("a recovered plan is confirmed; nobody at the machine has chosen anything")
	}

	if err := plan.Runnable(); err == nil {
		t.Error("a recovered plan is runnable; it would back up nothing and report success")
	}
}

// TestAnExistingPlanIsNeverOverwritten.
//
// Somebody may have spent ten minutes on the set-up page choosing folders, and
// the server has no opinion about those at all.
func TestAnExistingPlanIsNeverOverwritten(t *testing.T) {
	srv := &server{state: known()}
	d, ctx := machineFor(t, srv)

	mine := planbus.Plan{
		NodeID:     "chosen-by-hand",
		Repository: "s3:https://s3.example/somewhere-else",
		Targets:    []string{"/Users/domingo/Documents"},
		Schedule:   planbus.DefaultSchedule(),
	}

	if err := d.plan.Put(ctx, mine, time.Now()); err != nil {
		t.Fatal(err)
	}

	d.recoverPlan(ctx)

	plan, err := d.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.NodeID != "chosen-by-hand" || len(plan.Targets) != 1 {
		t.Errorf("the existing plan was overwritten: %+v", plan)
	}

	if srv.asked != 0 {
		t.Error("the server was asked about a machine that already knew who it was")
	}
}

// TestADeEnrolledMachineIsNotRepaired.
//
// A token the server refuses is not a plan waiting to be recovered. Writing
// one would produce a machine that looks configured and cannot fetch a
// credential.
func TestADeEnrolledMachineIsNotRepaired(t *testing.T) {
	d, ctx := machineFor(t, &server{err: machinebus.ErrNotEnrolled})

	d.recoverPlan(ctx)

	if _, err := d.plan.Get(ctx); !errors.Is(err, planbus.ErrNoPlan) {
		t.Error("a de-enrolled machine was given a plan")
	}
}

// TestAMachineWithNoRepositoryIsNotGivenOne.
//
// The server knows this machine and has no repository for it. There is nothing
// to recover and nothing this machine may invent: a repository is provisioned
// in Eumaeus, never here. Writing a plan with a made-up URL would be the worst
// answer available — it would look repaired and back up to nowhere.
func TestAMachineWithNoRepositoryIsNotGivenOne(t *testing.T) {
	d, ctx := machineFor(t, &server{err: machinebus.ErrNoRepository})

	d.recoverPlan(ctx)

	if _, err := d.plan.Get(ctx); !errors.Is(err, planbus.ErrNoPlan) {
		t.Error("a plan was invented for a machine the server has no repository for")
	}
}

// TestAnUnreachableServerIsSurvived, because every caller is a program
// starting up and a daemon that refuses to run is worse than one with nothing
// to say.
func TestAnUnreachableServerIsSurvived(t *testing.T) {
	d, ctx := machineFor(t, &server{err: errors.New("dial tcp: no route to host")})

	d.recoverPlan(ctx)

	if _, err := d.plan.Get(ctx); !errors.Is(err, planbus.ErrNoPlan) {
		t.Error("a plan was invented after a failed call")
	}
}

// TestAMachineWithNoTokenAsksNobody.
func TestAMachineWithNoTokenAsksNobody(t *testing.T) {
	d, ctx := machineFor(t, nil)

	d.recoverPlan(ctx)

	if _, err := d.plan.Get(ctx); !errors.Is(err, planbus.ErrNoPlan) {
		t.Error("an unenrolled machine was given a plan")
	}
}
