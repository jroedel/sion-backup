package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

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

	if !d.creds.Enrolled() {
		return errors.New("this machine is not enrolled, so there is no card to print.\n\n" +
			"Run \"sion-backup enroll --code <code>\" first")
	}

	// The state call first, for the node and the owner — the two things on the
	// card that are not secrets and that the credential fetch does not carry.
	// A failure here is not fatal: a card with a blank owner line is still a
	// card that restores somebody's files, and the machine may be printing it
	// precisely because something is wrong.
	var state machinebus.State

	if got, err := d.machine.State(ctx); err == nil {
		state = got
	} else {
		fmt.Fprintf(os.Stderr, "Note: could not read this machine's details (%v).\n"+
			"The card below is still complete.\n", err)
	}

	set, err := d.creds.ForRun(ctx)
	if err != nil {
		return err
	}
	defer set.Wipe()

	if !set.Restore.Complete() {
		return errors.New("Eumaeus did not return a read-only key pair for this machine, " +
			"so the card cannot be printed.\n\n" +
			"This needs fixing on the server: the card must never carry the machine's own " +
			"key, which can write to the bucket")
	}

	card := restoreCard{
		NodeID:           state.NodeID,
		OwnerEmail:       state.OwnerEmail,
		RepositoryURL:    set.RepositoryURL,
		ResticPassword:   string(set.Credentials.ResticPassword),
		RestoreKeyID:     string(set.Restore.AccessKeyID),
		RestoreKeySecret: string(set.Restore.SecretAccessKey),
	}

	if card.NodeID == "" {
		// The plan's copy, which is what the status page shows. Only a label
		// on the page, so a stale one is a cosmetic problem and a blank one is
		// a page somebody cannot file.
		if plan, err := d.plan.Get(ctx); err == nil {
			card.NodeID = plan.NodeID
		}
	}

	// Parsed out of the URL rather than asked for. The bucket name is on the
	// card as the thing an administrator recognises, and the URL is the
	// authority on it: a name taken from anywhere else could name a bucket
	// this card does not open.
	if target, err := s3probe.TargetFor(set.RepositoryURL); err == nil {
		card.RepositoryBucket = target.Bucket
	}

	printRestoreCard(card)

	if state.Card.DestroyTheOld() {
		fmt.Printf("  The card this replaces opens nothing any more — the bucket it names\n" +
			"  has been retired and its keys deleted. Destroy it.\n\n")
	}

	d.sayCardIssued(ctx, set.RepositoryURL)

	return nil
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
