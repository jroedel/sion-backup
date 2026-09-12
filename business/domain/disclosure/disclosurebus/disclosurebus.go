// Package disclosurebus is the log of every occasion the password that opens
// this machine's backups left the server, and the machine's own check that the
// log it is shown today still contains what it contained last week.
//
// # Why a backup client renders an audit trail at all
//
// The people who own the machines in this fleet have no account on Eumaeus.
// They are rows in a fleet store, not users, and the administration pages sit
// behind a key only administrators hold. The only surface an owner actually
// has is the status page their own computer serves them on 127.0.0.1 — so if
// they are ever to see who has held the one secret that opens their data, it
// has to be here.
//
// # Why the machine keeps hashes
//
// Because a page that fetched the log and rendered it would be checking the
// server's bookkeeping against the server's bookkeeping. Every entry's hash
// covers the one before it, so removing or editing any entry changes every
// hash after it; a head hash kept on this disk from last week is therefore
// something the server cannot reach and cannot revise. A week of status page
// views leaves a week of heads here, and [Business.Read] compares the log it
// is shown against every one of them.
//
// That check is the whole of the guarantee, and it is worth being exact about
// its edges:
//
//   - It proves that entries recorded before a kept head have not been changed
//     or removed since. It proves nothing about entries this machine has never
//     seen a head for, which is why [Report] names the date it can vouch from
//     rather than saying "verified".
//
//   - It does not prove that every disclosure was recorded. Somebody who can
//     read the server's vault file directly reads the password without passing
//     through any of this and leaves no row behind. The page says so in those
//     words. A transparency claim that overstates itself spends the trust it
//     was meant to earn, and this one is worth more kept small and true.
//
// # What is not reimplemented here
//
// The hash format. Recomputing each entry's hash from its fields would mean a
// second implementation of an encoding that lives in the server, and the day
// the two disagree this machine reports tampering that did not happen — a
// false alarm in the one place where a false alarm is indistinguishable from
// the real thing. So the checks here are the ones a client can make honestly:
// that the chain it was handed links up, that the count has never fallen, and
// that the heads it wrote down are still where it left them.
package disclosurebus

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind is how the password left, which is the first thing an owner reading
// their own log wants to know.
type Kind string

const (
	// KindMinted is the genesis row: a password drawn on the server for a new
	// bucket. Nobody saw it.
	KindMinted Kind = "minted"

	// KindAdopted is the other genesis: a password that already existed,
	// typed in by a person to take over a bucket this machine was already
	// backing up to. That person held it, out of band, before the server did.
	KindAdopted Kind = "adopted"

	// KindEnrolled is the copy handed to this machine as it claimed its
	// enrollment code: the first time the password left the server.
	KindEnrolled Kind = "enrolled"

	// KindFetched is this machine collecting the password to run a backup.
	// Almost every entry, about one a day.
	KindFetched Kind = "fetched"

	// KindRevealed is a person reading it, on the record.
	KindRevealed Kind = "revealed"

	// KindExported is a person copying it somewhere else, so that losing the
	// server does not lose the only way to read these backups.
	KindExported Kind = "exported"

	// KindKeysGranted is storage credentials able to reach the bucket being
	// created.
	//
	// The three kinds below are about the BUCKET rather than the password, and
	// the server is explicit that they are not equally grave: an S3 key
	// reaches the stored files and can be revoked in a second, where the
	// password makes them readable and can never be changed. The page keeps
	// them apart for that reason and says so.
	KindKeysGranted Kind = "keys-granted"

	// KindKeysRetired is storage credentials being withdrawn. Recorded
	// because without it the log would imply an old key still works.
	KindKeysRetired Kind = "keys-retired"

	// KindKeysForeign is a credential found at the storage provider that
	// Eumaeus did not create.
	//
	// The gravest entry in the log, and the only one that is an observation
	// rather than an act: somebody other than that server can reach the
	// stored files. It cannot say who, when, or whether they ever did — the
	// provider does not record it and the server never sees a request to the
	// bucket. [Entry.Actor] is empty for exactly that reason and must not be
	// filled in by anything here.
	KindKeysForeign Kind = "keys-foreign"
)

