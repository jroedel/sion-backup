package planbus

import (
	"fmt"
	"time"
)

// Rotation policy: when this machine should move to a fresh bucket, and how
// hard it should say so.
//
// # Why the decision is here
//
// Because Eumaeus will not make it. That server provisions storage, holds
// credentials and says what is available; it has withdrawn every field that
// tried to have an opinion about how a machine runs its own backups, and its
// own document of record says so in as many words — "when a machine moves is
// the client's decision, and the client's alone". What decides whether now is
// a good moment to spend three days of somebody's uplink is the link, the size
// of what is being moved, and whether its owner can leave the computer on.
// None of those facts exist on the other side of the API.
//
// So this file is the whole of the fleet's rotation policy, and it is worth
// being explicit that a threshold changed here changes the fleet's behaviour
// with no server change at all.
//
// # Why age is a reason at all, which it did not use to be
//
// Rotation used to be argued entirely from space: a repository that is mostly
// old versions of files costs money to keep and a fresh one would not. Age
// only qualified that argument — [MinAgeForRotation] stops the suggestion
// firing in week two — and it never drove it.
//
// It drives it now, because the other half of the credential story has been
// folded into this one operation. Eumaeus used to design a 90-day rotation of
// the two S3 access key pairs that ran independently of the bucket; that has
// been deleted, and keys now change when the bucket changes and at no other
// time. The restic password never could change without rewriting every
// snapshot, so it was already on the bucket's clock. Both are now on the same
// clock, and that clock is this file.
//
// What that means in practice: a bucket nobody rotates is a bucket whose
// password and whose two key pairs are as old as it is. The password cannot be
// revoked and a leak of it cannot be undone. Replacing the bucket is the only
// operation in this system that ends its usefulness.
//
// # And why the thresholds are a year and two years rather than ninety days
//
// Because the chore has to be affordable to the person who pays for it. A
// cutover is a seeding run: every byte this machine holds, uploaded again,
// over days, on a connection that may be someone's home broadband, by somebody
// who has to remember to leave the lid open. The failure this program is
// actually built against is not a key living too long — it is an owner
// deciding this system is a nuisance and quietly stopping. Every chore spends
// goodwill that is needed for the one message that cannot be skipped, which is
// "your computer has not backed up in three weeks".
//
// So: a year is when it becomes worth raising, two years is when it stops
// being a suggestion, and neither of them ever starts an upload on its own.
const (
	// RotateAfter is when a bucket is old enough to be worth replacing on age
	// alone, with no help from the space argument.
	RotateAfter = 365 * 24 * time.Hour

	// InsistAfter is when the page stops asking politely.
	//
	// Not an action, and deliberately not one. Everything past this threshold
	// is wording, placement and colour; the request is still a person's click
	// and the cutover is still a person's click. A client that filed its own
	// rotation request here would, on the day this shipped, put every machine
	// adopted from a legacy install into an administrator's queue at once —
	// see [Bucket.Age] for why those are all years old by construction.
	InsistAfter = 2 * 365 * 24 * time.Hour
)

// Urgency is how hard to press.
//
// Ordered, and compared with < and >= rather than switched on in most places,
// because every level does everything the level below it does and says it
// louder.
type Urgency int

// The levels.
const (
	// RotationNone is the ordinary state of a young bucket: say nothing.
	RotationNone Urgency = iota

	// RotationWorthRaising is a suggestion with both numbers beside it. A year
	// of age, or a repository that is mostly history.
	RotationWorthRaising

	// RotationOverdue is two years. The same facts, said as though they
	// mattered.
	RotationOverdue

	// RotationUrgent is a repository that failed its last integrity check.
	//
	// Above age and above space, because it is a different question. The other
	// two are about what the backup costs; this one is about whether it is
	// worth anything. A repository restic could not read back may not give the
	// files back either, and the only repair this program has is a fresh
	// bucket seeded from the files themselves — which are still on the disk,
	// and are the one copy nobody doubts.
	RotationUrgent
)

