package machinebus

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The four calls this machine makes about its own repository, and the refusals
// each of them has to be able to tell apart.
//
// # Why these live in the machine domain
//
// Because they are all `/machines/me/…` and they all act on the one answer
// [Business.State] returns. Splitting them into a rotation domain of their own
// would mean two packages decoding the same response, and the field that
// decides whether a call is even worth making — the offer — arrives on that
// response.
//
// # Every one of them is somebody's initiative and none of them is automatic
//
// Nothing in this package schedules anything. The rotation request is a person
// clicking a button on the status page, the cutover is a person choosing a
// moment to spend days of their uplink, and the release of the old bucket is
// the one call the daemon makes on its own — after a verified snapshot has
// landed in the new bucket, which is evidence rather than a schedule.

// ErrNothingOffered reports a cutover with no standing offer.
//
// The server answers 409 rather than 404 for this, deliberately: the machine
// and the endpoint both exist, and a 404 would be indistinguishable from the
// route not being served at all — which this client has misread in each
// direction once.
var ErrNothingOffered = errors.New("machinebus: no bucket has been offered to this machine")

// ErrAlreadyAsked reports a rotation request the fleet has already dealt with:
// a request is open, a rotation is under way, or a bucket is provisioned and
// waiting to be accepted.
//
// One sentinel for all three because they are one answer to the owner — the
// work an administrator could do has been done — and because the third is the
// interesting one: what is outstanding then is this machine's own decision,
// which no administrator can move.
var ErrAlreadyAsked = errors.New("machinebus: this machine has already asked, or has a bucket waiting")

// ErrNotCuttingOver reports a release of the old bucket that the server will
// not record yet.
//
// Two ordinary states, both worth retrying later. Either this machine is not
// being moved onto a new bucket, so it has none to let go of; or nothing
// verified has landed in the new bucket, so the old one is still the only
// readable copy of this machine's history. The second clears by itself when
// the seeding run finishes and restic reads it back, which is why the daemon
// treats this as a debug line rather than a failure.
var ErrNotCuttingOver = errors.New("machinebus: this machine is not cutting over, " +
	"or nothing verified has landed in the new bucket yet")

// Measured is what travels with a rotation request.
//
// The figures go with the ask so that an administrator sees what the owner was
// shown, and so that a request made against numbers that have since changed
// can be recognised as stale. The server need not trust them — it can measure
// again — but it should record them.
type Measured struct {
	RepositoryURL    string
	ReclaimableBytes int64
	FreshBytes       int64
	MeasuredAt       time.Time
}

// Released is what the server says once the old bucket has been let go.
type Released struct {
	// Bucket and URL name the bucket now waiting for a person to retire it.
	Bucket string
	URL    string

	// AskedAt is when the ask was recorded, which on a retry is the date of
	// the FIRST call and not of this one. A retry that moved it would make
	// "when did this machine say it was done" unanswerable for the person
	// about to act on it.
	AskedAt time.Time
}

// Source reads the state from wherever it lives, which is the server, and
// carries the four calls a machine makes about its own repository.
//
// One interface rather than two, because every one of these acts on the answer
// State returns and a caller holding one without the others could read an
// offer it has no way to accept.
type Source interface {
	State(ctx context.Context) (State, error)

	// RequestRotation asks an administrator for a fresh bucket. It creates a
	// work item and provisions nothing.
	RequestRotation(ctx context.Context, m Measured) error

	// Cutover accepts the standing offer, naming its URL back.
	Cutover(ctx context.Context, repositoryURL string) error

	// ReleaseOldBucket says this machine no longer needs the bucket it moved
	// off. It names the bucket the machine is on NOW.
	ReleaseOldBucket(ctx context.Context, repositoryURL string) (Released, error)

	// CardIssued records that the owner's restore card was rendered.
	CardIssued(ctx context.Context, repositoryURL string, printedAt time.Time) error
}