// AboutBucket reports whether this kind concerns the storage credentials that
// reach the files rather than the password that makes them readable.
//
// Checked by kind rather than by [Entry.ByMachine], because these went to
// neither the owner's computer nor a person — they are the server acting on
// the bucket, and filing them under either of the other two headings would put
// the most serious line on the page under a sentence saying somebody read a
// password.
//
// An unrecognised kind is deliberately NOT bucket business. A kind invented
// later falls back to ByMachine, which the server sets for exactly that
// purpose, and lands somewhere defensible rather than in a section whose
// heading would be a guess.
func (k Kind) AboutBucket() bool {
	return k == KindKeysGranted || k == KindKeysRetired || k == KindKeysForeign
}

// Known reports whether this is a kind this build has heard of.
//
// An unknown kind is not an error and must never be dropped. The server is
// free to add one, and the entry still carries [Entry.Describe] — a sentence
// written on the server for exactly this reason. An old client that hid rows
// it did not recognise would hide, silently, the newest way of getting at
// somebody's password.
func (k Kind) Known() bool {
	switch k {
	case KindMinted, KindAdopted, KindEnrolled, KindFetched, KindRevealed, KindExported,
		KindKeysGranted, KindKeysRetired, KindKeysForeign:
		return true
	default:
		return false
	}
}

// Entry is one occasion on which the password left the server, or arrived at
// it. It carries no secret and never has.
type Entry struct {
	Seq                int64
	At                 time.Time
	Kind               Kind
	Actor              string
	FromIP             string
	CredentialsVersion int

	// ByMachine is whether this went to the owner's own computer rather than
	// to a person. False is the line an owner is looking for.
	//
	// Taken from the server rather than derived from Kind, so that a kind
	// added after this build still lands on the correct side of the one
	// distinction the page is built on.
	ByMachine bool

	// Describe is the sentence to show, written on the server for somebody who
	// has never heard of restic.
	Describe string

	// Detail is what the kind needs naming, and empty for the kinds that need
	// nothing: the storage access key IDs for the three keys- kinds.
	//
	// Key IDs, never secrets. One is printed on the owner's own restore card,
	// and naming it is what makes a foreign key something an administrator can
	// act on rather than merely worry about.
	Detail string

	PrevHash string
	Hash     string
}

// Witness is the head of the chain: how long it was and what the newest entry
// hashed to, at one moment.
//
// This is the thing worth keeping. It is small, it is meaningless to anybody
// who steals it, and it is the only reason the rest of this package can say
// anything the server has not simply asserted.
type Witness struct {
	Count int64
	Hash  string
	At    time.Time
}

// Zero reports whether this witness records an empty chain.
func (w Witness) Zero() bool { return w.Count == 0 && w.Hash == "" }

// Keep is a witness as this machine wrote it down, with the day it did.
//
// Seen is what the page shows. "Agrees with what this computer saw on 14
// March" is a sentence an owner can act on; "agrees with head 9f2c1a7e" is
// not.
type Keep struct {
	Witness

	// Seen is when this machine first observed this head. First rather than
	// last, deliberately: the claim being made is how far back the check
	// reaches, and that is the earliest sighting.
	Seen time.Time
}

// Log is one reading of the machine's trail.
type Log struct {
	// Count is the length of the whole chain, which is not the length of
	// Entries: that list is capped and this is not.
	Count int64

	Head    string
	Entries []Entry

	// ChainedSince is set only where the chain does not reach the first entry
	// — a server upgraded with rows already in it hashed them all in one pass,
	// and those rows attest to nothing before that moment. Zero means the
	// chain covers everything.
	ChainedSince time.Time
}

// Witness reduces a reading to the head worth keeping.
func (l Log) Witness() Witness {
	w := Witness{Count: l.Count, Hash: l.Head}

	if len(l.Entries) > 0 {
		w.At = l.Entries[0].At
	}

	return w
}

// Verdict is what this machine can honestly say about the log it was shown.
type Verdict string

const (
	// VerdictFirstLook is the first reading on this machine: there is nothing
	// written down to compare against yet. Not a failure, and not reassurance
	// either — the page says which it is.
	VerdictFirstLook Verdict = "first-look"

	// VerdictAgrees means every head this machine kept is still exactly where
	// it left it.
	VerdictAgrees Verdict = "agrees"

	// VerdictUnproven means the log read back is consistent, but the capped
	// list did not reach far enough back to test any head this machine kept.
	// Raise the limit and it becomes one of the other two.
	VerdictUnproven Verdict = "unproven"

	// VerdictShrunk means the chain is shorter than this machine has already
	// seen it be. On its own, evidence that entries were removed.
	VerdictShrunk Verdict = "shrunk"

	// VerdictRewritten means an entry this machine holds a head for now hashes
	// to something else. The entry, or one before it, was altered.
	VerdictRewritten Verdict = "rewritten"

	// VerdictBroken means the chain handed over does not link up within
	// itself: an entry's prev_hash is not the hash of the entry before it.
	VerdictBroken Verdict = "broken"
)