// String names the level for a log line.
func (u Urgency) String() string {
	switch u {
	case RotationWorthRaising:
		return "worth raising"
	case RotationOverdue:
		return "overdue"
	case RotationUrgent:
		return "urgent"
	default:
		return "not yet"
	}
}

// Bucket is what the server says about the repository this machine writes to.
//
// Declared here rather than taken from machinebus so that the policy does not
// depend on how the facts were obtained; the composition root converts. Every
// field is zero on a machine whose state poll has never succeeded, and the
// assessment below is written to degrade to the space-only argument there
// rather than to guess.
type Bucket struct {
	// URL is the repository these facts describe. Facts for a different URL
	// than the plan names are stale and are discarded rather than shown.
	URL string

	// CreatedAt is the history horizon, and it is what [Bucket.Age] measures
	// from. See that method for the one surprise in it.
	CreatedAt time.Time

	// State is "active" or "cutting-over", as the server spells them. Empty
	// from a deployment older than the field, which is not the same as
	// "active" — see [Bucket.CuttingOver].
	State string

	// Adopted reports a bucket taken over from a legacy install rather than
	// provisioned empty.
	Adopted bool
}

// Repository states, as the server spells them.
//
// `offered` and `retired` are deliberately absent: neither ever reaches a
// machine. An offered bucket is one this machine has not been moved to, and it
// arrives as [Offered] on the state call instead.
const (
	StateActive      = "active"
	StateCuttingOver = "cutting-over"
)

// CuttingOver reports whether this machine is being moved to a new bucket.
//
// False from a server that does not send the field, which is the right answer:
// a client that read silence as "cutting over" would offer to release an old
// bucket that nothing is moving away from.
func (b Bucket) CuttingOver() bool { return b.State == StateCuttingOver }

// Age is how long this repository has been accumulating.
//
// Zero when the server has not said, which every caller reads as "do not argue
// from age" rather than as "it is new".
//
// # The surprise, which matters on migrated machines
//
// For a bucket Eumaeus provisioned, CreatedAt is when the bucket was made and
// the age is the obvious thing. For an **adopted** bucket it is where the
// snapshots start, which is years before Eumaeus ever heard of the machine —
// so a laptop migrated off a legacy script last month reports an age of seven
// years, and lands past [InsistAfter] the first time this code runs.
//
// That is correct and it is worth saying why, because it looks like a bug. An
// adopted bucket really has been accumulating for seven years, really does
// hold seven years of superseded file versions, and its restic password really
// did come from the legacy install rather than from anything Eumaeus minted.
// Every argument for rotating applies to it more strongly than to anything
// else in the fleet, not less.
//
// What it means operationally is that adopted machines all start loud at once,
// which is the reason nothing in this file files a request or starts an upload
// by itself.
func (b Bucket) Age(now time.Time) time.Duration {
	if b.CreatedAt.IsZero() || now.Before(b.CreatedAt) {
		return 0
	}

	return now.Sub(b.CreatedAt)
}

// Offered is a bucket provisioned for this machine that it has not been moved
// to.
//
// Absent for almost every machine almost always. Nothing expires it, nothing
// happens if it is ignored, and accepting it is the one call in the API that
// starts a cutover.
type Offered struct {
	URL       string
	Provider  string
	Region    string
	Bucket    string
	OfferedAt time.Time

	// Adopted says the offered bucket already holds snapshots, which makes the
	// seeding run a very different length — and makes "move onto the archive
	// we already have" a different sentence from "move onto empty storage".
	Adopted bool
}

// Card states, as the server spells them.
const (
	// CardNever means nobody has printed a card for the bucket this machine
	// writes to now. Print one.
	//
	// This is also what a machine reads during and after a cutover: the card
	// state is a fact about the current repository, and a new bucket has had
	// no card printed for it. So the signal that a reprint is owed is this
	// word arriving while the repository URL changes, not [CardSuperseded].
	CardNever = "never"

	// CardIssued means the current bucket's card has been rendered. Which is
	// not the same as it having been printed, handed over and filed; only a
	// person can asserRt that, and the fleet pages keep it separately.
	CardIssued = "issued"

	// CardSuperseded means the owner is holding paper that no longer opens
	// anything. Print one, and destroy the old.
	//
	// It cannot arrive during a cutover, and that is the whole reason it is
	// worth telling apart from [CardNever]: until the old bucket is retired
	// its keys still work, and its card is then the only way into the only
	// bucket holding any history. So the moment this word appears it is safe
	// to tell the owner to shred what they have — which is not something
	// "never" could ever have licensed.
	CardSuperseded = "superseded"
)

