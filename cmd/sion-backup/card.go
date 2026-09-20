package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/foundation/s3probe"
)

// cardCmd prints the owner's restore card again.
//
// # Why this exists
//
// Because until now there was exactly one moment in a machine's life at which
// the card could be printed, and that moment was enrolment. Everything a card
// carries — the repository URL, the read-only key pair, the restic password —
// changes when the bucket is rotated, so after a cutover the page in
// somebody's filing cabinet opens a bucket that is on its way to being
// deleted, and there was no way to produce the new one short of re-enrolling
// the machine.
//
// # What it costs, and why that is the right cost
//
// One audited credential fetch, which is the same read a backup makes. The
// card is assembled here and never stored: the password and the read-only key
// live in this process for as long as it takes to write them to the terminal,
// exactly as they do during a run.
func cardCmd(args []string) error {
	fs := flag.NewFlagSet("card", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	d, err := wire(ctx, *verbose)
	if err != nil {
		return err
	}
	defer d.close()

	built, err := d.buildCard(ctx)
	if err != nil {
		return err
	}

	// Not fatal, and said out loud. A card with a blank owner line is still a
	// card that restores somebody's files, and the machine may be printing it
	// precisely because something is wrong.
	if built.stateErr != nil {
		fmt.Fprintf(os.Stderr, "Note: could not read this machine's details (%v).\n"+
			"The card below is still complete.\n", built.stateErr)
	}

	card, state := built.card, built.state

	printRestoreCard(card)

	if state.Card.DestroyTheOld() {
		fmt.Printf("  The card this replaces opens nothing any more — the bucket it names\n" +
			"  has been retired and its keys deleted. Destroy it.\n\n")
	}

	d.sayCardIssued(ctx, card.RepositoryURL)

	return nil
}

// builtCard is an assembled card and what the machine knew while building it.
type builtCard struct {
	card  restoreCard
	state machinebus.State

	// stateErr is a failed read of the machine's details, which is not a
	// failed card: the owner line and the node name come from there and the
	// four values that do the restoring do not.
	stateErr error
}

// buildCard assembles the owner's card.
//
// Shared by the command and by the button on the status page, because a card
// printed at a terminal and a card printed from the web page have to be the
// same card. Two assemblies would be two chances to disagree about which
// bucket the keys open, and the disagreement would surface years later in
// front of somebody who has lost a laptop.
//
// It costs one audited credential fetch, which is the same read a backup
// makes. The secrets are copied into the card and the set is wiped here: what
// leaves this function is strings, and nothing holds the live credential.
func (d *deps) buildCard(ctx context.Context) (builtCard, error) {
	if !d.creds.Enrolled() {
		return builtCard{}, errors.New("this machine is not enrolled, so there is no card to print.\n\n" +
			"Run \"sion-backup enroll --code <code>\" first")
	}

	var out builtCard

	// The state call first, for the node and the owner — the two things on the
	// card that are not secrets and that the credential fetch does not carry.
	if got, err := d.machine.State(ctx); err == nil {
		out.state = got
	} else {
		out.stateErr = err
	}

	set, err := d.creds.ForRun(ctx)
	if err != nil {
		return builtCard{}, err
	}
	defer set.Wipe()

	if !set.Restore.Complete() {
		return builtCard{}, errors.New("Eumaeus did not return a read-only key pair for this machine, " +
			"so the card cannot be printed.\n\n" +
			"This needs fixing on the server: the card must never carry the machine's own " +
			"key, which can write to the bucket")
	}

	out.card = restoreCard{
		NodeID:           out.state.NodeID,
		OwnerEmail:       out.state.OwnerEmail,
		RepositoryURL:    set.RepositoryURL,
		ResticPassword:   string(set.Credentials.ResticPassword),
		RestoreKeyID:     string(set.Restore.AccessKeyID),
		RestoreKeySecret: string(set.Restore.SecretAccessKey),
	}

	if out.card.NodeID == "" {
		// The plan's copy, which is what the status page shows. Only a label
		// on the page, so a stale one is a cosmetic problem and a blank one is
		// a page somebody cannot file.
		if plan, err := d.plan.Get(ctx); err == nil {
			out.card.NodeID = plan.NodeID
		}
	}

	// Parsed out of the URL rather than asked for. The bucket name is on the
	// card as the thing an administrator recognises, and the URL is the
	// authority on it: a name taken from anywhere else could name a bucket
	// this card does not open.
	if target, err := s3probe.TargetFor(set.RepositoryURL); err == nil {
		out.card.RepositoryBucket = target.Bucket
	}

	return out, nil
}

// issueCard is the same card, for the button on the status page.
//
// The page renders what this returns and keeps none of it. Everything that
// touches a credential happens here, in the composition root, exactly as it
// does for a run — see statusapp.Config.IssueCard.
func (d *deps) issueCard(ctx context.Context) (statusapp.RestoreCard, error) {
	built, err := d.buildCard(ctx)
	if err != nil {
		return statusapp.RestoreCard{}, err
	}

	if built.stateErr != nil {
		// Logged rather than shown. The two fields it costs are a label and an
		// email address; the card restores files without either, and a warning
		// about them on the page somebody is about to print is noise at the
		// worst moment to be adding any.
		d.log.Warn("could not read this machine's details for the card", "err", built.stateErr)
	}

	out := statusapp.RestoreCard{
		NodeID:           built.card.NodeID,
		OwnerEmail:       built.card.OwnerEmail,
		RepositoryURL:    built.card.RepositoryURL,
		RepositoryBucket: built.card.RepositoryBucket,
		ResticPassword:   built.card.ResticPassword,
		RestoreKeyID:     built.card.RestoreKeyID,
		RestoreKeySecret: built.card.RestoreKeySecret,
		DestroyOld:       built.state.Card.DestroyTheOld(),
	}

	// Said on the page rather than returned as an error. The card on the
	// screen is complete and correct; what is missing is the fleet's record
	// that one was printed, which costs a banner that keeps asking.
	if err := d.recordCardIssued(ctx, built.card.RepositoryURL); err != nil {
		d.log.Warn("the card was printed but the fleet was not told", "err", err)

		out.Unrecorded = "This computer could not tell the server that a card was printed: " +
			err.Error() + "."
	}

	return out, nil
}

// cardOwed reports whether the server says a card should be printed, and what
// to say about it.
//
// The signal is the card state and never its date. `never` is "nobody has
// printed a card for the bucket this machine writes to now", which is what a
// machine reads immediately after a cutover — the state is a fact about the
// current repository and a new bucket has had no card printed for it.
// `superseded` arrives later, when an administrator retires the old bucket,
// and is the only moment at which it is safe to tell an owner to destroy the
// paper they are holding.
func cardOwed(state string) (owed bool, say string) {
	switch state {
	case machinebus.CardNever:
		return true, "Nobody has printed the page that lets this computer's files be " +
			"restored without us. Run \"sion-backup card\"."

	case machinebus.CardSuperseded:
		return true, "The printed page this computer's owner is holding no longer opens " +
			"anything: the bucket it names has been retired. Run \"sion-backup card\" " +
			"for a new one, and destroy the old."
	}

	return false, ""
}