// Wrong reports whether this verdict is one somebody needs to be told about
// rather than merely shown.
func (v Verdict) Wrong() bool {
	return v == VerdictShrunk || v == VerdictRewritten || v == VerdictBroken
}

// Report is a reading of the log, sorted for the page and checked against what
// this machine had written down.
type Report struct {
	Log

	// People is every entry where a human being saw the password, newest
	// first. This is what the whole log is printed for.
	People []Entry

	// Machines is this computer collecting the password to run a backup.
	Machines []Entry

	// Keys is the storage credentials that reach the bucket: granted,
	// retired, and found. Separate from both of the above because they are
	// access to the files rather than to what makes the files readable, and
	// presenting the two as equally grave would be wrong in whichever
	// direction the reader resolved it.
	Keys []Entry

	// Foreign is the subset of Keys that Eumaeus did not create. Its own
	// field because it is the one thing on this page that should interrupt
	// somebody: it says a credential outside this system can reach the
	// owner's files.
	Foreign []Entry

	// PeopleCount, MachineCount and KeyCount count the returned entries, not
	// the whole chain. Where Entries is capped they are lower bounds, and
	// Capped says so.
	PeopleCount  int
	MachineCount int
	KeyCount     int

	// Capped is whether the server had more entries than it returned.
	Capped bool

	Verdict Verdict

	// Against is the oldest head this machine kept that the reading was
	// actually tested against — the date the page can vouch from. Zero unless
	// the verdict is VerdictAgrees.
	Against Keep

	// Kept is how many heads this machine is holding.
	Kept int
}

// Source reads the trail from wherever it is kept, which is the server.
type Source interface {
	List(ctx context.Context, limit int) (Log, error)
}

// Store is where this machine writes down the heads it has seen.
type Store interface {
	// Remember records a head, ignoring one already written down: re-seeing
	// the same head must not move the date the check reaches back to.
	Remember(ctx context.Context, w Witness, seen time.Time) error

	// Kept returns every head written down, oldest first.
	Kept(ctx context.Context) ([]Keep, error)
}

// ErrNotEnrolled is returned by [Business.Read] on a machine with no token,
// which has no trail to read because it has never been given a password.
var ErrNotEnrolled = errors.New("disclosurebus: this machine is not enrolled")

// ErrUnsupported is a Eumaeus that does not serve a trail yet.
//
// Separated from an ordinary failure so the page can stay silent rather than
// showing an error to somebody who cannot act on it: a fleet mid-upgrade has
// machines talking to a server that has not deployed this endpoint, and that
// is a fact about the server, not about their backups.
var ErrUnsupported = errors.New("disclosurebus: this Eumaeus serves no disclosure trail")

// Business reads the trail and keeps the receipts.
type Business struct {
	source Source
	store  Store
}

// NewBusiness constructs one. A nil source is a machine that is not enrolled.
func NewBusiness(source Source, store Store) *Business {
	return &Business{source: source, store: store}
}

// Available reports whether there is a trail to read.
func (b *Business) Available() bool { return b.source != nil && b.store != nil }

// Read fetches the trail, checks it against every head this machine has
// written down, and writes down the new one.
//
// The new head is recorded even when the verdict is bad. A machine that
// stopped keeping receipts the moment something looked wrong would lose the
// evidence of what it was shown, and the next reading would have nothing to
// compare against but the reading after that.
func (b *Business) Read(ctx context.Context, limit int) (Report, error) {
	if !b.Available() {
		return Report{}, ErrNotEnrolled
	}

	log, err := b.source.List(ctx, limit)
	if err != nil {
		return Report{}, fmt.Errorf("disclosurebus: reading the trail: %w", err)
	}

	kept, err := b.store.Kept(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("disclosurebus: reading the kept heads: %w", err)
	}

	report := split(log)
	report.Kept = len(kept)
	report.Verdict, report.Against = check(log, kept)

	if err := b.store.Remember(ctx, log.Witness(), time.Now()); err != nil {
		return Report{}, fmt.Errorf("disclosurebus: keeping the head: %w", err)
	}

	return report, nil
}

