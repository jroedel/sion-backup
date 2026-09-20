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
	printed := fs.Bool("printed", false,
		"record that a card has been printed and filed, without printing another")

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

	// The record on its own, for the person who printed a card and answered
	// "no" to the question below -- or printed one from the status page and
	// never pressed the button. It fetches no credentials: saying that paper
	// exists needs nothing secret.
	if *printed {
		return d.confirmCardPrinted(ctx)
	}

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

	// Printing to a terminal is not printing. This used to record the card as
	// issued the moment it reached the screen, which is the same untruth the
	// status page would have told by recording a render -- and `card_issued_at`
	// is read by people deciding whether an owner can restore without us.
	//
	// Nothing here can see a printer, so the only honest source is the person
	// looking at one. Silence is a no: run from an installer with no terminal
	// attached, this records nothing and says how to record it later.
	ok, err := confirm("Has this been printed and filed?")
	if err != nil {
		return err
	}

	if !ok {
		fmt.Printf("\nNot recorded. This machine will go on asking for a card to be printed.\n" +
			"Run \"sion-backup card --printed\" once the page is on paper and filed.\n")

		return nil
	}

	d.sayCardIssued(ctx, card.RepositoryURL)

	return nil
}

// confirmCardPrinted records a card that exists on paper, and prints nothing.
//
// The server is asked where this machine writes before the plan is, because
// the server is the authority on that and the plan's copy lags a cutover --
// and the moment an owner is told to print a card is the moment after one. A
// machine that cannot reach the server falls back to the plan, which costs
// nothing: it cannot record anything either way, and the sentence it fails
// with should be about the server rather than about a missing repository.
func (d *deps) confirmCardPrinted(ctx context.Context) error {
	var repository string

	if state, err := d.machine.State(ctx); err == nil {
		repository = state.RepositoryURL
	}

	if repository == "" {
		plan, err := d.plan.Get(ctx)
		if err != nil {
			return fmt.Errorf("reading this machine's plan: %w", err)
		}

		repository = plan.Repository
	}

	if repository == "" {
		return errors.New("this machine has no repository yet, so there is no card to record")
	}

	if err := d.recordCardIssued(ctx, repository); err != nil {
		return fmt.Errorf("telling the server that the card was printed: %w", err)
	}

	fmt.Printf("Recorded: this machine's card is printed and filed.\n")

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

	return out, nil
}

// cardPrinted records that somebody is holding paper for the repository the
// card named.
//
// The URL comes back from the page the card was shown on and is recorded as
// given, because it is a statement about a card rather than about this
// machine: "paper exists for bucket X". A machine that cut over between the
// printing and the confirming is holding a card for the bucket it has left,
// and that is still the true thing to record -- the new bucket's card state
// stays `never`, which is correct, and its banner goes on asking for the card
// nobody has printed.
//
// It deliberately does not check the URL against the plan. The plan's copy of
// the repository lags a cutover until a run reconciles it -- rotate.go logs
// that disagreement and calls it what a machine that has been moved and has
// not run since looks like -- so a card printed at the very moment somebody is
// told to print one would have been refused for naming the bucket it correctly
// names.
func (d *deps) cardPrinted(ctx context.Context, repositoryURL string) error {
	if repositoryURL == "" {
		return errors.New("this computer could not tell which repository that card was " +
			"for, so it recorded nothing. Show the card again and confirm from the " +
			"page it is on")
	}

	if err := d.recordCardIssued(ctx, repositoryURL); err != nil {
		return fmt.Errorf("the card is fine, but this computer could not tell the server "+
			"about it: %w. Nothing is wrong with the page you printed -- press this "+
			"again when the machine can reach the server", err)
	}

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
func cardOwed(state, statusPage string) (owed bool, say string) {
	switch state {
	case machinebus.CardNever:
		return true, "Nobody has printed the page that lets this computer's files be " +
			"restored without us. Print it at " + statusPage + "/card."

	case machinebus.CardSuperseded:
		return true, "The printed page this computer's owner is holding no longer opens " +
			"anything: the bucket it names has been retired. Print a new one at " +
			statusPage + "/card, and destroy the old."
	}

	return false, ""
}
