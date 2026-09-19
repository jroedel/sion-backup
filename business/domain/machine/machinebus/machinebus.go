// Package machinebus is what the server knows about this machine.
//
// One call, `GET /machines/me`, and it answers the question a machine cannot
// answer about itself: which node it is, and which repository it writes to.
// Eumaeus owns both facts. This machine is merely told them, once, at
// enrolment — and until now that was the only time it was ever told, which
// made enrolment the single point of failure for knowing its own identity.
//
// # Why this exists
//
// Because it turned out to be exactly that. Enrolment assembled the plan those
// two facts live in, found it had no folders chosen yet, and dropped it
// unwritten; the machine kept its token, forgot its repository, and told its
// owner it had never been enrolled. `enroll` then refused to run again,
// correctly, because it had. There was no way back from either side, and no
// command anywhere that could recover what the server had been holding the
// whole time.
//
// The writing bug is fixed where it was. This is the other half: a machine
// that already has a token can always ask again. Nothing here is cached and
// nothing here is authoritative locally — the server is asked, every time, and
// what it says wins.
package machinebus

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotEnrolled is a machine with no token, or one the server no longer
// recognises. Both mean the same thing to everything above: there is nobody to
// ask.
var ErrNotEnrolled = errors.New("machinebus: this machine is not enrolled")

// ErrNoRepository is a machine the server knows and has no repository for.
//
// Its own status in the API, and worth keeping as one here. An enrolled
// machine always has a repository, so this is one removed from underneath a
// machine rather than an ordinary answer — and the machine cannot repair it,
// cannot back up, and must say which of those two things is wrong rather than
// reporting a general failure to whoever reads the log.
var ErrNoRepository = errors.New("machinebus: Eumaeus has no repository for this machine")

// State is what the server says about this machine.
//
// Deliberately a small part of what the endpoint returns. Fields are added
// here when something uses them; a struct that mirrored the whole response
// would be a second, stale copy of the server's model, and the client has no
// business holding opinions about most of it.
type State struct {
	// NodeID is what the fleet dashboard calls this machine.
	NodeID string

	// RepositoryURL is where this machine writes. The server is the authority:
	// a rotation changes this, and the machine finds out by asking.
	RepositoryURL string

	// RepositoryCreatedAt is where the history starts, which on an adopted
	// bucket is years before Eumaeus ever heard of it. Zero when the server
	// said nothing.
	RepositoryCreatedAt time.Time

	// Serves is every route this deployment answers, as "METHOD /path".
	//
	// Nil from a server old enough not to send it, and that is not the same
	// as an empty list: absence means "assume nothing", never "serves
	// nothing". [State.KnowsWhatItServes] is the question to ask first.
	//
	// # Why this is asked rather than inferred
	//
	// Because inferring it from a 404 cannot work, and this client proved it
	// the expensive way. A 404 from this API means either "no such path here"
	// or something documented about this machine — /machines/me answers one
	// when the repository has been removed from underneath it — and the two
	// are identical on the wire, body included: Eumaeus's catch-all has
	// returned an Error body for unserved paths since before the endpoints
	// worth probing for existed. A client guessing from the status will one
	// day report a broken machine as an old server, which this one did, or
	// skip a feature that was there all along. See jroedel/eumaeus#144.
	//
	// Named Routes rather than Serves so that [State.Serves] can be the
	// question a caller actually asks.
	Routes []string

	// OwnerName and OwnerEmail are whose computer this is. Used on the
	// restore card, which names the person it belongs to so that a page found
	// in a filing cabinet in four years is traceable to somebody.
	OwnerName  string
	OwnerEmail string

	// RepositoryState is "active" or "cutting-over", as the server spells
	// them. Empty from a deployment older than the field.
	//
	// Descriptive and never a permission. A cutting-over bucket may be one
	// that was adopted with ten years of somebody's snapshots in it, so
	// nothing may read this as "that bucket is empty and I may create a
	// repository there" — the only field in the API that says so is
	// expect_empty, and it arrives on the credential fetch. See credentialbus.
	RepositoryState string

	// RepositoryAdopted reports a bucket taken over from a legacy install
	// rather than provisioned empty. Repeated on this call so that a machine
	// reinstalled from nothing but its token learns it without re-enrolling.
	RepositoryAdopted bool

	// Offer is a bucket provisioned for this machine that it has NOT been
	// moved to, or nil — which is the answer for almost every machine almost
	// always.
	//
	// A pointer rather than a value with a zero test, because the distinction
	// carries weight: an offer is a thing an administrator did, and a client
	// that could not tell "no offer" from "an offer with empty fields" would
	// eventually accept the second.
	Offer *Offer

	// Card is what the server says about the owner's printed restore card.
	Card Card
}

// Repository states, as the server spells them.
//
// `offered` and `retired` never appear here. The first is the state of a
// bucket this machine has not been moved to, which arrives as [State.Offer] on
// the same response; the second belongs to a bucket the machine has finished
// with. The query behind this endpoint cannot select either.
const (
	StateActive      = "active"
	StateCuttingOver = "cutting-over"
)

