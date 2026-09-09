// Package credentialbus obtains the three secrets a backup needs, uses them,
// and throws them away.
//
// # Nothing is cached
//
// The S3 access key pair and the restic repository password are fetched from
// Eumaeus at the start of every run and wiped when it ends. They are never
// written to this machine's disk.
//
// The earlier design cached them in the platform keyring so a laptop could
// back up while offline. That capability was never real: a backup writes to a
// bucket over the internet, so a machine that cannot reach Eumaeus almost
// certainly cannot reach S3 either. What the cache actually bought was three
// credentials sitting on the most-likely-to-be-stolen computer in the fleet,
// one of which — the repository password — cannot be rotated, ever.
//
// Removing it also removed the most platform-specific code in the repository:
// a 900-line wrapper around DPAPI, the macOS Keychain and the Secret Service,
// with a file fallback for the headless Linux case where none of them answers.
//
// # What is left on the machine, and why that is different
//
// One machine token, in a 0600 file — see foundation/token, which is honest
// about the fact that this is not "no secrets on the client". Somebody who
// takes that file can fetch what the machine can fetch. The difference is that
// the token is revocable in one click, its every use is audited server-side,
// and the restic password never touches the disk at all.
//
// # The cost, stated plainly
//
// Eumaeus is now on the critical path of every backup. If it is down, nothing
// in the fleet backs up. That is a real dependency and it is the reason the
// staleness alerting exists — a fleet-wide outage shows up as every machine
// going overdue at once, which is a very legible failure.
package credentialbus

import (
	"context"
	"errors"
	"fmt"

	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/token"
)

// ErrNotEnrolled reports that this machine has no usable identity — no token,
// or one Eumaeus no longer recognises.
var ErrNotEnrolled = errors.New("credentialbus: this machine is not enrolled")

// Credentials are the three secrets, together, in memory only.
//
// They are []byte rather than string so they can be overwritten after use. A
// string cannot be, which means a repository password read into one stays in
// the heap until the process exits.
type Credentials struct {
	AccessKeyID     []byte
	SecretAccessKey []byte
	ResticPassword  []byte
}

// Wipe overwrites all three. Everything that obtains a Credentials defers this.
func (c Credentials) Wipe() {
	token.Wipe(c.AccessKeyID)
	token.Wipe(c.SecretAccessKey)
	token.Wipe(c.ResticPassword)
}

// Complete reports whether all three are present.
func (c Credentials) Complete() bool {
	return len(c.AccessKeyID) > 0 && len(c.SecretAccessKey) > 0 && len(c.ResticPassword) > 0
}

// Repository assembles what foundation/restic needs.
//
// It lives here rather than in the plan domain because this is the package
// that holds the secrets, and the fewer places a Credentials value is copied
// into, the fewer places have to remember to wipe it.
func (c Credentials) Repository(url string, packSizeMiB, readConcurrency int) restic.Repository {
	return restic.Repository{
		URL:             url,
		Password:        c.ResticPassword,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		PackSizeMiB:     packSizeMiB,
		ReadConcurrency: readConcurrency,
	}
}

// Set is one fetch: the credentials and the repository they open.
//
// The URL travels with them deliberately. Which repository a password belongs
// to has to be one atomic answer, or a machine polling across a bucket
// rotation could pair the new password with the old bucket and write a
// snapshot nothing can read.
type Set struct {
	RepositoryURL string
	Version       int
	Credentials   Credentials
}

// Wipe clears the secrets in the set.
func (s Set) Wipe() { s.Credentials.Wipe() }

// Source is where credentials come from: Eumaeus.
//
// An interface so the run path can be tested without a server, and so the
// domain does not have to know what HTTP is.
type Source interface {
	Fetch(ctx context.Context) (Set, error)
}

// Business is the credential domain.
type Business struct {
	source Source
}

// NewBusiness constructs it. A nil source means the machine is not enrolled,
// which is a state the status page has to render rather than a failure to
// construct.
func NewBusiness(source Source) *Business {
	return &Business{source: source}
}

// Enrolled reports whether this machine has somewhere to fetch from.
func (b *Business) Enrolled() bool { return b.source != nil }

// ForRun fetches the credentials for one backup.
//
// The caller wipes the result. Every failure path here returns nothing to
// wipe, so a caller that defers unconditionally is correct.
func (b *Business) ForRun(ctx context.Context) (Set, error) {
	if b.source == nil {
		return Set{}, ErrNotEnrolled
	}

	set, err := b.source.Fetch(ctx)
	if err != nil {
		return Set{}, err
	}

	// Checked here rather than left to restic, because restic's own message
	// for an empty password is about the repository being unreadable — which
	// sends somebody looking at the bucket rather than at the server that just
	// handed over a half-populated record.
	if !set.Credentials.Complete() {
		set.Wipe()

		return Set{}, errors.New("credentialbus: Eumaeus returned an incomplete credential set; " +
			"the machine's record on the server needs fixing before it can back up")
	}

	if set.RepositoryURL == "" {
		set.Wipe()

		return Set{}, errors.New("credentialbus: Eumaeus returned credentials with no repository URL")
	}

	return set, nil
}

// Describe is what the status page shows about credential handling.
//
// A sentence rather than a health struct, because there is no longer any
// per-machine variation to report: it is the same on every platform, and the
// only interesting question is whether the server can be reached, which the
// run history already answers.
func (b *Business) Describe() string {
	if b.source == nil {
		return "This machine is not enrolled, so it has no way to obtain credentials."
	}

	return "Fetched from Eumaeus at the start of every backup and discarded when it ends. " +
		"Nothing is stored on this computer except the token that identifies it."
}

// Unauthorised marks a failure that means the machine has been de-enrolled.
//
// It exists so the daemon can tell "the server refused us" from "the network
// is down" without importing the HTTP layer: a source wraps its 401 in this,
// and the status page can say something true rather than showing a connection
// error forever.
type Unauthorised struct{ Err error }

func (u *Unauthorised) Error() string {
	return fmt.Sprintf("this machine has been de-enrolled from Eumaeus: %v", u.Err)
}

func (u *Unauthorised) Unwrap() error { return u.Err }
