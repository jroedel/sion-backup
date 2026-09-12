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
}

// Usable reports whether the server said enough to act on.
//
// Both fields or neither. A state with a node ID and no repository cannot
// produce a plan, and half-writing one would leave a machine that looks
// repaired and is not.
func (s State) Usable() bool { return s.NodeID != "" && s.RepositoryURL != "" }

// Source reads the state from wherever it lives, which is the server.
type Source interface {
	State(ctx context.Context) (State, error)
}

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