// RequestRotation asks for a fresh bucket.
//
// **There is no permission to check first.** Eumaeus used to answer a 403 here
// when fresh buckets were switched off fleet-wide, and that switch is gone
// along with every other field on that API which held an opinion about how a
// machine runs its own backups. A rotation request is a work item: ask, and an
// administrator either mints a bucket or does not.
//
// It provisions nothing and it starts nothing. What an administrator's
// provisioning produces is an offer, and accepting that — therefore choosing
// when days of somebody's uplink get spent — is [Business.Cutover] and is this
// machine's alone.
func (b *Business) RequestRotation(ctx context.Context, m Measured) error {
	if !b.Available() {
		return ErrNotEnrolled
	}

	if m.RepositoryURL == "" {
		return errors.New("machinebus: a rotation request must name the repository to replace")
	}

	return b.source.RequestRotation(ctx, m)
}

// Cutover accepts a bucket that has been offered, and is the only thing in
// this program that starts one.
//
// The URL is named back rather than assumed, and the server refuses a mismatch
// with a sentence naming both. The case is real: an offer made in March,
// replaced in April because the first bucket was in the wrong region, accepted
// in May by a laptop that was shut the whole time. Being refused sends this
// machine back to the state call to read the current offer, which is right;
// silently moving to the April bucket would be right by accident.
//
// Accepting twice is success, so an acknowledgement lost in transit is safe to
// retry.
//
// # What this commits the owner to
//
// A seeding run: every byte this machine holds, uploaded again, because restic
// deduplicates against the repository's index and a new repository has none.
// Days, on most connections. The old bucket stays readable throughout and
// remains the fallback until the new one has proven itself, so the risk is
// somebody's evening rather than their data — but it is still days of their
// connection, and that is why nothing in this program calls this without
// somebody having clicked.
func (b *Business) Cutover(ctx context.Context, repositoryURL string) error {
	if !b.Available() {
		return ErrNotEnrolled
	}

	if repositoryURL == "" {
		return errors.New("machinebus: a cutover must name the repository being accepted")
	}

	return b.source.Cutover(ctx, repositoryURL)
}

// ReleaseOldBucket says this machine has finished with the bucket it moved off.
//
// It releases nothing. The old bucket stays live, stays readable and keeps its
// password; this call writes one timestamp, which puts the bucket in front of
// a person who can retire it from the fleet pages. Retirement deletes the
// superseded keys and promotes the new bucket; the bucket itself is deleted by
// a person in the provider's console, because nothing in Eumaeus is allowed to
// delete storage.
//
// # Why the machine is the one that says it
//
// The server can see that a verified snapshot landed in the new bucket. It
// cannot see whether anybody has restored a file from it, or whether this
// machine is still holding something it has not sent. So the ask is this
// side's, and the server's checks are it refusing to record the ask until its
// own evidence agrees.
//
// # Name where you are now, not where you were
//
// repositoryURL is the bucket this machine is backing up to now — the
// cutting-over one — and not the one being let go. Deliberately that
// direction: a machine's memory of an earlier bucket is local state that a
// reimage takes with it while the token survives, so naming where it is now
// makes this a coherence check rather than a guess.
func (b *Business) ReleaseOldBucket(ctx context.Context, repositoryURL string) (Released, error) {
	if !b.Available() {
		return Released{}, ErrNotEnrolled
	}

	if repositoryURL == "" {
		return Released{}, errors.New(
			"machinebus: releasing the old bucket must name the bucket this machine is on now")
	}

	return b.source.ReleaseOldBucket(ctx, repositoryURL)
}

// CardIssued records that the owner's restore card was rendered.
//
// Rendered, which is not the same as printed, handed over and filed. The fleet
// keeps that second confirmation separately and only a person can assert it;
// this one exists so that a machine whose password lives in exactly one
// database is visible as such.
//
// The repository is named so that a card printed for a superseded bucket
// cannot mark the current one as covered. A 404 is that refusal and is
// returned as-is rather than swallowed: it means the card does not describe
// where this machine backs up now, which is worth saying to whoever just
// printed it.
func (b *Business) CardIssued(ctx context.Context, repositoryURL string, printedAt time.Time) error {
	if !b.Available() {
		return ErrNotEnrolled
	}

	if repositoryURL == "" {
		return errors.New("machinebus: recording a printed card must name the repository it opens")
	}

	if err := b.source.CardIssued(ctx, repositoryURL, printedAt); err != nil {
		return fmt.Errorf("machinebus: recording the printed card: %w", err)
	}

	return nil
}
