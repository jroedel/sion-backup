package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// recoverPlan rebuilds a plan for a machine that has a token and no plan, by
// asking the server who it is.
//
// # The state this repairs
//
// A machine that enrolled, kept its token, and lost the two facts enrolment
// gave it: its node ID and its repository. Before v0.6.0 that happened to
// every machine installed without a config.toml, because enrolment assembled
// the plan, saw it had no folders chosen yet, and returned without writing it.
//
// What the owner then saw was the set-up page — the page enrolment had just
// opened for them — saying their computer had never been enrolled, while
// `enroll` refused to run again because it had. There was no way out of that
// from either end, and the machine was holding a valid token the entire time.
//
// # Why this is safe to do unasked
//
// Because the plan it writes cannot do anything. It has no targets, so it is
// not [planbus.Plan.Runnable]; it is unconfirmed, so the scheduler will not
// touch it; and both facts in it came from the server, which is their only
// authority anyway — this machine is not deciding anything, it is remembering
// something it was told and lost.
//
// What it does is stop the machine lying about itself, which turns a dead end
// into the ordinary state of a newly enrolled computer: a set-up page asking
// somebody which folders matter.
//
// # Why failure here is never fatal
//
// Every caller is a program starting up. A server that is unreachable, a
// version too old to answer, a revoked token — none of those is a reason for a
// daemon to refuse to run, because a machine that cannot reach Eumaeus can
// still serve its status page and show what it has done in the past. The
// repair is attempted, logged either way, and abandoned quietly.
func (d *deps) recoverPlan(ctx context.Context) {
	if !d.machine.Available() {
		return
	}

	switch _, err := d.plan.Get(ctx); {
	case err == nil:
		// A plan is here. Never overwritten: the person using this machine may
		// have spent ten minutes on the set-up page choosing folders, and the
		// server has no opinion about those at all.
		return

	case !errors.Is(err, planbus.ErrNoPlan):
		d.log.Warn("could not read the backup plan", "err", err)

		return
	}

	state, err := d.machine.State(ctx)

	switch {
	case errors.Is(err, machinebus.ErrNotEnrolled):
		// The token is there and the server refuses it: this machine has been
		// de-enrolled. Not a repair to make, and the status page already has
		// something true to say about it.
		d.log.Warn("this machine has a token the server will not accept",
			"note", "it has been de-enrolled; it cannot back up until it is enrolled again")

		return

	case errors.Is(err, machinebus.ErrNoRepository):
		// The server knows this machine and has no repository for it. Nothing
		// to recover, and nothing this machine can do: a repository is
		// provisioned in Eumaeus, never here.
		d.log.Warn("Eumaeus has no repository for this machine",
			"note", "an enrolled machine always has one, so this is a repository "+
				"removed from underneath it. It cannot back up until that is fixed "+
				"on the server")

		return

	case err != nil:
		d.log.Warn("could not ask the server which repository this machine belongs to",
			"err", err, "note", "will try again at the next start")

		return
	}

	plan := planbus.Plan{
		NodeID:     state.NodeID,
		Repository: state.RepositoryURL,
		Schedule:   planbus.DefaultSchedule(),
	}

	if err := d.plan.Put(ctx, plan, time.Now()); err != nil {
		d.log.Warn("could not write the recovered plan", "err", err)

		return
	}

	d.log.Info("recovered this machine's plan from the server",
		"node", plan.NodeID, "repository", plan.Repository,
		"note", "enrolment had not stored it. Nothing is scheduled: open "+
			d.statusPage()+"/setup to choose what to back up")

	fmt.Printf("This machine was enrolled but had lost its plan. Recovered it from "+
		"Eumaeus as %s.\nNothing is backed up until somebody chooses what to back up: %s/setup\n",
		plan.NodeID, d.statusPage())
}