// Card is what the server says about the owner's printed restore card.
type Card struct {
	// State is one of the three words above. Empty from a deployment older
	// than the field, which is read as "do not say anything about the card".
	State string

	// IssuedAt is when the CURRENT bucket's card was rendered, so it is zero
	// whenever State is [CardSuperseded].
	//
	// Not a second way to ask whether a card exists. State is the signal and
	// this is only its date: a client testing this for "has a card" would read
	// a superseded card as no card and lose the one distinction that says
	// whether the old paper is safe to destroy.
	IssuedAt time.Time
}

// Owed reports whether a card should be printed for the bucket this machine
// writes to now.
func (c Card) Owed() bool { return c.State == CardNever || c.State == CardSuperseded }

// DestroyTheOld reports whether the paper the owner is holding opens nothing
// any more.
func (c Card) DestroyTheOld() bool { return c.State == CardSuperseded }

// MachineState is the stored copy of what the server last said about this
// machine.
//
// # Why there is a stored copy at all
//
// Because the status page must not make an HTTP call to render, and because
// every part of this program that acts on a rotation has to act on the same
// answer. The daemon's poller asks, converts and writes this; pages and the
// backup path read it. One row, read and written whole, in the same key/value
// table as the measurement.
//
// # It is a record and never an authority
//
// The server is the authority on every field here, and this copy is only as
// fresh as [MachineState.AskedAt]. Nothing in this program may derive a
// permission from it: in particular it does not carry `expect_empty`, which is
// the one field in the API that lets a client create a repository, and which
// lives on the credential fetch precisely so that no client can pair it with a
// URL it read somewhere else. See credentialbus.
type MachineState struct {
	// AskedAt is when the answer was obtained. Zero means never.
	AskedAt time.Time

	// NodeID is what the fleet dashboard calls this machine.
	NodeID string

	// OwnerName and OwnerEmail are whose computer this is, for the restore
	// card and for the page.
	OwnerName  string
	OwnerEmail string

	// Bucket is the repository the machine writes to.
	Bucket Bucket

	// Offer is a bucket waiting to be accepted, or nil.
	Offer *Offered

	// Card is the state of the owner's printed card.
	Card Card

	// Error is what went wrong the last time the server was asked, in its own
	// words, or empty.
	//
	// Kept rather than dropped because the alternative is a page that shows a
	// three-week-old answer with no indication that it is three weeks old for
	// a reason. AskedAt is not updated when this is set: the age of the answer
	// and the fact that asking is currently failing are two different things
	// and a person looking at a stalled rotation needs both.
	Error string
}

// Known reports whether the server has ever answered.
func (s MachineState) Known() bool { return !s.AskedAt.IsZero() }

// Describes reports whether this answer is about the repository named.
//
// A state for a different URL is stale by definition — the plan has moved on
// and this copy has not — and is discarded rather than shown, by the same rule
// the measurement and the integrity check already follow.
func (s MachineState) Describes(repositoryURL string) bool {
	return s.Known() && s.Bucket.URL == repositoryURL
}

