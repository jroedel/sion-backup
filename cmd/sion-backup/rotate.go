package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// pollEvery is how often the daemon asks the server about this machine.
//
// # Why there is a number here at all
//
// Because nothing asked. `GET /machines/me` had exactly two callers — doctor,
// and the plan repair that fires on a machine holding a token and no plan — so
// a machine backing up normally never called it, and every bucket an
// administrator provisioned was offered, correctly, to nobody.
//
// # Why six hours
//
// The cadence is this machine's to choose; Eumaeus withdrew the field that
// used to suggest one, on the grounds that a server should not have opinions
// about how somebody's laptop spends its evenings. So the number is argued
// from what is actually waiting on the other end.
//
// Nothing on that response is urgent. An offer is meant to be sat on for days
// — the document of record says a bucket waiting a year is not a failure state
// — and the card state changes when an administrator retires a bucket, which
// is a thing that happens on the scale of weeks. Against that, a laptop that
// is open for six hours a day still asks at least once most days, which is as
// often as any of this can matter.
//
// Finer would be a wake-up on a battery for an answer that is the same as the
// last one. Coarser would mean a machine that is only ever on for a morning
// could go a fortnight without noticing a bucket had been provisioned for it.
const pollEvery = 6 * time.Hour

// poller keeps the stored answer to GET /machines/me fresh.
//
// Beside the scheduler and the flusher, and like them it never fails loudly: a
// server that cannot be reached is a page that shows an older answer, not a
// machine that stops backing up.
func (d *deps) poller(ctx context.Context) {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	// One immediately, so a machine that has just been started — or has just
	// taken an update — does not wait six hours before finding out what it
	// has been offered.
	d.pollState(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pollState(ctx)
		}
	}
}

// pollState asks the server about this machine and writes down the answer.
func (d *deps) pollState(ctx context.Context) {
	if !d.machine.Available() {
		return
	}

	before, _ := d.plan.MachineState(ctx)

	state, err := d.machine.State(ctx)
	if err != nil {
		// Kept rather than dropped. A page showing a three-week-old answer
		// with no indication that asking has been failing for three weeks is
		// worse than no page: the stored answer keeps its own timestamp, and
		// this is the other half of the picture.
		if err := d.plan.RecordStateFailure(ctx, err.Error()); err != nil {
			d.log.Warn("could not record that the state call failed", "err", err)
		}

		d.log.Debug("could not ask the server about this machine", "err", err)

		return
	}

	now := time.Now()
	stored := machineStateFrom(state, now)

	if err := d.plan.RecordMachineState(ctx, stored); err != nil {
		d.log.Warn("could not store what the server said about this machine", "err", err)

		return
	}

	d.saySomethingChanged(ctx, before, stored)
}

// machineStateFrom converts the server's answer into the form that is stored.
//
// The composition root converts, so that the plan domain — which holds the
// rotation policy and the stored copy — does not depend on how the facts were
// obtained. Same rule as planbus.Size against foundation/restic's.
func machineStateFrom(s machinebus.State, now time.Time) planbus.MachineState {
	stored := planbus.MachineState{
		AskedAt:    now,
		NodeID:     s.NodeID,
		OwnerName:  s.OwnerName,
		OwnerEmail: s.OwnerEmail,
		Bucket: planbus.Bucket{
			URL:       s.RepositoryURL,
			CreatedAt: s.RepositoryCreatedAt,
			State:     s.RepositoryState,
			Adopted:   s.RepositoryAdopted,
		},
		Card: planbus.Card{State: s.Card.State, IssuedAt: s.Card.IssuedAt},
	}

	if o := s.Offer; o != nil {
		stored.Offer = &planbus.Offered{
			URL:       o.URL,
			Provider:  o.Provider,
			Region:    o.Region,
			Bucket:    o.Bucket,
			OfferedAt: o.OfferedAt,
			Adopted:   o.Adopted,
		}
	}

	return stored
}

// saySomethingChanged puts the interesting transitions in the log.
//
// Only the transitions. This runs four times a day and the answer is almost
// always the same as the last one; a line every time would be the whole of a
// quiet machine's log, and would bury the one line somebody eventually reads.
func (d *deps) saySomethingChanged(ctx context.Context, before, now planbus.MachineState) {
	if now.Offer != nil && (before.Offer == nil || before.Offer.URL != now.Offer.URL) {
		d.log.Info("a fresh bucket has been provisioned for this machine",
			"bucket", now.Offer.Bucket, "region", now.Offer.Region,
			"adopted", now.Offer.Adopted,
			"note", "nothing moves until somebody accepts it at "+d.statusPage()+"/rotation")
	}

	if now.Bucket.CuttingOver() && !before.Bucket.CuttingOver() {
		d.log.Info("this machine is being moved to a new bucket",
			"repository", now.Bucket.URL,
			"note", "the next backup fills it from scratch, and the old bucket stays readable")
	}

	if owed, say := cardOwed(now.Card.State); owed && before.Card.State != now.Card.State {
		d.log.Warn("the owner's restore card needs printing", "state", now.Card.State, "note", say)
	}

	// The plan's copy of the URL is only what the page shows — the backup path
	// reconciles it against the credential fetch, which is the atomic answer —
	// but a disagreement that lasts is worth one line, because it is what a
	// machine that has been moved and has not run since looks like.
	if plan, err := d.plan.Get(ctx); err == nil &&
		plan.Repository != "" && now.Bucket.URL != "" && plan.Repository != now.Bucket.URL {

		d.log.Info("the server names a different repository than this machine's plan does",
			"plan", plan.Repository, "server", now.Bucket.URL,
			"note", "the next run takes the server's, which is the authority")
	}
}

