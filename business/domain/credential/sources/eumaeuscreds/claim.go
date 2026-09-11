package eumaeuscreds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// claimPath is relative to eumaeusapi.APIPrefix.
const claimPath = "/enrollments/claim"

// ErrCodeUsed reports an enrollment code that has already been claimed.
//
// The server refuses rather than re-issuing a token, and it is right to: a
// retry after a dropped response is indistinguishable from somebody replaying
// a code they saw. Issuing a fresh code is cheap, and whoever is enrolling the
// machine is standing in front of it.
var ErrCodeUsed = errors.New("eumaeuscreds: that enrollment code has already been used")

// ErrCodeUnknown reports a code that does not exist or has expired.
var ErrCodeUnknown = errors.New("eumaeuscreds: that enrollment code is not valid, or has expired")

// legacyRepositoryField is what Eumaeus names in the one refusal an adopting
// client has to recognise, rather than merely print. See [RepositoryMismatch].
const legacyRepositoryField = "legacy.repository_url"

// RepositoryMismatch reports a claim refused because the code enrols this
// machine against a repository it is not the one writing to.
//
// Worth telling apart from every other refusal for one reason: the code has
// NOT been consumed. Whoever is standing at the machine can have the bucket
// adopted properly and present the same code again, and a client that let this
// read like an ordinary failure would send them for a second one — or, worse,
// let them conclude the machine is fine to enrol against the new bucket.
//
// The sentence itself is the server's and names both repositories; it is
// printed as it stands.
func RepositoryMismatch(err error) bool {
	var refused *eumaeusapi.BadRequest

	return errors.As(err, &refused) && refused.Field == legacyRepositoryField
}

// Machine is what the client tells the server about itself when it enrols.
type Machine struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`

	// LocalAccount is the OS account the daemon runs as. Not trivia: it is how
	// the dashboard explains a machine that suddenly stopped working after
	// somebody was moved to a new profile.
	LocalAccount string `json:"local_account"`

	Agent string `json:"agent"`

	// Legacy is the old backup this machine is being migrated off, when there
	// is one and the client was able to look at it. See [LegacyInstall].
	Legacy *LegacyInstall `json:"legacy,omitempty"`
}

// LegacyInstall is what the machine says about the backup already running on
// it, sent with the claim.
//
// It exists for one failure nothing else catches. `provision` typed where
// `adopt` was meant returns a working, empty bucket: the claim succeeds, the
// first backup succeeds, the dashboard goes green, and years of snapshots sit
// in a bucket nothing points at. Both buckets are real, so only the machine
// that has been writing to one of them nightly can tell the difference — and
// the claim is the last moment at which saying so is free.
//
// Every field is optional and the whole block is omitted by a client that
// found nothing. The server uses it for exactly two things: refusing a claim
// whose RepositoryURL is not the repository the code enrols this machine
// against, without consuming the code, and filling in the history of a bucket
// that was adopted without one.
type LegacyInstall struct {
	// RepositoryURL is where the legacy script writes.
	//
	// Omitted rather than guessed at. A client that found an install and
	// could not read its repository says nothing here, because "I do not
	// know" must not reach the server as "somewhere else" — a machine with an
	// unreadable legacy install has to be able to enrol.
	//
	// Eumaeus compares it after trimming space and trailing slashes and
	// otherwise exactly: case is significant in a bucket name.
	RepositoryURL string `json:"repository_url,omitempty"`

	// Snapshots and OldestSnapshot are what restic reported when this client
	// opened the legacy repository. They only ever widen what the server
	// knows: a figure an administrator typed at adoption is never replaced,
	// and a provisioned repository takes neither.
	Snapshots      int       `json:"snapshots,omitempty"`
	OldestSnapshot time.Time `json:"oldest_snapshot,omitzero"`
}

// claimRequest is the wire form.
type claimRequest struct {
	Code string `json:"code"`
	Machine
}

// keyPair is one S3 access key.
type keyPair struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// claimResponse is what the server returns.
type claimResponse struct {
	MachineToken string `json:"machine_token"`
	NodeID       string `json:"node_id"`

	Owner struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"owner"`

	WarnAfterHours int `json:"warn_after_hours"`

	Repository struct {
		URL      string `json:"url"`
		Provider string `json:"provider"`
		Region   string `json:"region"`
		Bucket   string `json:"bucket"`

		// CreatedAt is when the history starts, which on an adopted
		// repository is years before the row in Eumaeus was made. See
		// docs/eumaeus-api.md §4.
		CreatedAt time.Time `json:"created_at"`

		// Adopted and Snapshots describe a bucket taken over from a legacy
		// install rather than provisioned empty. Both are omitted rather
		// than zeroed when the server has nothing to say, and their absence
		// means "ask restic" — not "there is nothing there".
		Adopted   bool `json:"adopted"`
		Snapshots int  `json:"snapshots"`
	} `json:"repository"`

	Credentials struct {
		ResticPassword string  `json:"restic_password"`
		Machine        keyPair `json:"machine"`
		Restore        keyPair `json:"restore"`
	} `json:"credentials"`
}