// Rotation thresholds for the space argument.
//
// Three conditions, all of which must hold, because each one on its own
// produces a bad suggestion:
//
//   - Age alone would nag the owner of a machine whose files never change,
//     where a year of history costs almost nothing. (Age on its own is a
//     reason in this file now, but it is [RotateAfter]'s year rather than
//     this ninety days, and it is argued from the credentials rather than
//     from the bill.)
//   - Fraction alone would fire in week two of a new repository, when a few
//     large deletions briefly make the ratio look terrible.
//   - An absolute floor keeps it quiet about savings too small to be worth a
//     conversation, let alone two days of somebody's uplink.
const (
	// MinAgeForRotation is how long a repository must have been accumulating
	// before its shape is worth arguing from.
	MinAgeForRotation = 90 * 24 * time.Hour

	// MinFractionForRotation is how much of it must be recoverable.
	MinFractionForRotation = 0.30

	// MinBytesForRotation is the floor below which it is not worth mentioning.
	MinBytesForRotation = 5 << 30 // 5 GiB
)

// Offer is the suggestion shown on the status page, or the absence of one.
//
// The wording matters more than the arithmetic here. Rotation costs the owner
// days of upload and every snapshot older than the cutover, and it saves the
// organisation money and replaces three credentials that cannot otherwise be
// replaced. Somebody being asked to trade the first for the second is entitled
// to see both in the same sentence, and to say no.
type Offer struct {
	// Show is whether to put this in front of the person at all.
	Show bool

	// Urgency is how hard to press. Never an instruction to anything: no
	// level of this starts an upload or files a request.
	Urgency Urgency

	// Reasons are the arguments that fired, in the order they are worth
	// reading. Each is one sentence, written for the owner.
	Reasons []string

	// Age is how long the repository has been accumulating, and is therefore
	// also how old its password and its two access keys are. Zero when the
	// server has not said.
	Age time.Duration

	// Reclaimable is the space a fresh repository would not need.
	Reclaimable int64

	// Fraction of the repository that is history rather than current data.
	Fraction float64

	// Discards is how much history rotating would throw away. The same
	// quantity as Age when the server has answered, and the locally known
	// history horizon otherwise.
	Discards time.Duration

	// Upload is what the seeding run would have to send.
	Upload int64
}

// Consider decides whether to raise the subject, and how loudly.
//
// A suggestion and never an action. Nothing here rotates anything: this
// machine cannot create a bucket, and the point of computing it locally is
// that the person who pays the bandwidth gets to be the one who asks.
//
// The three arguments are independent and any of them is enough. They are
// checked from weakest to strongest so that the strongest reason ends up
// setting the level, and every reason that fired is reported rather than only
// the winning one — an owner deciding whether to spend three days on this is
// owed all of it.
func Consider(now time.Time, b Bucket, m Measurement, i Integrity) Offer {
	offer := Offer{
		Reclaimable: m.Reclaimable(),
		Fraction:    m.Fraction(),
		Upload:      m.Fresh,
		Age:         b.Age(now),
	}

	// The server's horizon when there is one, and the local record otherwise.
	// They answer the same question and the server's is the better answer: on
	// an adopted bucket the local record starts when this program first wrote
	// to it, which is years after the snapshots it would be discarding.
	switch {
	case offer.Age > 0:
		offer.Discards = offer.Age
	case !m.Since.IsZero():
		offer.Discards = now.Sub(m.Since)
	}

	// The space argument. It needs a measurement, and it needs the repository
	// to have been running long enough for its shape to mean anything. An
	// unmeasured machine simply does not make this argument — the two below do
	// not need a measurement and are still allowed to fire.
	measured := !m.MeasuredAt.IsZero() && m.Now > 0

	if measured &&
		offer.Discards >= MinAgeForRotation &&
		offer.Fraction >= MinFractionForRotation &&
		offer.Reclaimable >= MinBytesForRotation {

		offer.raise(RotationWorthRaising, fmt.Sprintf(
			"%s of what this backup stores is old versions of files that have since "+
				"changed or been deleted. A fresh start would free it.",
			percent(offer.Fraction)))
	}

	// The age argument. Independent of the measurement, because it is not
	// about the bill: it is about three credentials that are exactly as old as
	// the bucket and cannot be replaced without replacing it.
	switch {
	case offer.Age >= InsistAfter:
		offer.raise(RotationOverdue, fmt.Sprintf(
			"This backup has been going to the same place for %s. Its password and its "+
				"access keys are that old too, and replacing the bucket is the only way "+
				"to replace them.", Span(offer.Age)))

	case offer.Age >= RotateAfter:
		offer.raise(RotationWorthRaising, fmt.Sprintf(
			"This backup has been going to the same place for %s. A fresh bucket every "+
				"year or so is how its password and access keys get replaced.",
			Span(offer.Age)))
	}

	// The integrity argument, which is not about cost at all.
	if i.Failed() {
		offer.raise(RotationUrgent,
			"The last check could not read this backup back in one piece. What is on "+
				"this computer is still fine; what is in the bucket may not be. A fresh "+
				"bucket, filled from the files themselves, is the repair.")
	}

	offer.Show = offer.Urgency > RotationNone

	return offer
}