// openRepository is the one place outside enrolment where this program may
// create a repository, and it may do so only because the server said to.
//
// # The rule, which must not be weakened
//
// The permission is `expect_empty == true` on THIS credential fetch and
// nothing else. Not the repository state — a cutting-over bucket may be one
// that was adopted with ten years of somebody's snapshots in it. Not the local
// Seeding flag, which is computed from local history and is true both when
// this machine was deliberately moved and when its repository has vanished; a
// reimage takes the local record and leaves the token, so from here the two
// are the same picture. And never a stored copy: a cached permission outlives
// a reimage and defeats the server's own third condition, which closes the
// window the moment the seeding run lands.
//
// Absence is false, and false does exactly what this program did before the
// field existed — which is why the whole function is a no-op then. A run
// against a missing repository still fails, loudly, in restic, at the moment
// it tries to write; nothing here softens that.
//
// The server folds `!adopted` into `expect_empty` itself, so there is no
// second opinion to check on this path and none is checked. That is correct
// and it is worth saying, so that a reader does not conclude otherwise from
// the absence of a test for it.
//
// # Where it sits
//
// After the URL reconciliation, because the question is about the repository
// the server has just named. After the credential fetch, because that is what
// the permission arrives on. Before the run is announced to the fleet, because
// a machine that cannot open its repository has not started a backup.
func (d *deps) openRepository(ctx context.Context, plan planbus.Plan, set credentialbus.Set) error {
	if !set.ExpectEmpty {
		return nil
	}

	d.log.Info("the server says this machine has been moved to an empty bucket",
		"repository", plan.Repository, "state", set.RepositoryState,
		"note", "this run fills it from scratch and will take much longer than usual")

	repo := set.Credentials.Repository(plan.Repository, plan.PackSizeMiB, plan.ReadConcurrency)

	created, err := d.restic.Ensure(ctx, repo, true)

	switch {
	case errors.Is(err, restic.ErrAbsent):
		// Unreachable while mayCreate is true, and kept as a distinct branch
		// so that it stays unreachable: if Ensure ever grows a path that
		// refuses with this while permitted, a machine must not read it as an
		// ordinary failure to reach the bucket.
		return fmt.Errorf("the server permitted this machine to create the repository at %s "+
			"and it was not created: %w", plan.Repository, err)

	case err != nil:
		return err
	}

	if created {
		d.log.Info("created the repository the server moved this machine to",
			"repository", plan.Repository)
	}

	return nil
}

// releaseOldBucket tells the server this machine no longer needs the bucket it
// moved off.
//
// # Why the machine says it and why it says it here
//
// The server can see that a verified snapshot landed in the new bucket. It
// cannot see whether anybody has restored a file from it, or whether this
// machine is still holding something it has not sent. So the ask is this
// side's — and the evidence it rests on is a run that finished, verified, and
// has just been reported.
//
// It releases nothing. The old bucket stays live, stays readable and keeps its
// password; this writes one timestamp that puts it in front of a person who
// can retire it.
//
// Every refusal here is ordinary and every one of them clears by itself, which
// is why nothing about this is loud and why it is retried after every verified
// run rather than being remembered as done.
func (d *deps) releaseOldBucket(ctx context.Context, repositoryURL string) {
	// The bucket this machine is on NOW, which is the cutting-over one. Named
	// that way round deliberately: a machine's memory of an earlier bucket is
	// local state that a reimage takes with it while the token survives, so
	// naming where it is now makes this a coherence check rather than a guess.
	released, err := d.machine.ReleaseOldBucket(ctx, repositoryURL)

	switch {
	case errors.Is(err, machinebus.ErrNotCuttingOver):
		d.log.Debug("not letting go of the old bucket yet",
			"note", "either this machine is not being moved, or nothing verified has landed")

		return

	case err != nil:
		d.log.Debug("could not say this machine is finished with the old bucket", "err", err)

		return
	}

	d.log.Info("said this machine no longer needs the bucket it moved off",
		"bucket", released.Bucket, "asked_at", released.AskedAt,
		"note", "the old bucket stays readable until a person retires it")
}