// Enrollment is everything one claim yields.
//
// Note what is and is not kept afterwards. The MachineToken is written to
// disk. Everything else in this struct exists only long enough to prove the
// bucket opens and to print the owner's card, and is then gone — the next
// backup fetches its own copy.
type Enrollment struct {
	MachineToken   string
	NodeID         string
	OwnerName      string
	OwnerEmail     string
	WarnAfterHours int

	RepositoryURL      string
	RepositoryProvider string
	RepositoryRegion   string
	RepositoryBucket   string

	// RepositoryCreatedAt is the history horizon: the date the owner's page
	// means by "backups available since". Zero when the server said nothing.
	RepositoryCreatedAt time.Time

	// RepositoryAdopted and RepositorySnapshots say that this bucket was
	// taken over from a legacy install, and how much was already in it.
	//
	// False and zero are not "a fresh bucket" — they are "the server did not
	// say", which is the state every machine adopted before Eumaeus grew
	// these fields is in. `adopt-enroll` treats them as a claim to check
	// rather than an answer, and asks restic either way.
	RepositoryAdopted   bool
	RepositorySnapshots int

	ResticPassword string

	// MachineKey writes to the bucket and may delete only under locks/.
	MachineKeyID     string
	MachineKeySecret string

	// RestoreKey is read-only, and is what goes on the owner's printed card.
	// A card that carried a delete-capable key would mean a lost page could
	// destroy the backup rather than merely expose it.
	RestoreKeyID     string
	RestoreKeySecret string
}

// Claim exchanges a one-time code for a machine token and the machine's
// credentials.
//
// The client must be anonymous — there is no token yet, which is the whole
// point of the call.
func Claim(ctx context.Context, client *eumaeusapi.Client, code string, m Machine) (Enrollment, error) {
	var got claimResponse

	err := client.Do(ctx, http.MethodPost, claimPath, claimRequest{Code: code, Machine: m}, &got)

	var refused *eumaeusapi.BadRequest

	switch {
	case errors.Is(err, eumaeusapi.ErrNotFound):
		return Enrollment{}, ErrCodeUnknown

	case errors.Is(err, eumaeusapi.ErrConflict):
		return Enrollment{}, ErrCodeUsed

	case errors.As(err, &refused):
		// Passed up as it stands, with no prefix of ours in front of it.
		//
		// A refused claim is the one error in this program whose reader is
		// standing at the machine rather than reading a log: they typed the
		// hostname, or failed to, and the server has told them which input to
		// look at. Eumaeus dropped its own package prefix from that sentence
		// at our asking; wrapping it here would put a prefix straight back.
		return Enrollment{}, refused

	case err != nil:
		return Enrollment{}, fmt.Errorf("eumaeuscreds: claiming the enrollment code: %w", err)
	}

	if got.MachineToken == "" || got.NodeID == "" || got.Repository.URL == "" {
		return Enrollment{}, errors.New(
			"eumaeuscreds: the server accepted the code but returned an incomplete enrollment")
	}

	if got.Credentials.ResticPassword == "" {
		// Refused loudly. A machine enrolled against a repository whose
		// password it never received would fail its first backup for a reason
		// that looks like a network problem.
		return Enrollment{}, errors.New(
			"eumaeuscreds: the server returned no repository password; " +
				"the machine's record in Eumaeus needs fixing before it can be enrolled")
	}

	return Enrollment{
		MachineToken:       got.MachineToken,
		NodeID:             got.NodeID,
		OwnerName:          got.Owner.Name,
		OwnerEmail:         got.Owner.Email,
		WarnAfterHours:     got.WarnAfterHours,
		RepositoryURL:      got.Repository.URL,
		RepositoryProvider: got.Repository.Provider,
		RepositoryRegion:   got.Repository.Region,
		RepositoryBucket:   got.Repository.Bucket,

		RepositoryCreatedAt: got.Repository.CreatedAt,
		RepositoryAdopted:   got.Repository.Adopted,
		RepositorySnapshots: got.Repository.Snapshots,
		ResticPassword:      got.Credentials.ResticPassword,
		MachineKeyID:        got.Credentials.Machine.AccessKeyID,
		MachineKeySecret:    got.Credentials.Machine.SecretAccessKey,
		RestoreKeyID:        got.Credentials.Restore.AccessKeyID,
		RestoreKeySecret:    got.Credentials.Restore.SecretAccessKey,
	}, nil
}