// CuttingOver reports whether this machine is being moved to a new bucket.
//
// False from a server that does not send the field. That is the right answer
// rather than an unknown: everything gated on this is an extra step in a
// rotation, and a deployment that cannot describe a rotation is not running
// one.
func (s State) CuttingOver() bool { return s.RepositoryState == StateCuttingOver }

// Offer is storage waiting for this machine to accept it.
//
// Not an instruction. A cutover is a seeding run — everything this machine
// holds, uploaded again, usually over days — and every fact that decides when
// to spend them is on this side of the API. Nothing expires an offer and
// nothing happens if it is ignored, so a bucket sitting here for two months is
// a conversation rather than a fault.
type Offer struct {
	// URL is the repository this machine would move to. It is named back on
	// the cutover call, which refuses any URL that is not the offer currently
	// standing.
	URL string

	Provider string
	Region   string
	Bucket   string

	// OfferedAt is when the storage was created, which is when it became
	// available. A status page is entitled to say this has been sitting here
	// for two months.
	OfferedAt time.Time

	// Adopted says the offered bucket already holds snapshots. It changes the
	// length of the seeding run and it changes the sentence shown to the
	// owner, which is why the server sends it on the offer rather than only
	// after the move.
	Adopted bool
}

// Card states, as the server spells them.
const (
	// CardNever means nobody has printed a card for the bucket this machine
	// writes to now.
	//
	// This is what a machine reads immediately after a cutover, because the
	// card state is a fact about the current repository and a new bucket has
	// had no card printed for it. So the signal that a reprint is owed is this
	// word arriving while the repository URL changes — not [CardSuperseded],
	// which arrives days or weeks later when an administrator retires the old
	// bucket.
	CardNever = "never"

	// CardIssued means the current bucket's card has been rendered.
	CardIssued = "issued"

	// CardSuperseded means the owner is holding paper that no longer opens
	// anything: print a new card, and destroy the old one.
	//
	// It cannot arrive while the old bucket's keys still work, which is the
	// whole reason it is worth telling apart from [CardNever]. Until a bucket
	// is retired its card is the only way into the only bucket holding any
	// history, and telling an owner to shred that would be the worst advice
	// this program could give.
	CardSuperseded = "superseded"
)

// Card is what the server says about the owner's printed restore card.
type Card struct {
	// State is one of the three words above, or empty from a deployment older
	// than the field.
	State string

	// IssuedAt is when the CURRENT bucket's card was rendered, and is
	// therefore zero whenever State is [CardSuperseded] — the card that was
	// rendered names a bucket that has since been retired.
	//
	// Never a second way to ask whether a card exists. State is the signal and
	// this is only its date.
	IssuedAt time.Time
}

// Owed reports whether a card should be printed for the bucket this machine
// writes to now.
func (c Card) Owed() bool { return c.State == CardNever || c.State == CardSuperseded }

// DestroyTheOld reports whether the paper the owner is holding opens nothing
// any more.
//
// Only [CardSuperseded] says so. [CardNever] never does, and the difference is
// the point: during a cutover the card state falls back to "never" while the
// old bucket's keys still work, and its card is then the only way into the
// only bucket holding any history.
func (c Card) DestroyTheOld() bool { return c.State == CardSuperseded }

// KnowsWhatItServes reports whether the server said what it implements.
//
// False means a deployment older than the field, and the honest reading is
// "assume nothing" — fall back to whatever the client did before it could ask,
// rather than treating silence as a list with nothing in it.
func (s State) KnowsWhatItServes() bool { return s.Routes != nil }

// Serves reports whether this deployment answers one route.
//
// The method is part of the question and is load-bearing: the server matches
// method and path together, so a GET to a POST-only path falls through to the
// catch-all as a 404 rather than a 405, and a client matching on the path
// alone would believe it could call it.
//
// A server that did not say returns false for everything, which is why callers
// must ask [State.KnowsWhatItServes] first rather than reading a false here as
// "not served".
func (s State) Serves(method, path string) bool {
	want := method + " " + path

	for _, route := range s.Routes {
		if route == want {
			return true
		}
	}

	return false
}

// Usable reports whether the server said enough to act on.
//
// Both fields or neither. A state with a node ID and no repository cannot
// produce a plan, and half-writing one would leave a machine that looks
// repaired and is not.
func (s State) Usable() bool { return s.NodeID != "" && s.RepositoryURL != "" }

// Business is the machine-state domain.
type Business struct {
	source Source
}

// NewBusiness constructs one. A nil source is a machine with no token.
func NewBusiness(source Source) *Business {
	return &Business{source: source}
}

// Available reports whether there is anybody to ask.
func (b *Business) Available() bool { return b != nil && b.source != nil }

// State asks the server who this machine is.
func (b *Business) State(ctx context.Context) (State, error) {
	if !b.Available() {
		return State{}, ErrNotEnrolled
	}

	state, err := b.source.State(ctx)
	if err != nil {
		return State{}, err
	}

	if !state.Usable() {
		return State{}, fmt.Errorf(
			"machinebus: the server named neither a node nor a repository for this machine")
	}

	return state, nil
}