// raise records a reason and lifts the level to at least the one given.
func (o *Offer) raise(level Urgency, reason string) {
	if level > o.Urgency {
		o.Urgency = level
	}

	o.Reasons = append(o.Reasons, reason)
}

// Insisting reports whether the page should stop being polite.
func (o Offer) Insisting() bool { return o.Urgency >= RotationOverdue }

// Urgent reports whether the reason is the repository itself rather than its
// age or its shape.
func (o Offer) Urgent() bool { return o.Urgency >= RotationUrgent }

// Estimate is how long the seeding run would take at a measured upload speed,
// or zero when nothing has been measured.
//
// # Why this is on the page at all
//
// Because it is the number the decision actually turns on, and it is the one
// number nobody can guess. "161 GiB" means nothing to the person being asked;
// "about four days, and the computer has to stay on and awake for them" is the
// whole question. A fleet spread across office fibre and somebody's rural
// broadband will get answers two orders of magnitude apart from the same byte
// count, which is exactly why the server is not the place this is decided.
//
// Deliberately pessimistic in its rounding, and deliberately not presented to
// the minute: an estimate given precisely is an estimate somebody will hold
// this program to.
func (o Offer) Estimate(bytesPerSecond float64) time.Duration {
	if bytesPerSecond <= 0 || o.Upload <= 0 {
		return 0
	}

	return time.Duration(float64(o.Upload)/bytesPerSecond) * time.Second
}

// MonthlySaving converts reclaimable bytes into money, or "" when no price is
// configured.
//
// Deliberately conservative and deliberately vague in its wording: storage is
// billed in ways this program does not model — per-account minimums, minimum
// retention periods — so a figure presented as exact would eventually be wrong
// in a way that costs the next number its credibility.
//
// The price lives in this machine's own config.toml, beside the thresholds it
// is compared against, and not on the other side of the API. Eumaeus used to
// serve it and withdrew it for that reason: what a gigabyte is worth to an
// owner is not a fact about a fleet.
func (o Offer) MonthlySaving(pricePerTiBMonth float64) string {
	if pricePerTiBMonth <= 0 || o.Reclaimable <= 0 {
		return ""
	}

	perMonth := float64(o.Reclaimable) / float64(1<<40) * pricePerTiBMonth

	if perMonth < 0.5 {
		return "under $1 a month"
	}

	return fmt.Sprintf("about $%.0f a month, $%.0f a year", perMonth, perMonth*12)
}

// percent renders a fraction the way the page does, so the sentence this
// package writes and the figure the template prints cannot disagree.
func percent(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// Span renders a long duration in the coarsest unit that is still true.
//
// Coarse on purpose. "2 years" is the fact; "2 years, 4 months and 9 days" is
// the same fact, harder to read, and implies a precision that a bucket's
// creation date does not carry on a machine that was migrated.
//
// Exported because the sentences written here and the figures the status page
// prints beside them have to agree, and two copies of this would eventually
// disagree by a day.
func Span(d time.Duration) string {
	days := int(d.Hours() / 24)

	switch {
	case days >= 730:
		return fmt.Sprintf("more than %d years", days/365)
	case days >= 365:
		return "more than a year"
	case days >= 60:
		return fmt.Sprintf("%d months", days/30)
	default:
		return fmt.Sprintf("%d days", days)
	}
}