// split sorts a reading into the two columns the page is built on.
func split(log Log) Report {
	r := Report{Log: log}

	for _, e := range log.Entries {
		switch {
		case e.Kind.AboutBucket():
			r.Keys = append(r.Keys, e)

			if e.Kind == KindKeysForeign {
				r.Foreign = append(r.Foreign, e)
			}

		case e.ByMachine:
			r.Machines = append(r.Machines, e)

		default:
			r.People = append(r.People, e)
		}
	}

	r.PeopleCount, r.MachineCount, r.KeyCount = len(r.People), len(r.Machines), len(r.Keys)
	r.Capped = int64(len(log.Entries)) < log.Count

	return r
}

// check compares a reading against the heads this machine wrote down.
//
// A free function taking everything it needs, so the verdicts — which are the
// part of this package somebody's trust rests on — are testable without a
// server or a database.
func check(log Log, kept []Keep) (Verdict, Keep) {
	if v := linked(log); v != "" {
		return v, Keep{}
	}

	// Oldest first, so the first head that both agrees and could be tested is
	// the furthest back this machine can vouch from.
	var against Keep

	tested := false

	for _, k := range kept {
		if k.Zero() {
			continue
		}

		if log.Count < k.Count {
			return VerdictShrunk, Keep{}
		}

		entry, found := at(log.Entries, k.Count)
		if !found {
			// Older than the capped list reaches. Neither agreement nor
			// disagreement; asking for more entries would settle it.
			continue
		}

		if entry.Hash != k.Hash {
			return VerdictRewritten, Keep{}
		}

		if !tested {
			against, tested = k, true
		}
	}

	switch {
	case tested:
		return VerdictAgrees, against
	case len(kept) == 0:
		return VerdictFirstLook, Keep{}
	default:
		return VerdictUnproven, Keep{}
	}
}

// linked checks that the reading holds together on its own terms, returning
// the empty verdict when it does.
//
// This catches a truncated or reordered list without reference to anything
// kept, which is what makes it worth doing first: it is the one check that
// works on a machine's very first reading.
func linked(log Log) Verdict {
	if len(log.Entries) == 0 {
		// An empty list with a head is a contradiction; an empty list with no
		// head is a provisioned machine that has never enrolled, which is a
		// real and ordinary answer.
		if log.Count != 0 || log.Head != "" {
			return VerdictBroken
		}

		return ""
	}

	newest := log.Entries[0]

	if newest.Seq == log.Count && newest.Hash != log.Head {
		return VerdictBroken
	}

	for i := 0; i+1 < len(log.Entries); i++ {
		this, prev := log.Entries[i], log.Entries[i+1]

		// Only adjacent entries say anything about each other. A gap in the
		// sequence is not itself proof of tampering here — the server may cap
		// a list anywhere — but two entries that claim to be adjacent and do
		// not link is.
		if this.Seq != prev.Seq+1 {
			continue
		}

		if this.PrevHash != prev.Hash {
			return VerdictBroken
		}
	}

	return ""
}

// at finds the entry at a position in the chain.
func at(entries []Entry, seq int64) (Entry, bool) {
	for _, e := range entries {
		if e.Seq == seq {
			return e, true
		}
	}

	return Entry{}, false
}

// Glance is the one line the status page shows, built from what this machine
// has written down and nothing else.
//
// No call to the server, deliberately. The status page is the front page, it
// refreshes itself every five seconds while a backup is running, and it must
// answer "is this computer backing up" without waiting on a network round trip
// for a footnote. What it can say from the witness table alone — how many
// times the password has been handed out, and when the most recent of those
// was — is enough for a line whose job is to be a door.
type Glance struct {
	// Known is whether this machine has ever read its trail. False before the
	// first visit to the page, which is why the line it produces is an
	// invitation rather than a figure.
	Known bool

	Count int64

	// At is when the newest recorded disclosure happened, as the server dated
	// it. Zero on a chain with no entries.
	At time.Time
}

// Glance reads the newest head this machine has written down.
func (b *Business) Glance(ctx context.Context) (Glance, error) {
	if !b.Available() {
		return Glance{}, nil
	}

	kept, err := b.store.Kept(ctx)
	if err != nil {
		return Glance{}, fmt.Errorf("disclosurebus: reading the kept heads: %w", err)
	}

	if len(kept) == 0 {
		return Glance{}, nil
	}

	// Kept is oldest first, so the newest head is the last row.
	newest := kept[len(kept)-1]

	return Glance{Known: true, Count: newest.Count, At: newest.At}, nil
}
